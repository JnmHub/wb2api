import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { get, type RemainingPayload, type SeriesPoint, type UsageAgg, type UsageSummary } from '../api'
import { BarChart } from '../components/Chart'
import { credit, ms, num, pct, span, tokens } from '../format'
import { PricingCard } from './PricingCard'

type Range = 'today' | 'week' | 'month' | 'all'
const ranges: { key: Range; label: string }[] = [
  { key: 'today', label: '今日' },
  { key: 'week', label: '本周' },
  { key: 'month', label: '本月' },
  { key: 'all', label: '全部' },
]

export function UsagePage() {
  const [range, setRange] = useState<Range>('today')
  const [realm, setRealm] = useState('')
  const base = `range=${range}${realm ? `&realm=${realm}` : ''}`
  const live = { refetchInterval: 10_000, refetchIntervalInBackground: false }
  const sum = useQuery({ queryKey: ['usage', base], queryFn: () => get<UsageSummary>(`/api/v2/usage/summary?${base}`), ...live })
  const bucket = range === 'today' ? 'hour' : 'day'
  const series = useQuery({
    queryKey: ['usage-series', base, bucket],
    queryFn: () => get<{ points: SeriesPoint[] }>(`/api/v2/usage/series?${base}&bucket=${bucket}`),
    ...live,
  })
  const t = sum.data?.totals
  const bars = (series.data?.points ?? []).map((p) => ({
    label: bucket === 'hour' ? p.bucket.slice(11) + ':00' : p.bucket.slice(5),
    value: p.total_tokens,
    detail: `${num(p.requests)} 次请求`,
  }))
  const success = t && t.requests ? (t.ok / t.requests) * 100 : undefined

  return (
    <div className="page">
      <div className="toolbar">
        <div className="chips" role="group" aria-label="时间范围">
          {ranges.map((r) => (
            <button key={r.key} className="chip" aria-pressed={range === r.key} onClick={() => setRange(r.key)}>
              {r.label}
            </button>
          ))}
        </div>
        <select className="select" value={realm} onChange={(e) => setRealm(e.target.value)} aria-label="区域">
          <option value="">全部区域</option>
          <option value="intl">国际版</option>
          <option value="cn">国内版</option>
        </select>
        <span className="spacer" />
        {sum.data && sum.data.queue.dropped > 0 && <span className="badge restricted">已丢弃 {sum.data.queue.dropped} 条用量记录</span>}
        {sum.data?.queue.last_error && <span className="badge restricted" title={sum.data.queue.last_error}>用量写入异常</span>}
      </div>

      <div className="kpis">
        <Kpi label="请求数" value={num(t?.requests)} />
        <Kpi label="成功率" value={pct(success)} />
        <Kpi label="总 Token" value={tokens(t?.total_tokens)} />
        <Kpi label="输出 Token" value={tokens(t?.completion_tokens)} />
        <Kpi label="缓存命中" value={pct(t?.cache_hit_pct)} />
        <Kpi label="平均首字延迟" value={ms(t?.avg_ttft_ms)} />
        <Kpi label="生成速度" value={t && t.tokens_per_sec ? t.tokens_per_sec.toFixed(1) + ' t/s' : '—'} />
        <Kpi label="积分消耗" value={credit(t?.credit)} />
      </div>

      <div className="card" style={{ padding: 14 }}>
        <b>{bucket === 'hour' ? '每小时 Token' : '每日 Token'}</b>
        <BarChart bars={bars} format={tokens} />
      </div>

      <PricingCard range={range} realm={realm} />

      <div className="grid2">
        <Table title="按模型" rows={sum.data?.by_model} />
        <Table title="按账号" rows={sum.data?.by_account} note={sum.data?.by_account_truncated ? '仅显示前 50 个' : undefined} mono />
        <Table title="按 API Key" rows={sum.data?.by_key} blank="（未使用 Key）" />
        <Table title="按区域" rows={sum.data?.by_realm} blank="（未知）" />
      </div>

      <RemainingCard />
    </div>
  )
}

