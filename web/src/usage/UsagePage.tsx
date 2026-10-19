import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { get, type SeriesPoint, type UsageAgg, type UsageSummary } from '../api'
import { BarChart } from '../components/Chart'
import { credit, ms, num, pct, tokens } from '../format'

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

      <div className="grid2">
        <Table title="按模型" rows={sum.data?.by_model} />
        <Table title="按账号" rows={sum.data?.by_account} note={sum.data?.by_account_truncated ? '仅显示前 50 个' : undefined} mono />
        <Table title="按 API Key" rows={sum.data?.by_key} blank="（未使用 Key）" />
        <Table title="按区域" rows={sum.data?.by_realm} blank="（未知）" />
      </div>
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
