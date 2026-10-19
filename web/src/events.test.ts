import { describe, expect, it } from 'vitest'
import { applyPatchToPage } from './events'
import type { Account, AccountPage } from './api'

const acc = (uid: string, over: Partial<Account> = {}): Account =>
  ({ uid, nickname: uid, weight: 1, status: 'ready', enabled: true, ...over }) as Account
const page = (...uids: string[]): AccountPage => ({ items: uids.map((u) => acc(u)), total: 50, offset: 0, limit: 100 })

describe('applyPatchToPage', () => {
  it('merges changed accounts in place, keeping order', () => {
    const p = page('a', 'b', 'c')
    const out = applyPatchToPage(p, { accounts: [acc('b', { weight: 9, status: 'cooling' })] })
    expect(out.items.map((a) => a.uid)).toEqual(['a', 'b', 'c'])
    expect(out.items[1]).toMatchObject({ weight: 9, status: 'cooling' })
    expect(out.items[0]).toBe(p.items[0]) // untouched rows keep identity, so React skips them
  })
  it('returns the same object when nothing on the page changed', () => {
    const p = page('a', 'b')
    expect(applyPatchToPage(p, { accounts: [acc('zzz')] })).toBe(p)
    expect(applyPatchToPage(p, {})).toBe(p)
  })
  it('removes deleted accounts and lowers the total', () => {
    const out = applyPatchToPage(page('a', 'b', 'c'), { removed: ['b'] })
    expect(out.items.map((a) => a.uid)).toEqual(['a', 'c'])
    expect(out.total).toBe(49)
  })
})
