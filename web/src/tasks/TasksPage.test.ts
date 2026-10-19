import { describe, expect, it } from 'vitest'
import { parseHours } from './TasksPage'

describe('parseHours', () => {
  it('parses the comma separated grid', () => {
    expect(parseHours('9, 21')).toEqual([9, 21])
    expect(parseHours('9，21、3')).toEqual([3, 9, 21])
  })
  it('drops out-of-range values and duplicates, and sorts', () => {
    expect(parseHours('24 9 9 -1 0 23')).toEqual([0, 9, 23])
  })
  it('returns an empty grid for junk', () => {
    expect(parseHours('')).toEqual([])
    expect(parseHours('abc')).toEqual([])
  })
})
