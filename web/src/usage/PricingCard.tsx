import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { get, post, type PricingCost, type PricingStatus } from '../api'
import { num, tokens } from '../format'

/**
 * Estimated API-equivalent cost of the selected range, priced from OpenRouter's
 * catalogue. Unpriced models can be mapped by hand without leaving the card.
 */
export function PricingCard({ range, realm }: { range: string; realm: string }) {
  const qc = useQueryClient()
  const base = `range=${range}${realm ? `&realm=${realm}` : ''}`
  const status = useQuery({ queryKey: ['pricing'], queryFn: () => get<PricingStatus>('/api/v2/pricing'), retry: false })
  const cost = useQuery({ queryKey: ['pricing-cost', base], queryFn: () => get<PricingCost>(`/api/v2/pricing/cost?${base}`), retry: false })
  const refresh = useMutation({
    mutationFn: () => post<PricingStatus>('/api/v2/pricing/refresh', {}),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['pricing'] })
      qc.invalidateQueries({ queryKey: ['pricing-cost'] })
    },
  })
  const [mapping, setMapping] = useState<Record<string, string>>({})
  const map = useMutation({
    mutationFn: (v: { model: string; or_id: string }) => post<PricingStatus>('/api/v2/pricing/policy', { model: v.model, mode: 'manual', or_id: v.or_id }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['pricing'] })
      qc.invalidateQueries({ queryKey: ['pricing-cost'] })
    },
  })
  const s = status.data
  const c = cost.data
  const rows = (c?.models ?? []).slice(0, 8)
  return (
    <section className="card sec">
      <div className="row">
        <b>价格估算</b>
        {s?.stale && <span className="badge cooling">目录已过期</span>}
        {s && s.models > 0 && <span className="dim">OpenRouter {num(s.models)} 个模型 · 汇率 {s.usd_cny}</span>}
        <span className="spacer" />
        {c && (
          <span>
            <b>{cny(c.totals.cost_cny)}</b> <span className="dim">/ {usd(c.totals.cost_usd)}</span>
          </span>
        )}
        <button className="btn sm" disabled={refresh.isPending} onClick={() => refresh.mutate()}>
          {refresh.isPending ? '刷新中…' : '刷新价格'}
        </button>
      </div>
      <p className="dim" style={{ margin: '4px 0 8px' }}>
        按 OpenRouter 公布的模型级单价估算等值 API 成本，不是实际扣费；缓存命中按缓存读取价、其余输入按未命中价。汇率固定为目录里的 usd_cny。
      </p>
      {refresh.isError && <div className="err">{(refresh.error as Error).message}</div>}
      {s?.error && <div className="dim">上次刷新失败：{s.error}</div>}
      {c && (
        <div className="row dim">
          <span>{num(c.totals.priced_models)} 个模型已定价</span>
          {c.totals.unpriced_models > 0 && <span className="badge cooling">{num(c.totals.unpriced_models)} 个未定价</span>}
        </div>
      )}
      {rows.length === 0 && <div className="dim pad">这个区间还没有用量。</div>}
      {rows.map((r) => (
        <div key={r.model} className="row" style={{ borderTop: '1px solid var(--border)', padding: '6px 0' }}>
          <span className="mono">{r.model}</span>
          <span className="dim">
            {num(r.requests)} 次 · {tokens(r.prompt_tokens + r.completion_tokens)} tokens
          </span>
          <span className="spacer" />
          {r.priced ? (
            <span>
              {cny(r.cost_cny)} <span className="dim">/ {usd(r.cost_usd)}</span>
              {r.source !== 'exact' && <span className="dim">（{r.source}）</span>}
            </span>
          ) : (
            <span className="row">
              <input
                className="input mono"
                style={{ width: 220 }}
                placeholder="OpenRouter ID，如 z-ai/glm-5.3-flash"
                value={mapping[r.model] ?? ''}
                onChange={(e) => setMapping({ ...mapping, [r.model]: e.target.value })}
              />
              <button
                className="btn sm"
                disabled={!mapping[r.model]?.trim() || map.isPending}
                onClick={() => map.mutate({ model: r.model, or_id: mapping[r.model].trim() })}
              >
                映射
              </button>
            </span>
          )}
        </div>
      ))}
      {map.isError && <div className="err">{(map.error as Error).message}</div>}
    </section>
  )
}

function cny(v: number) {
  return '¥' + (v < 0.01 && v > 0 ? v.toFixed(4) : v.toFixed(2))
}
function usd(v: number) {
  return '$' + (v < 0.01 && v > 0 ? v.toFixed(4) : v.toFixed(2))
}
