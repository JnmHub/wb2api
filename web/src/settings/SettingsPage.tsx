import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, get, patch, post, type SettingsView } from '../api'
import { Toggle } from '../components/Modal'
import { KeysSection } from './KeysSection'
import { ProxySection } from './ProxySection'

export function useSettings() {
  return useQuery({ queryKey: ['settings'], queryFn: () => get<SettingsView>('/api/v2/settings') })
}

/** PATCH /settings with a partial body; the answer is the fresh snapshot. */
export function useSaveSettings() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: Record<string, unknown>) => patch<SettingsView>('/api/v2/settings', body),
    onSuccess: (s) => qc.setQueryData(['settings'], s),
  })
}

const limitInfo: Record<string, { label: string; hint: string; unit: string }> = {
  reserve_credits: { label: '保留积分线', hint: '余额不高于此值的账号停止接单，避免被扣到零触发上游提醒短信。0 = 关闭。', unit: '积分' },
  daily_token_limit: { label: '每日 Token 上限', hint: '每个账号当天累计 token 达到后停到次日零点。0 = 不限。', unit: 'tokens' },
  daily_credit_limit: { label: '每日积分上限', hint: '每个账号当天花费达到后只停付费模型，免费模型照常。0 = 不限。', unit: '积分' },
  model_daily_token_limit: { label: '单模型每日 Token 上限', hint: '只停用到顶的那一个模型，账号的其它模型照常。0 = 不限。', unit: 'tokens' },
  expiring_window_days: { label: '临期积分优先窗口', hint: '最早到期的积分包落在此天数内的账号优先接单（偏好，不是限制）。0 = 关闭。', unit: '天' },
}

const switchInfo: { key: string; label: string; hint: string }[] = [
  { key: 'auto_switch_product', label: '429 时自动切换出站身份', hint: '同一账号在 桌面端 → VSCode → CLI 之间换身份重试，等于换一条配额线；会占用重试次数。' },
  { key: 'egress_backoff_enabled', label: '出口级瞬时失败退避', hint: '502/503/504 与连接抖动记在出口上做秒级退避，而不是让每个账号吃 30 分钟熔断。' },
  { key: 'free_model_credit_fallback', label: '免费模型用尽后回退到付费同款', hint: '仅国际版 deepseek-v4.1-flash → -sg：所有账号的免费窗口都用尽后才会触发一次，会真实消耗积分。' },
  { key: 'enable_anthropic_messages', label: '启用 Anthropic Messages（/v1/messages）', hint: '网关会忽略 cache_control，可能多耗积分，默认关闭。' },
  { key: 'auth_disabled', label: '关闭 API Key 校验', hint: '危险：任何能访问端口的人都能使用你的账号。仅限可信内网。' },
]

export function SettingsPage() {
  const q = useSettings()
  const save = useSaveSettings()
  if (q.isPending) return <div className="page dim">加载中…</div>
  if (q.isError) return <div className="page err">{(q.error as Error).message}</div>
  const s = q.data
  return (
    <div className="page settings">
      {save.isError && <div className="err" role="alert">{(save.error as Error).message}</div>}

      <section className="card sec">
        <h3>出口</h3>
        <p className="dim">未绑定出口的 Key、也没有 X-Realm / ?realm= 的请求，走这里选的出口。只在国内版提供的模型（如 deepseek-v4-pro）必须选国内版或使用绑定国内版的 Key。</p>
        <div className="chips">
          {(['intl', 'cn'] as const).map((r) => (
            <button key={r} className="chip" aria-pressed={s.realm.current === r} onClick={() => save.mutate({ realm: r })}>
              {r === 'intl' ? '国际版 (Global)' : '国内版 (China)'}
            </button>
          ))}
        </div>
      </section>

      <CatalogSection s={s} />
      <KeysSection s={s} />

      <section className="card sec">
        <h3>限额与调度偏好</h3>
        <p className="dim">每项有一个全局值和可选的区域覆盖（留空 = 继承全局）。修改立即生效。</p>
        <div className="limits">
          <div className="lhead">
            <span />
            <span>全局</span>
            <span>国际版</span>
            <span>国内版</span>
          </div>
          {Object.entries(limitInfo)
            .filter(([key]) => s.limits[key])
            .map(([key, info]) => (
              <LimitRow key={key} name={key} info={info} entry={s.limits[key]} onSave={(body) => save.mutate({ limits: { [key]: body } })} />
            ))}
        </div>
      </section>

      <section className="card sec">
        <h3>开关</h3>
        <div className="switches">
          {switchInfo.map((sw) => (
            <Toggle
              key={sw.key}
              label={sw.label}
              hint={sw.hint}
              checked={!!s.switches[sw.key]}
              onChange={(v) => save.mutate({ switches: { [sw.key]: v } })}
            />
          ))}
        </div>
        {s.switches.auto_switch_product && (
          <div className="field">
            <label>允许切换到的身份</label>
            <div className="chips">
              {['workbuddy', 'vscode', 'cli'].map((p) => {
                const on = s.auto_switch_products.includes(p)
                return (
                  <button
                    key={p}
                    className="chip"
                    aria-pressed={on}
                    onClick={() => save.mutate({ auto_switch_products: on ? s.auto_switch_products.filter((x) => x !== p) : [...s.auto_switch_products, p] })}
                  >
                    {p}
                  </button>
                )
              })}
            </div>
          </div>
        )}
      </section>

      <UpstreamSection s={s} />
      <ProbeSection s={s} />
      <ProxySection s={s} />
    </div>
  )
}