function RemainingCard() {
  const q = useQuery({
    queryKey: ['remaining'],
    queryFn: () => get<RemainingPayload>('/api/v2/usage/remaining'),
    refetchInterval: 30_000,
    retry: false,
  })
  const rows = q.data?.rows ?? []
  const models = q.data?.models ?? []
  return (
    <div className="card sec">
      <div className="row">
        <h3>剩余用量估算</h3>
        <span className="dim">按上游 24 小时窗口估算；预算来自真实撞线时的窗口用量样本（最近三天按 6/3/1 加权）</span>
        <span className="spacer" />
        {q.data && <span className="dim">更新于 {new Date(q.data.generated_at * 1000).toLocaleTimeString()}</span>}
      </div>
      {q.isError && <div className="dim">暂无估算：需要先有一次带重置时刻的 429 撞线记录。</div>}
      {!q.isError && rows.length === 0 && <div className="dim">还没有可估算的「账号×模型」配额线。</div>}
      {models.length > 0 && (
        <div className="scroll" style={{ maxHeight: 180 }}>
          <table className="t">
            <thead>
              <tr>
                <th>模型</th>
                <th className="num">预估预算</th>
                <th className="num">总剩余</th>
                <th className="num">组合</th>
                <th className="num">冷却</th>
                <th className="num">未知</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.realm + m.model}>
                  <td title={m.model}>
                    <span className={'badge ' + m.realm}>{m.realm === 'cn' ? '国内' : '国际'}</span> {m.model}
                  </td>
                  <td className="num">{m.budget == null ? '—' : tokens(m.budget)}</td>
                  <td className="num">{m.total_remaining == null ? '—' : tokens(m.total_remaining)}</td>
                  <td className="num">{m.pairs}</td>
                  <td className="num">{m.cooling}</td>
                  <td className="num">{m.unknown}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {rows.length > 0 && (
        <div className="scroll" style={{ maxHeight: 320 }}>
          <table className="t">
            <thead>
              <tr>
                <th>账号</th>
                <th>模型</th>
                <th className="num">已用</th>
                <th className="num">预算</th>
                <th className="num">剩余</th>
                <th className="num">占用</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.uid + r.model}>
                  <td title={r.uid}>{r.nickname || r.uid.slice(0, 8)}</td>
                  <td className="mono" title={r.model}>{r.model}</td>
                  <td className="num">{tokens(r.used)}</td>
                  <td className="num">{r.budget == null ? '—' : tokens(r.budget)}</td>
                  <td className="num">{r.remaining == null ? '—' : tokens(r.remaining)}</td>
                  <td className="num">{r.used_pct == null ? '—' : pct(r.used_pct, 0)}</td>
                  <td className="dim">
                    {r.cooling ? `冷却中 · ${r.reset_at ? span(r.reset_at - Date.now() / 1000) + '后重置' : '待重置'}` : r.estimated ? '按 24h 估算' : '本窗口'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function Kpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="card kpi">
      <div className="v">{value}</div>
      <div className="l">{label}</div>
    </div>
  )
}

function Table(props: { title: string; rows?: (UsageAgg & { key: string })[]; note?: string; mono?: boolean; blank?: string }) {
  const rows = props.rows ?? []
  return (
    <div className="card">
      <div style={{ padding: '10px 12px', display: 'flex', gap: 8, alignItems: 'baseline' }}>
        <b>{props.title}</b>
        {props.note && <span className="dim">{props.note}</span>}
      </div>
      <div className="scroll">
        <table className="t">
          <thead>
            <tr>
              <th>名称</th>
              <th className="num">请求</th>
              <th className="num">Token</th>
              <th className="num">缓存</th>
              <th className="num">t/s</th>
              <th className="num">积分</th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr><td colSpan={6} className="dim">没有数据</td></tr>
            )}
            {rows.map((r) => (
              <tr key={r.key}>
                <td className={props.mono ? 'mono' : undefined} title={r.key}>{r.key || props.blank || '—'}</td>
                <td className="num">{num(r.requests)}</td>
                <td className="num">{tokens(r.total_tokens)}</td>
                <td className="num">{pct(r.cache_hit_pct, 0)}</td>
                <td className="num">{r.tokens_per_sec ? r.tokens_per_sec.toFixed(1) : '—'}</td>
                <td className="num">{credit(r.credit)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
