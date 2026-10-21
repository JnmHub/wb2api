import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Card, Checkbox, Col, Input, InputNumber, Row, Segmented, Select, Space, Tag, Typography } from 'antd'
import { ApiError, get, patch, post, type SettingsView, type UpdateStatus } from '../api'
import { Toggle } from '../components/Modal'
import { AgentsSection } from './AgentsSection'
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
  { key: 'remaining_priority_enabled', label: '剩余用量优先调度', hint: '估计剩余越接近耗尽的「账号×模型」分到越多请求，先把快用完的额度用掉；默认关闭，需要先真实撞过限额才有预算样本。' },
  { key: 'local_web_tools', label: '本地执行 web_search / web_fetch', hint: '仅 /v1/responses：网关自己搜索/抓取并把结果喂回模型，默认关闭。搜索走 DuckDuckGo / Wikipedia / Google News，抓取只允许公网 http(s) 文本页。' },
  { key: 'filter_models_by_key', label: '按 API Key 过滤 /v1/models', hint: '打开后，配了模型限制的 Key 在 /v1/models 里只看到允许的模型；未限制的 Key 和面板仍看全部。默认关闭，保持全量目录。' },
  { key: 'daily_chat_web', label: '国际版每日活跃：网页会话', hint: '除桌面端轻量对话外，再建一条网页端 agent 会话并驱动它跑完。网页会话才计入每日活跃积分；默认开启，30/50 积分的官方任务依赖它。' },
  { key: 'daily_chat_web_retry', label: '网页会话失败重试', hint: '会话没在首轮等待时间内跑完时再试，等待时间每轮翻倍（首轮 120s → 240s → 480s），默认开启。' },
  { key: 'enable_anthropic_messages', label: '启用 Anthropic Messages（/v1/messages）', hint: '网关会忽略 cache_control，可能多耗积分，默认关闭。' },
  { key: 'auth_disabled', label: '关闭 API Key 校验', hint: '危险：任何能访问端口的人都能使用你的账号。仅限可信内网。' },
  { key: 'update_check_enabled', label: '启用更新检查', hint: '每天最多查一次 GitHub Releases；只读，不下载、不替换、不重启。默认关闭。' },
]

export function SettingsPage() {
  const q = useSettings()
  const save = useSaveSettings()
  if (q.isPending) return <div className="page dim">加载中…</div>
  if (q.isError) return <div className="page err">{(q.error as Error).message}</div>
  const s = q.data
  return (
    <div className="page settings">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            设置
          </Typography.Title>
          <Typography.Text type="secondary">出口、Key、限额、上游与自动化；修改立即生效。</Typography.Text>
        </div>
      </div>
      {save.isError && <div className="err" role="alert">{(save.error as Error).message}</div>}

      <Card title="出口">
        <Typography.Paragraph type="secondary">
          未绑定出口的 Key、也没有 X-Realm / ?realm= 的请求，走这里选的出口。只在国内版提供的模型（如 deepseek-v4-pro）必须选国内版或使用绑定国内版的 Key。
        </Typography.Paragraph>
        <Segmented
          value={s.realm.current}
          onChange={(r) => save.mutate({ realm: String(r) })}
          options={[
            { value: 'intl', label: '国际版 (Global)' },
            { value: 'cn', label: '国内版 (China)' },
          ]}
        />
      </Card>

      <CatalogSection s={s} />
      <KeysSection s={s} />

      <Card title="限额与调度偏好">
        <Typography.Paragraph type="secondary">每项有一个全局值和可选的区域覆盖（留空 = 继承全局）。修改立即生效。</Typography.Paragraph>
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
      </Card>

      <Card title="开关">
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
          <div style={{ marginTop: 12 }}>
            <Typography.Text type="secondary">允许切换到的身份</Typography.Text>
            <div>
              <Checkbox.Group
                value={s.auto_switch_products}
                options={['workbuddy', 'vscode', 'cli']}
                onChange={(vals) => save.mutate({ auto_switch_products: vals as string[] })}
              />
            </div>
          </div>
        )}
      </Card>

      <DailyChatSection s={s} />
      <OpsSection s={s} />
      <TpsSection s={s} />
      <UpstreamSection s={s} />
      <ProbeSection s={s} />
      <ModelsSection s={s} />
      <AgentsSection />
      <UpdatesSection />
      <ProxySection s={s} />
    </div>
  )
}

function OpsSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  const [conc, setConc] = useState(s.ops_concurrency)
  const [gap, setGap] = useState(s.ops_interval_ms)
  const dirty = conc !== s.ops_concurrency || gap !== s.ops_interval_ms
  return (
    <Card
      title="批量账号任务"
      extra={
        <Button type="primary" size="small" disabled={!dirty} loading={save.isPending} onClick={() => save.mutate({ ops_concurrency: conc, ops_interval_ms: gap })}>
          保存
        </Button>
      }
    >
      <Typography.Paragraph type="secondary" style={{ marginTop: 0 }}>
        刷新积分、签到、每日活跃、批量检测共用这两个限制。账号很多时调低并发、增加间隔，可以减轻对同一个出口 IP 的突发压力，
        代价是批量任务耗时变长。正常模型请求不受这里影响。
      </Typography.Paragraph>
      <Space size={24} wrap>
        <div>
          <Typography.Text type="secondary">同时进行的上游请求（1–16）</Typography.Text>
          <div style={{ marginTop: 4 }}>
            <InputNumber min={1} max={16} value={conc} onChange={(v) => setConc(v ?? 4)} style={{ width: 160 }} />
          </div>
        </div>
        <div>
          <Typography.Text type="secondary">每个账号操作后的间隔（毫秒，0–60000）</Typography.Text>
          <div style={{ marginTop: 4 }}>
            <InputNumber min={0} max={60000} step={50} value={gap} onChange={(v) => setGap(v ?? 0)} style={{ width: 160 }} />
          </div>
        </div>
      </Space>
      {save.isError && <div className="err" style={{ marginTop: 8 }}>{String(save.error)}</div>}
    </Card>
  )
}

function TpsSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  const [tps, setTps] = useState<number>(s.output_tps_limit)
  const dirty = tps !== s.output_tps_limit
  return (
    <Card
      title="流式输出速度限制"
      extra={
        <Button type="primary" size="small" disabled={!dirty} loading={save.isPending} onClick={() => save.mutate({ output_tps_limit: tps })}>
          保存
        </Button>
      }
    >
      <Typography.Paragraph type="secondary" style={{ marginTop: 0 }}>
        仅限制流式响应的输出速度（token/秒），首字延迟不受影响。0 = 关闭；Key 可单独覆盖，Key 的 0 表示跟随这里，-1 表示该 Key 关闭。
        上游仍按自己的速度生成和计费，网关只控制转发节奏。
      </Typography.Paragraph>
      <div>
        <Typography.Text type="secondary">全局默认上限（token/s，0–1000）</Typography.Text>
        <div style={{ marginTop: 4 }}>
          <InputNumber min={0} max={1000} step={0.5} value={tps} onChange={(v) => setTps(v ?? 0)} style={{ width: 180 }} />
        </div>
      </div>
      {save.isError && <div className="err" style={{ marginTop: 8 }}>{String(save.error)}</div>}
    </Card>
  )
}

function DailyChatSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  const [attempts, setAttempts] = useState(String(s.daily_chat_web_attempts))
  const [timeout, setTimeout] = useState(String(s.daily_chat_web_timeout))
  const dirty = attempts !== String(s.daily_chat_web_attempts) || timeout !== String(s.daily_chat_web_timeout)
  return (
    <Card
      title="国际版每日活跃"
      extra={
        <Button size="small" type="primary" disabled={!dirty} loading={save.isPending} onClick={() => save.mutate({ daily_chat_web_attempts: Number(attempts), daily_chat_web_timeout: Number(timeout) })}>
          保存
        </Button>
      }
    >
      <Typography.Paragraph type="secondary">
        网页通道会创建会话、接上沙箱并驱动一轮 agent 对话；只有真正跑完的会话才计入每日活跃积分（30/50 积分任务），所以它比桌面端那次“Hi”更重要。首轮没跑完会按“次数”重试，第 N 轮等待是首轮的 2^(N-1) 倍。
      </Typography.Paragraph>
      <Row gutter={12}>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">最多尝试次数</Typography.Text>
          <InputNumber min={1} max={5} style={{ width: '100%' }} value={Number(attempts)} onChange={(v) => setAttempts(String(v ?? 1))} aria-label="网页会话最多尝试次数" />
        </Col>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">首轮等待（秒）</Typography.Text>
          <InputNumber min={30} max={600} style={{ width: '100%' }} value={Number(timeout)} onChange={(v) => setTimeout(String(v ?? 30))} aria-label="网页会话首轮等待秒数" />
        </Col>
      </Row>
      {save.isError && <Typography.Text type="danger">{String(save.error)}</Typography.Text>}
    </Card>
  )
}

function LimitRow(props: { name: string; info: { label: string; hint: string; unit: string }; entry: SettingsView['limits'][string]; onSave: (b: Record<string, unknown>) => void }) {  const { entry } = props
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
      <InputNumber min={0} value={g === '' ? null : Number(g)} onChange={(v) => setG(v == null ? '' : String(v))} aria-label={props.info.label + ' 全局'} />
      <InputNumber min={0} value={i === '' ? null : Number(i)} placeholder="继承" onChange={(v) => setI(v == null ? '' : String(v))} aria-label={props.info.label + ' 国际版'} />
      <InputNumber min={0} value={c === '' ? null : Number(c)} placeholder="继承" onChange={(v) => setC(v == null ? '' : String(v))} aria-label={props.info.label + ' 国内版'} />
      <Button size="small" type="primary" disabled={!dirty} onClick={submit}>
        保存
      </Button>
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
  const label: Record<string, string> = { remote: '上游 API 实时', cache: '本机桌面缓存文件', legacy: '桌面缓存（宽松解析）', bundled: '内置快照', 'not built yet': '尚未加载' }
  return (
    <Card title="模型清单" extra={<Typography.Text type="secondary">同一出口 30 秒冷却</Typography.Text>}>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
        /v1/models 的内容。优先从上游 /v3/config 取，每 5 分钟缓存；这里可以手动刷新。
      </Typography.Paragraph>
      <Space direction="vertical" size={0} style={{ width: '100%' }}>
        {(s.catalog ?? []).map((c) => (
          <div key={c.realm} className="catline">
            <b>{c.realm === 'intl' ? '国际版' : '国内版'}</b>
            <span>{label[c.source] ?? c.source}</span>
            <span className="dim">{c.count ? c.count + ' 个模型' : '暂无'}</span>
            <span className="dim">{c.fetched_at ? new Date(c.fetched_at * 1000).toLocaleString() : ''}</span>
            <span className="spacer" />
            {msg[c.realm] && <span className="dim">{msg[c.realm]}</span>}
            <Button size="small" loading={refresh.isPending} onClick={() => refresh.mutate(c.realm)}>
              刷新
            </Button>
          </div>
        ))}
      </Space>
    </Card>
  )
}

function ProbeSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  const [models, setModels] = useState(s.quota_scan_models.join(', '))
  const dirty = models !== s.quota_scan_models.join(', ')
  const run = useMutation({
    mutationFn: () => post<{ ok: boolean; sent: number; msg: string }>('/api/v2/quota-scan/run', {}),
  })
  return (
    <Card
      title="额度窗口探针"
      extra={
        <Typography.Text type="secondary">撞到 429 后才生效，不额外消耗正常请求额度</Typography.Text>
      }
    >
      <Typography.Paragraph type="secondary">
        上游按 24 小时窗口给「账号 × 模型」配额，撞到 429 时会给一个重置时刻。网关会在那一刻发一个最小请求：通了就说明这条配额线已经重新开始计时，仍然 429 就按 15 秒重试（最多 20 次）。
      </Typography.Paragraph>
      <Toggle
        label="启用额度窗口探针"
        hint="默认开启。关掉后不再自动探测，已限额的模型要等下一次真实请求命中才会重新计时。"
        checked={s.window_probe}
        onChange={(v) => save.mutate({ window_probe: v })}
      />
      <div style={{ marginTop: 12 }}>
        <Typography.Text type="secondary">主动查询的模型（每 6 小时体检一次，逗号分隔；留空 = 关闭）</Typography.Text>
        <Input className="mono" value={models} onChange={(e) => setModels(e.target.value)} placeholder="hy3, deepseek-v4.1-flash" />
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          只查勾选的模型；已有 429 窗口的配额线交给上面的窗口探针，不重复查询。每次查询会消耗该账号极少量额度。
        </Typography.Text>
      </div>
      <Space style={{ marginTop: 12 }}>
        <Button size="small" loading={run.isPending} onClick={() => run.mutate()}>
          立即查询
        </Button>
        <Button
          size="small"
          type="primary"
          disabled={!dirty || save.isPending}
          onClick={() => save.mutate({ quota_scan_models: models.split(/[\s,]+/).filter(Boolean) })}
        >
          保存查询模型
        </Button>
      </Space>
      {run.data && <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>{run.data.msg}</Typography.Paragraph>}
      {run.isError && <Typography.Text type="danger">{String(run.error)}</Typography.Text>}
    </Card>
  )
}

function ModelsSection({ s }: { s: SettingsView }) {
  const save = useSaveSettings()
  const [models, setModels] = useState(s.banned_models.join('\n'))
  const [sub, setSub] = useState(s.banned_substring)
  const dirty = models !== s.banned_models.join('\n') || sub !== s.banned_substring
  return (
    <Card
      title="模型封锁"
      extra={
        <Button
          type="primary"
          size="small"
          disabled={!dirty || save.isPending}
          onClick={() => save.mutate({ banned_models: models.split(/[\s,]+/).filter(Boolean), banned_substring: sub })}
        >
          保存封锁名单
        </Button>
      }
    >
      <Typography.Paragraph type="secondary">
        命中的模型在网关本机直接返回 400，不会发往上游；同时从 `/v1/models` 列表里隐藏。精确名单优先于封锁子串，白名单（`allowed_models` / `allowed_prefixes`，默认空）又会覆盖两者。
      </Typography.Paragraph>
      <Typography.Text type="secondary">精确封锁的模型（每行一个，大小写不敏感）</Typography.Text>
      <Input.TextArea className="mono" rows={4} value={models} onChange={(e) => setModels(e.target.value)} placeholder={'gpt-5.5\ndeepseek-v4-pro'} />
      <div style={{ marginTop: 12 }}>
        <Typography.Text type="secondary">封锁子串（模型名包含它就拒绝，留空 = 关闭）</Typography.Text>
        <Input className="mono" value={sub} onChange={(e) => setSub(e.target.value)} placeholder="image-alpha" />
      </div>
      {save.isError && <Typography.Text type="danger">{String(save.error)}</Typography.Text>}
    </Card>
  )
}

