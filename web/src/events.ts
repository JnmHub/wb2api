import { useEffect } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import type { Account, AccountPage, Job } from './api'

export interface AccountPatch {
  accounts?: Account[]
  removed?: string[]
}

/** Merge a pushed patch into one cached page of the account list. */
export function applyPatchToPage(page: AccountPage, patch: AccountPatch): AccountPage {
  const byUid = new Map((patch.accounts ?? []).map((a) => [a.uid, a]))
  const gone = new Set(patch.removed ?? [])
  if (byUid.size === 0 && gone.size === 0) return page
  let changed = false
  const items = page.items
    .filter((a) => {
      if (gone.has(a.uid)) {
        changed = true
        return false
      }
      return true
    })
    .map((a) => {
      const n = byUid.get(a.uid)
      if (!n) return a
      changed = true
      return { ...a, ...n }
    })
  return changed ? { ...page, items, total: page.total - (page.items.length - items.length) } : page
}

export function applyPatch(qc: QueryClient, patch: AccountPatch) {
  qc.setQueriesData<AccountPage>({ queryKey: ['accounts'] }, (old) => (old ? applyPatchToPage(old, patch) : old))
  if (patch.removed?.length) qc.invalidateQueries({ queryKey: ['accounts'] })
  qc.invalidateQueries({ queryKey: ['accounts-summary'] })
}

/**
 * One server-sent-event connection for the whole app. It closes while the tab
 * is hidden (nothing to show, nothing to poll) and refetches on return, so a
 * background tab costs the server nothing.
 */
export function useEventStream() {
  const qc = useQueryClient()
  useEffect(() => {
    let es: EventSource | null = null
    let closeTimer: ReturnType<typeof setTimeout> | undefined
    const open = () => {
      if (es) return
      es = new EventSource('/api/v2/events')
      es.addEventListener('account.patch', (e) => {
        try {
          applyPatch(qc, JSON.parse((e as MessageEvent).data))
        } catch {
          /* ignore malformed frames */
        }
      })
      es.addEventListener('job.progress', (e) => {
        try {
          const j = JSON.parse((e as MessageEvent).data) as Job
          qc.setQueryData(['job', j.id], j)
          if (j.state !== 'running') {
            qc.invalidateQueries({ queryKey: ['accounts'] })
            qc.invalidateQueries({ queryKey: ['accounts-summary'] })
          }
        } catch {
          /* ignore */
        }
      })
      es.addEventListener('resync', () => {
        qc.invalidateQueries({ queryKey: ['accounts'] })
        qc.invalidateQueries({ queryKey: ['accounts-summary'] })
      })
    }
    const close = () => {
      es?.close()
      es = null
    }
    const onVis = () => {
      clearTimeout(closeTimer)
      if (document.hidden) {
        closeTimer = setTimeout(close, 15_000)
      } else if (!es) {
        open()
        qc.invalidateQueries({ queryKey: ['accounts'] })
        qc.invalidateQueries({ queryKey: ['accounts-summary'] })
      }
    }
    open()
    document.addEventListener('visibilitychange', onVis)
    return () => {
      document.removeEventListener('visibilitychange', onVis)
      clearTimeout(closeTimer)
      close()
    }
  }, [qc])
}
