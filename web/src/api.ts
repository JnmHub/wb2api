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

export type Status = 'ready' | 'expiring' | 'cooling' | 'restricted' | 'need_login' | 'disabled' | 'no_credit'

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
  unavailable_reason?: string
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
  model_cooling?: number
  cooling_total?: number
  today: { tokens: number; requests: number; credit?: number }
  credits?: {
    remain: number
    available: number
    expiring_7d: number
    today_used: number
    known: number
    zero: number
  }
  by_realm_credits?: Record<
    string,
    { remain: number; available: number; expiring_7d: number; total: number; ready: number; zero: number; keys: number }
  >
  keys?: number
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

export interface UsageAgg {  requests: number
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

export interface UsageRequest {
  id: number
  at: number
  model: string
  account: string
  realm: string
  key: string
  stream: boolean
  outcome: string
  status: number
  elapsed_ms: number
  ttft_ms?: number
  gen_ms?: number
  held_ms: number
  prompt_tokens: number
  completion_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  total_tokens: number
  credit: number
  reasoning_effort: string
  usage_estimated: boolean
  error: string
  tokens_per_sec: number
}

export interface UsageRequestPage {
  requests: UsageRequest[]
  next_before: number
}

export interface CatalogModel {
  id: string
  name?: string
  description?: string
  context_length?: number
  context_windows?: number[]
  max_output_tokens?: number
  reasoning_efforts?: string[] | Record<string, string>
  supports_vision?: boolean
  supports_tool_calls?: boolean
  supports_reasoning?: boolean
  vision_note?: string
}

export interface ModelCatalogue {
  realm: string
  source: string
  fetched_at: number
  models: CatalogModel[]
}

// ---------------------------------------------------------------- settings

export interface KeyView {
  id: string
  name: string
  key_preview: string
  realm: '' | 'intl' | 'cn'
  models: string[]
  concurrency_limit: number
  tps_limit: number
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
  banned_models: string[]
  banned_substring: string
  daily_chat_web_attempts: number
  daily_chat_web_timeout: number
  ops_concurrency: number
  ops_interval_ms: number
  output_tps_limit: number
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

export interface GrowthTask {
  code: string
  title: string
  description: string
  jump_url: string
  status: string
  type: string
  current: number
  target: number
  reward_credit: number
  reward_energy: number
}

export interface GrowthSummary {
  energy: number
  streak_days: number
  travel: { state: string; daily_limit_reached?: boolean }
}

export interface TasksSnapshot {
  account: { uid: string; nickname: string; realm: string; enterprise: boolean }
  accounts: { uid: string; nickname: string; realm: string; enterprise: boolean }[]
  tasks: GrowthTask[]
  summary: GrowthSummary
}

export interface GrowthRun {
  ok: boolean
  credit_added?: number
  logs?: string[]
  msg?: string
  accounts_count?: number
}

export interface RemainingRow {
  uid: string
  nickname: string
  realm: string
  model: string
  used: number
  budget: number | null
  remaining: number | null
  used_pct: number | null
  reset_at?: number
  cooling: boolean
  estimated: boolean
}

export interface RemainingModel {
  realm: string
  model: string
  budget: number | null
  total_remaining: number | null
  pairs: number
  cooling: number
  unknown: number
}

export interface RemainingPayload {
  window_seconds: number
  generated_at: number
  rows: RemainingRow[]
  models: RemainingModel[]
  etag: string
}

export interface UpdateStatus {
  current_version: string
  latest_version: string | null
  latest_tag: string
  update_available: boolean
  enabled: boolean
  checking: boolean
  last_attempt: string
  last_success: boolean
  last_error: string
  release_url: string
  repo_url: string
  published_at: string
  skipped?: string
}

export interface DesktopFile {
  path: string
  file: string
  realm: string
  realm_name: string
  domain: string
  uid: string
  nickname: string
  expires_at: number
  readable: boolean
  valid: boolean
  encrypted: boolean
  needs_key: boolean
  error: string
}

export interface DesktopStatus {
  platform: string
  hunt_supported: boolean
  hunt_hint: string
  keyblob: string
  keyblob_found: boolean
  key_id: string
  key_cached: boolean
  key_source: string
  error: string
}

export interface DesktopResult {
  ok?: boolean
  status: DesktopStatus
  files: DesktopFile[]
  imported?: { path: string; uid?: string; nickname?: string; realm?: string; ok?: boolean; error?: string }[]
}

export interface PricingPrice {
  input: number
  output: number
  cache_read: number
  cache_write: number
}

export interface PricingModel {
  id: string
  name: string
  price: PricingPrice
}

export interface PricingPolicy {
  mode: string
  or_id?: string
  input?: number
  output?: number
  cache_read?: number
  cache_write?: number
  note?: string
}

export interface PricingStatus {
  fetched_at: string
  usd_cny: number
  source: string
  models: number
  catalogue: PricingModel[]
  policies: Record<string, PricingPolicy>
  error: string
  stale: boolean
}

export interface PricingCostRow {
  model: string
  requests: number
  prompt_tokens: number
  completion_tokens: number
  cached_tokens: number
  cost_cny: number
  cost_usd: number
  priced: boolean
  source: string
}

export interface PricingCost {
  since: string
  until: string
  models: PricingCostRow[]
  totals: { cost_cny: number; cost_usd: number; priced_models: number; unpriced_models: number }
}

export interface AgentApplied {
  at: string
  base_url: string
  model: string
  files: number
  external_change: boolean
}

export interface AgentClient {
  id: string
  label: string
  desc: string
  protocol: string
  installed: boolean
  configured: boolean
  config_paths: string[]
  applied?: AgentApplied
}

export interface AgentsView {
  clients: AgentClient[]
  loopback: boolean
  base_url: string
}
