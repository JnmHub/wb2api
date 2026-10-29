// Number / time formatting shared by the pages.

export function num(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n)) return '—'
  return Math.round(n).toLocaleString('en-US')
}

/** 1.2K / 3.4M / 5.6B style token counts. */
export function tokens(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n)) return '—'
  const a = Math.abs(n)
  if (a >= 1e9) return (n / 1e9).toFixed(a >= 1e10 ? 1 : 2) + 'B'
  if (a >= 1e6) return (n / 1e6).toFixed(a >= 1e7 ? 1 : 2) + 'M'
  if (a >= 1e4) return (n / 1e3).toFixed(1) + 'K'
  return String(Math.round(n))
}

export function pct(n: number | undefined | null, digits = 1): string {
  if (n == null || Number.isNaN(n)) return '—'
  return n.toFixed(digits) + '%'
}

export function ms(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n) || n === 0) return '—'
  return n >= 1000 ? (n / 1000).toFixed(2) + ' s' : Math.round(n) + ' ms'
}

export function credit(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n)) return '—'
  return n >= 100 ? n.toFixed(0) : n.toFixed(2)
}

/** "3h 12m" / "5d" until a unix-seconds deadline, or "expired". */
export function until(sec: number | undefined, now = Date.now() / 1000): string {
  if (!sec) return '—'
  const d = sec - now
  if (d <= 0) return '已过期'
  return span(d)
}

/** Seconds as a short human span: 45s, 12m, 3h 5m, 2d 4h. */
export function span(sec: number): string {
  const s = Math.max(0, Math.round(sec))
  if (s < 60) return s + 's'
  const m = Math.floor(s / 60)
  if (m < 60) return m + 'm'
  const h = Math.floor(m / 60)
  if (h < 24) return h + 'h' + (m % 60 ? ' ' + (m % 60) + 'm' : '')
  const d = Math.floor(h / 24)
  return d + 'd' + (h % 24 ? ' ' + (h % 24) + 'h' : '')
}

export const statusLabel: Record<string, string> = {
  ready: '可用',
  expiring: '续期中',
  cooling: '冷却中',
  restricted: '风控限制',
  need_login: '需重新登录',
  disabled: '已停用',
  no_credit: '无积分',
  model_cooling: '模型冷却',
}

export const realmLabel: Record<string, string> = { intl: '国际版', cn: '国内版' }

export function debounce<A extends unknown[]>(fn: (...a: A) => void, wait: number) {
  let t: ReturnType<typeof setTimeout> | undefined
  const d = (...a: A) => {
    clearTimeout(t)
    t = setTimeout(() => fn(...a), wait)
  }
  d.cancel = () => clearTimeout(t)
  return d
}
