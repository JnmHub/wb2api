import { describe, expect, it } from 'vitest'
import { mergeLogs } from './LogsPage'
import type { LogEntry } from '../api'

const line = (id: number): LogEntry => ({ id, ts: '', time: '00:00:00', level: 'INFO', tag: 'chat', msg: 'm' + id })

describe('mergeLogs', () => {
  it('keeps the same array when a poll brought nothing new', () => {
    const kept = [line(1), line(2)]
    expect(mergeLogs(kept, [])).toBe(kept)
    expect(mergeLogs(kept, [line(1), line(2)])).toBe(kept)
  })

  it('appends only ids after the last kept line, in order', () => {
    const out = mergeLogs([line(1), line(2)], [line(2), line(3), line(4)])
    expect(out.map((e) => e.id)).toEqual([1, 2, 3, 4])
  })

  it('is bounded: old lines fall off the front', () => {
    const kept = Array.from({ length: 2000 }, (_, i) => line(i + 1))
    const out = mergeLogs(kept, [line(2001), line(2002)])
    expect(out).toHaveLength(2000)
    expect(out[0].id).toBe(3)
    expect(out[out.length - 1].id).toBe(2002)
  })
})