function UpdatesSection() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['updates'], queryFn: () => get<UpdateStatus>('/api/v2/updates'), retry: false, refetchInterval: 60_000 })
  const check = useMutation({
    mutationFn: () => post<UpdateStatus>('/api/v2/updates/check', {}),
    onSuccess: (st) => qc.setQueryData(['updates'], st),
  })
  const st = q.data
  return (
    <Card
      title="更新检查"
      extra={
        <Button size="small" loading={check.isPending} onClick={() => check.mutate()}>
          立即检查
        </Button>
      }
    >
      <Typography.Paragraph type="secondary">
        只查询 GitHub Releases 并显示版本，不会下载或替换任何文件。默认关闭；开启后后台每天最多检查一次。
      </Typography.Paragraph>
      {q.isError && <Typography.Text type="secondary">更新检查接口不可用。</Typography.Text>}
      {st && (
        <Space wrap size={[12, 6]}>
          <Typography.Text>当前版本 {st.current_version}</Typography.Text>
          {st.latest_version && (
            <Typography.Text>
              最新 {st.latest_version}
              {st.update_available && <Tag color="warning" style={{ marginLeft: 6 }}>有新版本</Tag>}
            </Typography.Text>
          )}
          <Typography.Text type="secondary">{st.last_attempt ? `上次检查 ${st.last_attempt}` : '尚未检查'}</Typography.Text>
          {st.last_error && <Typography.Text type="danger">{st.last_error}</Typography.Text>}
          {st.release_url && (
            <a href={st.release_url} target="_blank" rel="noreferrer">
              发行页
            </a>
          )}
          {st.repo_url && (
            <a href={st.repo_url} target="_blank" rel="noreferrer">
              GitHub 仓库
            </a>
          )}
        </Space>
      )}
    </Card>
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
    <Card
      title="上游与提示词"
      extra={
        <Button
          type="primary"
          size="small"
          loading={save.isPending}
          onClick={() => {
            save.mutate({ upstream, prompt: { mode, file }, credits_refresh_hours: Number(credits) })
            setToken('')
          }}
        >
          保存
        </Button>
      }
    >
      <Row gutter={[12, 12]}>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">响应头超时（秒）：连接 + 首字节</Typography.Text>
          <InputNumber min={1} style={{ width: '100%' }} value={Number(hdr)} onChange={(v) => setHdr(String(v ?? 1))} />
        </Col>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">流空闲超时（秒）：流中途静默多久算断</Typography.Text>
          <InputNumber min={1} style={{ width: '100%' }} value={Number(idle)} onChange={(v) => setIdle(String(v ?? 1))} />
        </Col>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">积分后台刷新间隔（小时，0 = 关闭）</Typography.Text>
          <InputNumber min={0} step={0.5} style={{ width: '100%' }} value={Number(credits)} onChange={(v) => setCredits(String(v ?? 0))} />
        </Col>
        <Col xs={24} md={12}>
          <Typography.Text type="secondary">X-Device-Token（仅国内版发送）{s.upstream.device_token_set ? ' — 已设置' : ''}</Typography.Text>
          <Input.Password placeholder={s.upstream.device_token_set ? '留空 = 保持不变' : '可选'} value={token} onChange={(e) => setToken(e.target.value)} />
        </Col>
        <Col xs={24} md={12}>
          <Typography.Text type="secondary">或设备 Token 文件路径</Typography.Text>
          <Input value={tokenFile} onChange={(e) => setTokenFile(e.target.value)} />
        </Col>
        <Col xs={24} md={12}>
          <Typography.Text type="secondary">系统提示词模式</Typography.Text>
          <Select
            style={{ width: '100%' }}
            value={mode}
            onChange={(v) => setMode(v as typeof mode)}
            options={[
              { value: 'passthrough', label: '透传客户端的系统提示词（默认）' },
              { value: 'append', label: '追加网关提示词' },
              { value: 'custom', label: '用网关提示词替换' },
            ]}
          />
        </Col>
        <Col xs={24} md={12}>
          <Typography.Text type="secondary">提示词文件（custom / append 模式，留空 = 内置中性提示词）</Typography.Text>
          <Input value={file} onChange={(e) => setFile(e.target.value)} disabled={mode === 'passthrough'} />
        </Col>
      </Row>
      {save.isError && <Typography.Text type="danger">{String(save.error)}</Typography.Text>}
    </Card>
  )
}
