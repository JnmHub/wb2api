import { describe, expect, it } from 'vitest'
import { credit, ms, num, pct, span, tokens, until } from './format'

describe('format', () => {
  it('abbreviates token counts', () => {
    expect(tokens(950)).toBe('950')
    expect(tokens(12_345)).toBe('12.3K')
    expect(tokens(1_234_567)).toBe('1.23M')
    expect(tokens(64_000)).toBe('64.0K')
    expect(tokens(2_500_000_000)).toBe('2.50B')
    expect(tokens(undefined)).toBe('—')
  })
  it('formats spans', () => {
    expect(span(45)).toBe('45s')
    expect(span(600)).toBe('10m')
    expect(span(3 * 3600 + 5 * 60)).toBe('3h 5m')
    expect(span(2 * 86400 + 4 * 3600)).toBe('2d 4h')
    expect(span(-5)).toBe('0s')
  })
  it('formats token lifetimes against a clock', () => {
    expect(until(0)).toBe('—')
    expect(until(1000, 2000)).toBe('已过期')
    expect(until(1000 + 3 * 3600, 1000)).toBe('3h')
  })
  it('formats numbers, percentages, durations and credits', () => {
    expect(num(1234567)).toBe('1,234,567')
    expect(pct(12.345)).toBe('12.3%')
    expect(pct(null)).toBe('—')
    expect(ms(0)).toBe('—')
    expect(ms(850)).toBe('850 ms')
    expect(ms(2500)).toBe('2.50 s')
    expect(credit(1.5)).toBe('1.50')
    expect(credit(250.4)).toBe('250')
  })
})
