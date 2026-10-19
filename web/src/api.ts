// Thin fetch wrapper for the dashboard API. The session lives in an HttpOnly
// cookie (JS never sees it); the CSRF token is kept in memory and re-fetched
// from /session after a page reload.

export class ApiError extends Error {
  status: number
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

let csrf = ''
export function setCsrf(v: string) {
  csrf = v
}

type Unauthorized = () => void
const listeners = new Set<Unauthorized>()
export function onUnauthorized(fn: Unauthorized) {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && csrf) headers['X-WB-CSRF'] = csrf
  const res = await fetch(path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  let data: unknown = null
  const text = await res.text()
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = null
    }
  }
  if (!res.ok) {
    const err = (data as { error?: { message?: string; code?: string } } | null)?.error
    // Only "your session is gone" signs the user out. A 401 for a wrong password
    // (code invalid_password) or the session probe itself must not.
    if (res.status === 401 && err?.code === 'login_required') listeners.forEach((fn) => fn())
    throw new ApiError(res.status, err?.message ?? `HTTP ${res.status}`, err?.code)
  }
  return data as T
}

export const get = <T>(path: string) => api<T>('GET', path)
export const post = <T>(path: string, body?: unknown) => api<T>('POST', path, body ?? {})
export const patch = <T>(path: string, body: unknown) => api<T>('PATCH', path, body)
export const del = <T>(path: string) => api<T>('DELETE', path)

// ------------------------------------------------------------------- types

export interface SessionInfo {
  authenticated: boolean
  csrf?: string
  remember?: boolean
  expires_at?: number
  using_default_password: boolean
}

export type Status = 'ready' | 'expiring' | 'cooling' | 'restricted' | 'need_login' | 'disabled'

export interface Account {
  uid: string
  short_id: string
  nickname: string
  realm: 'intl' | 'cn'
  product: string
  enterprise_id: string
  enabled: boolean
  weight: number
  note: string
  disabled_models: string[]
  source: string
  proxy_slot: string
  proxy: string
  expires_at: number
  status: Status
  need_login: boolean
  has_refresh_token: boolean
  added_at: number
  inflight: number
  today: { requests: number; tokens: number; credit: number }
  cooldown_for?: number
  restricted_for?: number
  model_cooldowns?: { model: string; expires_at: number }[]
  credits?: { remain?: number; total?: number }
  refresh?: { failures: number; next_try: number; last_error: string }
  // detail only
  machine_id?: string
  session_id?: string
  domain?: string
  stats?: { requests: number; failures: number }
  credits_raw?: unknown
  model_caps?: unknown
}

export interface AccountPage {
  items: Account[]
  total: number
  offset: number
  limit: number
}

export interface AccountSummary {
  total: number
  by_status: Partial<Record<Status, number>>
  by_realm: Record<string, Partial<Record<Status, number>>>
  today: { tokens: number; requests: number }
}

export interface Job {
  id: string
  kind: string
  state: 'running' | 'done' | 'cancelled'
  total: number
  done: number
  failed: number
  errors?: { item: string; error: string }[]
}

export interface UsageAgg {
  requests: number
  ok: number
  aborted: number
  failed: number
  prompt_tokens: number
  completion_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  total_tokens: number
  credit: number
  avg_elapsed_ms: number
  avg_ttft_ms: number
  tokens_per_sec: number
  cache_hit_pct: number
}

export interface UsageSummary {
  totals: UsageAgg
  by_model: (UsageAgg & { key: string })[]
  by_account: (UsageAgg & { key: string })[]
  by_key: (UsageAgg & { key: string })[]
  by_realm: (UsageAgg & { key: string })[]
  by_account_truncated: boolean
  queue: { written: number; dropped: number; last_error: string }
}

export interface SeriesPoint extends UsageAgg {
  bucket: string
}
