import { useEffect, useMemo, useRef, useState } from 'react'
import { keepPreviousData, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { useVirtualizer } from '@tanstack/react-virtual'
import { get, type Account, type AccountPage, type AccountSummary, type Status } from '../api'
import { credit, debounce, num, realmLabel, span, statusLabel, tokens, until } from '../format'
import { AccountDrawer } from './AccountDrawer'
import { BatchBar, type Selection } from './BatchBar'

export const PAGE = 100
const ROW = 46

export interface Filters {
  q: string
  realm: '' | 'intl' | 'cn'
  status: Status | ''
  sort: string
  order: 'asc' | 'desc'
}

const defaultFilters: Filters = { q: '', realm: '', status: '', sort: 'default', order: 'asc' }

export function queryString(f: Filters, extra: Record<string, string | number> = {}): string {
  const p = new URLSearchParams()
  if (f.q) p.set('q', f.q)
  if (f.realm) p.set('realm', f.realm)
  if (f.status) p.set('status', f.status)
  if (f.sort !== 'default') {
    p.set('sort', f.sort)
    p.set('order', f.order)
  }
  for (const [k, v] of Object.entries(extra)) p.set(k, String(v))
  return p.toString()
}

export function AccountsPage() {
  const qc = useQueryClient()
  const [filters, setFilters] = useState<Filters>(defaultFilters)
  const [qInput, setQInput] = useState('')
  const setQ = useDebouncedSetter((v: string) => setFilters((f) => ({ ...f, q: v })), 250)
  const [selection, setSelection] = useState<Selection>({ uids: new Set(), allMatching: false })
  const [open, setOpen] = useState<string | null>(null)

  // Any change of the filter resets the selection: it referred to other rows.
  useEffect(() => setSelection({ uids: new Set(), allMatching: false }), [filters])

  const summary = useQuery({
    queryKey: ['accounts-summary'],
    queryFn: () => get<AccountSummary>('/api/v2/accounts/summary'),
    refetchInterval: 30_000,
  })

  // ---- windowed data: only the pages that are on screen are fetched --------
  const scrollRef = useRef<HTMLDivElement>(null)
  const [total, setTotal] = useState(0)
  const [range, setRange] = useState({ first: 0, last: 0 })
  const virtualizer = useVirtualizer({
    count: total,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW,
    overscan: 12,
  })
  const items = virtualizer.getVirtualItems()
  const first = items[0]?.index ?? 0
  const last = items[items.length - 1]?.index ?? 0
  useEffect(() => {
    setRange((r) => (r.first === first && r.last === last ? r : { first, last }))
  }, [first, last])

  const pages = useMemo(() => {
    const out: number[] = []
    for (let p = Math.floor(range.first / PAGE); p <= Math.floor(range.last / PAGE); p++) out.push(p)
    return out
  }, [range])
  const results = useQueries({
    queries: pages.map((p) => ({
      queryKey: ['accounts', filters, p],
      queryFn: () => get<AccountPage>(`/api/v2/accounts?${queryString(filters, { offset: p * PAGE, limit: PAGE })}`),
      placeholderData: keepPreviousData,
      staleTime: 20_000,
    })),
  })
  const byPage = new Map<number, AccountPage>()
  results.forEach((r, i) => r.data && byPage.set(pages[i], r.data))
  useEffect(() => {
    for (const p of byPage.values()) {
      if (p.total !== total) {
        setTotal(p.total)
        break
      }
    }
  })
  const rowAt = (i: number): Account | undefined => byPage.get(Math.floor(i / PAGE))?.items[i % PAGE]

  const loadedUids = () => {
    const out: string[] = []
    for (const p of byPage.values()) for (const a of p.items) out.push(a.uid)
    return out
  }

  const toggleSort = (key: string) =>
    setFilters((f) => (f.sort === key ? { ...f, order: f.order === 'asc' ? 'desc' : 'asc' } : { ...f, sort: key, order: key === 'tokens' ? 'desc' : 'asc' }))
  const arrow = (key: string) => (filters.sort === key ? (filters.order === 'asc' ? ' ↑' : ' ↓') : '')

  const sum = summary.data
  const count = (s: Status) => sum?.by_status[s] ?? 0
  const chips: { s: Status | ''; label: string; n: number }[] = [
    { s: '', label: '全部', n: sum?.total ?? 0 },
    { s: 'ready', label: statusLabel.ready, n: count('ready') },
    { s: 'cooling', label: statusLabel.cooling, n: count('cooling') },
    { s: 'restricted', label: statusLabel.restricted, n: count('restricted') },
    { s: 'need_login', label: statusLabel.need_login, n: count('need_login') },
    { s: 'disabled', label: statusLabel.disabled, n: count('disabled') },
  ]

  const selectedCount = selection.allMatching ? total : selection.uids.size
  const allLoadedSelected = loadedUids().length > 0 && loadedUids().every((u) => selection.uids.has(u))

  return (
    <div className="page">
      <div className="toolbar">
        <input
          className="input"
          style={{ width: 260 }}
          placeholder="搜索 UID / 昵称 / 备注 / 代理槽"
          value={qInput}
          onChange={(e) => {
            setQInput(e.target.value)
            setQ(e.target.value)
          }}
          aria-label="搜索账号"
        />
        <select className="select" value={filters.realm} onChange={(e) => setFilters({ ...filters, realm: e.target.value as Filters['realm'] })} aria-label="区域">
          <option value="">全部区域</option>
          <option value="intl">{realmLabel.intl}</option>
          <option value="cn">{realmLabel.cn}</option>
        </select>
        <div className="chips" role="group" aria-label="按状态筛选">
          {chips.map((c) => (
            <button key={c.s || 'all'} className="chip" aria-pressed={filters.status === c.s} onClick={() => setFilters({ ...filters, status: c.s })}>
              {c.label}
              <b>{num(c.n)}</b>
            </button>
          ))}
        </div>
        <span className="spacer" />
        <span className="dim">
          今日 {tokens(sum?.today.tokens)} tokens · {num(sum?.today.requests)} 次请求
        </span>
        <button className="btn sm" onClick={() => qc.invalidateQueries({ queryKey: ['accounts'] })}>
          刷新
        </button>
      </div>

      {selectedCount > 0 && (
        <BatchBar
          selection={selection}
          total={total}
          filters={filters}
          onClear={() => setSelection({ uids: new Set(), allMatching: false })}
          onSelectAllMatching={() => setSelection({ uids: new Set(), allMatching: true })}
        />
      )}

      <div className="card table" role="table" aria-label="账号列表" aria-rowcount={total}>
        <div className="thead" role="row">
          <input
            type="checkbox"
            aria-label="选择已加载的账号"
            checked={allLoadedSelected && !selection.allMatching}
            onChange={(e) => setSelection({ uids: e.target.checked ? new Set(loadedUids()) : new Set(), allMatching: false })}
          />
          <button onClick={() => toggleSort('name')}>账号{arrow('name')}</button>
          <span>区域</span>
          <button onClick={() => toggleSort('status')}>状态{arrow('status')}</button>
          <span>身份</span>
          <button onClick={() => toggleSort('weight')}>权重{arrow('weight')}</button>
          <button className="num" onClick={() => toggleSort('tokens')}>
            今日{arrow('tokens')}
          </button>
          <button className="num" onClick={() => toggleSort('credits')}>
            积分{arrow('credits')}
          </button>
          <button className="num" onClick={() => toggleSort('expires')}>
            有效期{arrow('expires')}
          </button>
          <span />
        </div>
        <div className="tbody" ref={scrollRef}>
          <div style={{ height: virtualizer.getTotalSize(), position: 'relative' }}>
            {items.map((v) => {
              const a = rowAt(v.index)
              const selected = selection.allMatching || (a ? selection.uids.has(a.uid) : false)
              return (
                <div
                  key={v.key}
                  role="row"
                  className={'trow' + (selected ? ' sel' : '')}
                  style={{ transform: `translateY(${v.start}px)` }}
                  data-index={v.index}
                >
                  {a ? (
                    <Row
                      a={a}
                      selected={selected}
                      locked={selection.allMatching}
                      onToggle={() =>
                        setSelection((s) => {
                          const uids = new Set(s.uids)
                          if (uids.has(a.uid)) uids.delete(a.uid)
                          else uids.add(a.uid)
                          return { uids, allMatching: false }
                        })
                      }
                      onOpen={() => setOpen(a.uid)}
                    />
                  ) : (
                    <Skeleton />
                  )}
                </div>
              )
            })}
          </div>
          {total === 0 && !results.some((r) => r.isPending) && (
            <div className="dim" style={{ padding: 28, textAlign: 'center' }}>
              {filters.q || filters.status || filters.realm ? '没有符合条件的账号' : '还没有账号'}
            </div>
          )}
        </div>
      </div>

      {open && <AccountDrawer uid={open} onClose={() => setOpen(null)} />}
    </div>
  )
}

function Skeleton() {
  return (
    <>
      <span />
      <div className="skeleton" style={{ width: '70%' }} />
      <div className="skeleton" />
      <div className="skeleton" />
      <div className="skeleton" />
      <div className="skeleton" />
      <div className="skeleton" />
      <div className="skeleton" />
      <div className="skeleton" />
      <span />
    </>
  )
}

function Row({ a, selected, locked, onToggle, onOpen }: { a: Account; selected: boolean; locked: boolean; onToggle: () => void; onOpen: () => void }) {
  const now = Date.now() / 1000
  const wait = a.status === 'restricted' ? a.restricted_for : a.status === 'cooling' ? a.cooldown_for : undefined
  const expSoon = a.expires_at && a.expires_at - now < 3600
  return (
    <>
      <input type="checkbox" aria-label={`选择 ${a.nickname}`} checked={selected} disabled={locked} onChange={onToggle} />
      <div className="cell-main">
        <div className="name" title={a.uid}>
          {a.nickname}
        </div>
        <div className="sub mono">
          {a.short_id}
          {a.note ? ` · ${a.note}` : ''}
          {a.model_cooldowns?.length ? ` · ${a.model_cooldowns.length} 个模型受限` : ''}
        </div>
      </div>
      <span>
        <span className={'badge ' + a.realm}>{realmLabel[a.realm] ?? a.realm}</span>
      </span>
      <span title={a.refresh?.last_error}>
        <span className={'badge ' + a.status}>{statusLabel[a.status]}</span>
        {wait ? <span className="dim"> {span(wait)}</span> : null}
      </span>
      <span className="dim">{a.product === 'workbuddy' ? '桌面端' : a.product}</span>
      <span className="num">{a.weight}</span>
      <span className="num" title={`${num(a.today.requests)} 次请求`}>
        {tokens(a.today.tokens)}
      </span>
      <span className="num">{a.credits?.remain != null ? credit(a.credits.remain) : '—'}</span>
      <span className="num" style={expSoon ? { color: 'var(--warn)' } : undefined}>
        {until(a.expires_at, now)}
      </span>
      <button className="btn sm" onClick={onOpen} aria-label={`详情 ${a.nickname}`}>
        ⋯
      </button>
    </>
  )
}

function useDebouncedSetter<T>(fn: (v: T) => void, wait: number) {
  const ref = useRef(fn)
  ref.current = fn
  return useMemo(() => debounce((v: T) => ref.current(v), wait), [wait])
}
