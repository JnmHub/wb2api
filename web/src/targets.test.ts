import { describe, expect, it } from 'vitest'
import { targetOf } from './accounts/BatchBar'

const filters = { q: 'nick', realm: 'cn', status: 'cooling', sort: 'default', order: 'asc' } as const

describe('targetOf', () => {
  it('targets explicit uids', () => {
    expect(targetOf({ uids: new Set(['a', 'b']), allMatching: false }, { ...filters })).toEqual({ uids: ['a', 'b'] })
  })
  it('targets everything matching the filter, not only the visible page', () => {
    expect(targetOf({ uids: new Set(), allMatching: true }, { ...filters })).toEqual({
      filter: { realm: 'cn', status: 'cooling', q: 'nick' },
    })
    expect(targetOf({ uids: new Set(), allMatching: true }, { q: '', realm: '', status: '', sort: 'default', order: 'asc' })).toEqual({ filter: {} })
  })
})
