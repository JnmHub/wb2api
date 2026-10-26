import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { LogsPage } from './logs/LogsPage'
import { SettingsPage } from './settings/SettingsPage'
import { TasksPage } from './tasks/TasksPage'

function json(body: unknown) {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

/** Routes fetch by URL substring; anything else is a 404 JSON error. */
function route(map: Record<string, () => Response>) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    for (const [part, make] of Object.entries(map)) if (url.includes(part)) return make()
    return new Response(JSON.stringify({ error: { message: 'not found' } }), { status: 404 })
  }) as unknown as typeof fetch
}

function renderPage(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('TasksPage', () => {
  it('renders the scheduler state and the activity table', async () => {
    route({
      '/api/v2/scheduler': () =>
        json({
          enabled: true,
          running: false,
          last_run_time: '2026-10-19 10:00:00',
          next_run_time: '2026-10-19 22:00:00',
          last_summary: '巡检完成',
          schedule: { checkin: [9, 21], keepalive: [22] },
          logs: ['scheduler tick'],
          credits: { ttl_hours: 6, last_run: '', last_uid: '', last_error: '' },
        }),
      '/api/v2/activity': () =>
        json({
          rows: [
            { ts: '2026-10-19 09:00:00', uid: 'u1', nickname: 'Alpha', realm: 'cn', task: 'checkin', trigger: 'scheduler', ok: true, message: '签到成功' },
          ],
          total: 1,
          limit: 200,
          truncated: false,
        }),
    })
    renderPage(<TasksPage />)
    expect(await screen.findByText('后台任务')).toBeInTheDocument()
    expect(await screen.findByText('Alpha')).toBeInTheDocument()
    expect(screen.getByText('签到成功')).toBeInTheDocument()
    expect(screen.getByText('已启用')).toBeInTheDocument()
    expect(screen.getByDisplayValue('9, 21')).toBeInTheDocument()
  })
})

describe('LogsPage', () => {
  it('renders log lines returned by the ring', async () => {
    route({
      '/api/v2/logs': () =>
        json({
          entries: [
            { id: 7, ts: '2026-10-19 10:00:00', time: '10:00:00', level: 'WARN', tag: 'egress', msg: 'soft rate limited' },
          ],
          last: 7,
          tags: ['egress'],
        }),
    })
    renderPage(<LogsPage />)
    expect(await screen.findByText('soft rate limited')).toBeInTheDocument()
    expect(screen.getByText('WARN')).toBeInTheDocument()
    expect(screen.getByText('[egress]')).toBeInTheDocument()
  })
})

describe('SettingsPage', () => {
  const settings = {
    realm: { current: 'intl', options: ['intl', 'cn'] },
    keys: [
      { id: 'k1', name: 'ci', key_preview: 'sk-a…b', realm: '', models: [], concurrency_limit: 0, enabled: true, created_at: '', deleted_at: '', deleted: false },
    ],
    auth_required: true,
    limits: {
      reserve_credits: { global: 5, intl: null, cn: null },
      daily_token_limit: { global: 0, intl: null, cn: null },
      daily_credit_limit: { global: 0, intl: null, cn: null },
      model_daily_token_limit: { global: 0, intl: null, cn: null },
      expiring_window_days: { global: 3, intl: 7, cn: null },
    },
    switches: { auto_switch_product: true, egress_backoff_enabled: true, enable_anthropic_messages: false, auth_disabled: false },
    auto_switch_products: ['workbuddy', 'vscode'],
    upstream: { header_timeout_seconds: 120, idle_timeout_seconds: 60, device_token_set: false, device_token_file: '' },
    prompt: { mode: 'passthrough', file: '' },
    credits_refresh_hours: 6,
    window_probe: true,
    quota_scan_models: [],
    banned_models: [],
    banned_substring: '',
    model_aliases: {},
    models_only_available: false,
    proxy_slots: [{ id: 's1', name: 'hk', url: 'socks5://127.0.0.1:1080', enabled: true, ip: '', country: '', country_code: '', ip_type: '', isp: '', asn: '', probed_at: 0 }],
    catalog: [{ realm: 'intl', source: 'bundled', fetched_at: 0, count: 3 }],
  }

  it('renders every settings section from the snapshot', async () => {
    route({ '/api/v2/settings': () => json(settings) })
    renderPage(<SettingsPage />)
    expect(await screen.findByText('API Key')).toBeInTheDocument()
    expect(screen.getByText('模型清单')).toBeInTheDocument()
    expect(screen.getByText('限额与调度偏好')).toBeInTheDocument()
    expect(screen.getByText('关闭 API Key 校验')).toBeInTheDocument()
    expect(screen.getByDisplayValue('socks5://127.0.0.1:1080')).toBeInTheDocument()
    expect(screen.getByDisplayValue('7')).toBeInTheDocument() // intl override of expiring_window_days
  })
})