function LimitRow(props: { name: string; info: { label: string; hint: string; unit: string }; entry: SettingsView['limits'][string]; onSave: (b: Record<string, unknown>) => void }) {
  const { entry } = props
  const [g, setG] = useState(String(entry.global))
  const [i, setI] = useState(entry.intl == null ? '' : String(entry.intl))
  const [c, setC] = useState(entry.cn == null ? '' : String(entry.cn))
  const dirty = g !== String(entry.global) || i !== (entry.intl == null ? '' : String(entry.intl)) || c !== (entry.cn == null ? '' : String(entry.cn))
  const submit = () => {
    const body: Record<string, unknown> = { global: Number(g || 0) }
    const clear: string[] = []
    if (i === '') clear.push('intl')
    else body.intl = Number(i)
    if (c === '') clear.push('cn')
    else body.cn = Number(c)
    if (clear.length) body.clear = clear
    props.onSave(body)
  }
  return (
    <div className="lrow">
      <div>
        <b>{props.info.label}</b> <span className="dim">（{props.info.unit}）</span>
        <div className="dim hint">{props.info.hint}</div>
      </div>
      <input className="input" type="number" min={0} value={g} onChange={(e) => setG(e.target.value)} aria-label={props.info.label + ' 全局'} />
      <input className="input" type="number" min={0} placeholder="继承" value={i} onChange={(e) => setI(e.target.value)} aria-label={props.info.label + ' 国际版'} />
      <input className="input" type="number" min={0} placeholder="继承" value={c} onChange={(e) => setC(e.target.value)} aria-label={props.info.label + ' 国内版'} />
      <button className="btn sm primary" disabled={!dirty} onClick={submit}>
        保存
      </button>
    </div>
  )
}

function CatalogSection({ s }: { s: SettingsView }) {
  const qc = useQueryClient()
  const [msg, setMsg] = useState<Record<string, string>>({})
  const refresh = useMutation({
    mutationFn: async (realm: string) => {
      try {
        return await post<{ ok: boolean; msg: string }>('/api/v2/catalog/refresh', { realm })
      } catch (e) {
        if (e instanceof ApiError) return { ok: false, msg: e.message }
        throw e
      }
    },
    onSuccess: (r, realm) => {
      setMsg((m) => ({ ...m, [realm]: r.msg }))
      qc.invalidateQueries({ queryKey: ['settings'] })
    },
  })
  const label: Record<string, string> = { remote: '上游 API 实时', cache: '本机桌面缓存文件', legacy: '桌面缓存（宽松解析）', bundled: '内置快照' }
  return (
    <section className="card sec">
      <h3>模型清单</h3>
      <p className="dim">/v1/models 的内容。优先从上游 /v3/config 取，每 5 分钟缓存；这里可以手动刷新（同一出口 30 秒冷却）。</p>
      <div className="catalogs">
        {(s.catalog ?? []).map((c) => (
          <div key={c.realm} className="catrow">
            <b>{c.realm === 'intl' ? '国际版' : '国内版'}</b>
            <span>{label[c.source] ?? c.source}</span>
            <span className="dim">{c.count ? c.count + ' 个模型' : ''}</span>
            <span className="dim">{c.fetched_at ? new Date(c.fetched_at * 1000).toLocaleString() : ''}</span>
            <button className="btn sm" disabled={refresh.isPending} onClick={() => refresh.mutate(c.realm)}>
              刷新
            </button>
            {msg[c.realm] && <span className="dim">{msg[c.realm]}</span>}
          </div>
        ))}
      </div>
    </section>
  )
}

function ProbeSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  return (
    <section className="card sec">
      <h3>额度窗口探针</h3>
      <p className="dim" style={{ margin: 0 }}>
        上游按 24 小时窗口给「账号 × 模型」配额，撞到 429 时会给一个重置时刻。网关会在那一刻发一个最小请求：通了就说明这条配额线已经重新开始计时，仍然 429 就按 15 秒重试（最多 20 次）。它只影响已经限额的配额线，不额外消耗正常请求的额度。
      </p>
      <Toggle
        label="启用额度窗口探针"
        hint="默认开启。关掉后不再自动探测，已限额的模型要等下一次真实请求命中才会重新计时。"
        checked={s.window_probe}
        onChange={(v) => save.mutate({ window_probe: v })}
      />
    </section>
  )
}

function UpstreamSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  const [hdr, setHdr] = useState(String(s.upstream.header_timeout_seconds))
  const [idle, setIdle] = useState(String(s.upstream.idle_timeout_seconds))
  const [token, setToken] = useState('')
  const [tokenFile, setTokenFile] = useState(s.upstream.device_token_file)
  const [mode, setMode] = useState(s.prompt.mode)
  const [file, setFile] = useState(s.prompt.file)
  const [credits, setCredits] = useState(String(s.credits_refresh_hours))
  const upstream: Record<string, unknown> = {
    header_timeout_seconds: Number(hdr), idle_timeout_seconds: Number(idle), device_token_file: tokenFile,
  }
  if (token) upstream.device_token = token
  return (
    <section className="card sec">
      <h3>上游与提示词</h3>
      <div className="grid2">
        <div className="field">
          <label>响应头超时（秒）：连接 + 首字节</label>
          <input className="input" type="number" min={1} value={hdr} onChange={(e) => setHdr(e.target.value)} />
        </div>
        <div className="field">
          <label>流空闲超时（秒）：流中途静默多久算断</label>
          <input className="input" type="number" min={1} value={idle} onChange={(e) => setIdle(e.target.value)} />
        </div>
        <div className="field">
          <label>X-Device-Token（仅国内版发送）{s.upstream.device_token_set ? ' — 已设置' : ''}</label>
          <input className="input" type="password" placeholder={s.upstream.device_token_set ? '留空 = 保持不变' : '可选'} value={token} onChange={(e) => setToken(e.target.value)} />
        </div>
        <div className="field">
          <label>或设备 Token 文件路径</label>
          <input className="input" value={tokenFile} onChange={(e) => setTokenFile(e.target.value)} />
        </div>
        <div className="field">
          <label>系统提示词模式</label>
          <select className="select" value={mode} onChange={(e) => setMode(e.target.value as typeof mode)}>
            <option value="passthrough">透传客户端的系统提示词（默认）</option>
            <option value="append">追加网关提示词</option>
            <option value="custom">用网关提示词替换</option>
          </select>
        </div>
        <div className="field">
          <label>提示词文件（custom / append 模式，留空 = 内置中性提示词）</label>
          <input className="input" value={file} onChange={(e) => setFile(e.target.value)} disabled={mode === 'passthrough'} />
        </div>
        <div className="field">
          <label>积分后台刷新间隔（小时，0 = 关闭）</label>
          <input className="input" type="number" min={0} step={0.5} value={credits} onChange={(e) => setCredits(e.target.value)} />
        </div>
      </div>
      <div className="row end">
        <button
          className="btn primary"
          disabled={save.isPending}
          onClick={() => {
            save.mutate({ upstream, prompt: { mode, file }, credits_refresh_hours: Number(credits) })
            setToken('')
          }}
        >
          保存
        </button>
      </div>
    </section>
  )
}
