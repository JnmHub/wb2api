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
export const put = <T>(path: string, body: unknown) => api<T>('PUT', path, body)

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

// ---------------------------------------------------------------- settings

export interface KeyView {
  id: string
  name: string
  key_preview: string
  realm: '' | 'intl' | 'cn'
  models: string[]
  concurrency_limit: number
  enabled: boolean
  created_at: string
  deleted_at: string
  deleted: boolean
}

export interface LimitEntry {
  global: number
  intl: number | null
  cn: number | null
}

export interface ProxySlot {
  id: string
  name: string
  url: string
  enabled: boolean
  ip: string
  country: string
  country_code: string
  ip_type: string
  isp: string
  asn: string
  probed_at: number
}

export interface CatalogStatus {
  realm: string
  source: string
  fetched_at: number
  count: number
}

export interface SettingsView {
  realm: { current: 'intl' | 'cn'; options: string[] }
  keys: KeyView[]
  auth_required: boolean
  limits: Record<string, LimitEntry>
  switches: Record<string, boolean>
  auto_switch_products: string[]
  upstream: { header_timeout_seconds: number; idle_timeout_seconds: number; device_token_set: boolean; device_token_file: string }
  prompt: { mode: 'passthrough' | 'custom' | 'append'; file: string }
  credits_refresh_hours: number
  window_probe: boolean
  quota_scan_models: string[]
  proxy_slots: ProxySlot[]
  catalog?: CatalogStatus[]
}

export interface LogEntry {
  id: number
  ts: string
  time: string
  level: 'INFO' | 'WARN' | 'ERROR'
  tag: string
  msg: string
}

export interface LogList {
  entries: LogEntry[]
  last: number
  tags: string[]
}

export interface ActivityRow {
  ts: string
  uid: string
  nickname: string
  realm: string
  task: string
  trigger: string
  ok: boolean
  message: string
}

export interface ActivityList {
  rows: ActivityRow[]
  total: number
  limit: number
  truncated: boolean
}

export interface SchedulerStatus {
  enabled: boolean
  running: boolean
  last_run_time: string
  next_run_time: string
  last_summary: string
  schedule: Record<string, number[]>
  logs: string[]
  credits: { ttl_hours: number; last_run: string; last_uid: string; last_error: string }
}

export interface ActionResult {
  ok: boolean
  msg?: string
  error?: string
  need_relogin?: boolean
  // probe results
  reply?: string
  elapsed_ms?: number
  status?: number
  restricted?: boolean
  network?: boolean
}

export interface ImportReport {
  added: string[]
  updated: string[]
  skipped: { uid: string; reason: string }[]
  invalid: { index: number; reason: string }[]
}

export interface ImportResponse {
  count: number
  dry_run: boolean
  result: ImportReport
}

export interface LoginStart {
  state: string
  authUrl: string
  realm: 'intl' | 'cn'
  platform: string
}

export interface LoginPoll {
  status: 'pending' | 'ok' | 'error' | 'expired' | 'unknown'
  message?: string
  uid?: string
  nickname?: string
}
