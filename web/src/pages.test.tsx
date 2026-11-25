import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AccountDrawer } from './accounts/AccountDrawer'
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

describe('AccountDrawer', () => {
  it('refreshes credits for a disabled CodeArts account and displays the result', async () => {
    const path = '/api/v2/accounts/codearts-test'
    const message = '剩 12 / 40 积分（基础套餐）'
    route({
      [`${path}/actions/credits`]: () => json({ ok: true, msg: message }),
      [path]: () => json({
        uid: 'codearts-test',
        nickname: 'CodeArts 测试账号',
        realm: 'codearts',
        enabled: false,
        tasks_enabled: false,
        status: 'disabled',
        weight: 1,
        note: '',
        disabled_models: [],
        proxy_slot: '',
        product: 'workbuddy',
        source: 'codearts-import',
        expires_at: 0,
        inflight: 0,
        today: { requests: 0, tokens: 0 },
      }),
    })

    renderPage(<AccountDrawer uid="codearts-test" onClose={() => {}} />)
    const refresh = await screen.findByRole('button', { name: '刷新积分' })
    expect(refresh).toBeEnabled()
    fireEvent.click(refresh)
    expect(await screen.findByText(message)).toBeInTheDocument()
    expect(globalThis.fetch).toHaveBeenCalledWith(`${path}/actions/credits`, expect.objectContaining({ method: 'POST' }))
  })
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
    upstream: { header_timeout_seconds: 120, idle_timeout_seconds: 60, codearts_history_limit_mib: 0, raccoon_history_limit_mib: 0, device_token_set: false, device_token_file: '' },
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

  it.each([
    ['CodeArts 历史裁剪阈值', 'codearts_history_limit_mib', '小浣熊历史裁剪阈值', 'raccoon_history_limit_mib'],
    ['小浣熊历史裁剪阈值', 'raccoon_history_limit_mib', 'CodeArts 历史裁剪阈值', 'codearts_history_limit_mib'],
  ])('saves the fractional value for %s independently', async (label, key, otherLabel, otherKey) => {
    const upstream = { ...settings.upstream, [otherKey]: 2 }
    const fetchSettings = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method !== 'PATCH') return json({ ...settings, upstream })
      const patch = JSON.parse(String(init.body))
      return json({ ...settings, upstream: { ...upstream, ...patch.upstream } })
    })
    globalThis.fetch = fetchSettings
    renderPage(<SettingsPage />)

    const limit = await screen.findByRole('spinbutton', { name: label })
    const otherLimit = screen.getByRole('spinbutton', { name: otherLabel })
    expect(limit).toHaveAttribute('aria-valuenow', '0')
    expect(otherLimit).toHaveAttribute('aria-valuenow', '2')
    fireEvent.focus(limit)
    fireEvent.change(limit, { target: { value: '0.8' } })
    fireEvent.blur(limit)
    const card = limit.closest('.ant-card')
    if (!(card instanceof HTMLElement)) throw new Error('Missing upstream settings card')
    fireEvent.click(within(card).getByRole('button', { name: /^保\s*存$/ }))

    await waitFor(() => expect(fetchSettings).toHaveBeenCalledWith('/api/v2/settings', expect.objectContaining({ method: 'PATCH' })))
    const request = fetchSettings.mock.calls.find(([, init]) => init?.method === 'PATCH')
    const saved = JSON.parse(String(request?.[1]?.body))
    expect(saved.upstream[key]).toBe(0.8)
    expect(saved.upstream).not.toHaveProperty(otherKey)
    await waitFor(() => expect(limit).toHaveDisplayValue('0.8'))
    expect(otherLimit).toHaveAttribute('aria-valuenow', '2')
  })
})

describe('AccountDrawer (raccoon)', () => {
  it('renders a raccoon account with credit wallets, check-in and claim actions', async () => {
    const path = '/api/v2/accounts/rc-8080402'
    route({
      [`${path}/actions/credits`]: () => json({ ok: true, msg: '剩 9267 积分' }),
      [path]: () =>
        json({
          uid: 'rc-8080402',
          nickname: '小浣熊测试',
          realm: 'raccoon',
          enabled: true,
          tasks_enabled: true,
          status: 'ready',
          weight: 1,
          note: '',
          disabled_models: [],
          proxy_slot: '',
          product: '',
          source: 'raccoon-oauth',
          expires_at: 1797741411,
          inflight: 0,
          today: { requests: 3, tokens: 1200 },
          checkin: { at: '2026-11-20 13:31:12', ok: true, message: '今日积分 +300', credits: '300 积分' },
          credits: {
            remain: 9267,
            available: 9267,
            unit: '积分',
            wallets: [{ type: 'daily_points', displayName: '每日积分', balance: 276 }],
          },
        }),
    })

    renderPage(<AccountDrawer uid="rc-8080402" onClose={() => {}} />)
    expect(await screen.findByText('小浣熊')).toBeInTheDocument()
    expect(await screen.findByText(/今日积分 \+300/)).toBeInTheDocument()
    // The check-in button is offered for the raccoon channel.
    const checkin = await screen.findByRole('button', { name: /签\s*到/ })
    expect(checkin).toBeEnabled()
    const credits = await screen.findByRole('button', { name: '刷新积分' })
    fireEvent.click(credits)
    expect(await screen.findByText('剩 9267 积分')).toBeInTheDocument()
  })
})

describe('AccountDrawer (null-tolerant fields)', () => {
  it('renders an account whose array fields come back as null', async () => {
    const path = '/api/v2/accounts/rc-null'
    route({
      [path]: () =>
        json({
          uid: 'rc-null',
          nickname: '空数组账号',
          realm: 'raccoon',
          enabled: true,
          tasks_enabled: true,
          status: 'ready',
          weight: 1,
          note: '',
          disabled_models: null,
          proxy_slot: '',
          product: '',
          source: 'raccoon-import',
          expires_at: 0,
          inflight: 0,
          today: { requests: 0, tokens: 0 },
        }),
    })
    renderPage(<AccountDrawer uid="rc-null" onClose={() => {}} />)
    // The drawer used to throw "Cannot read properties of null (reading 'join')".
    expect(await screen.findByText('空数组账号')).toBeInTheDocument()
    expect(await screen.findByText('账号操作')).toBeInTheDocument()
  })
})
