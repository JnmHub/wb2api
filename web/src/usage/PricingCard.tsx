import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Card, Input, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { RefreshCw } from 'lucide-react'
import { get, post, type PricingCost, type PricingCostRow, type PricingStatus } from '../api'
import { num, tokens } from '../format'

/**
 * Estimated API-equivalent cost of the selected range, priced from OpenRouter's
 * catalogue. Unpriced models can be mapped by hand without leaving the card.
 */
export function PricingCard({ base }: { base: string }) {
  const qc = useQueryClient()
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
  const c = cost.data
  const rows = c?.models ?? []
  const unpriced = rows.filter((r) => !r.priced)
  const columns: ColumnsType<PricingCostRow> = [
    { title: '模型', dataIndex: 'model', ellipsis: true, render: (v: string) => <span className="mono">{v}</span> },
    { title: '请求', dataIndex: 'requests', width: 76, align: 'right', render: (v: number) => num(v) },
    {
      title: 'Token',
      width: 96,
      align: 'right',
      render: (_, r) => tokens(r.prompt_tokens + r.completion_tokens),
    },
    {
      title: '等值成本',
      width: 170,
      align: 'right',
      render: (_, r) =>
        r.priced ? (
          <span>
            ¥{money(r.cost_cny)} <Typography.Text type="secondary">/ ${money(r.cost_usd)}</Typography.Text>
            {r.source !== 'exact' && <Tag style={{ marginLeft: 6 }}>{r.source}</Tag>}
          </span>
        ) : (
          <Tag color="warning">未定价</Tag>
        ),
    },
  ]
  return (
    <Card
      title="价格估算"
      style={{ marginBottom: 12 }}
      extra={
        <Space>
          {c && (
            <Typography.Text>
              <b>¥{money(c.totals.cost_cny)}</b> <Typography.Text type="secondary">/ ${money(c.totals.cost_usd)}</Typography.Text>
            </Typography.Text>
          )}
          {status.data?.stale && <Tag color="warning">目录已过期</Tag>}
          <Button size="small" icon={<RefreshCw size={13} />} loading={refresh.isPending} onClick={() => refresh.mutate()}>
            刷新价格
          </Button>
        </Space>
      }
      styles={{ body: { padding: rows.length ? 0 : 16 } }}
    >
      <Typography.Paragraph type="secondary" style={{ padding: '12px 16px 0', margin: 0 }}>
        按 OpenRouter 公布的模型级单价估算等值 API 成本，不是实际扣费；缓存命中按缓存读取价。汇率固定为目录里的 usd_cny。
        {c && ` 已定价 ${c.totals.priced_models} 个模型${c.totals.unpriced_models ? `，未定价 ${c.totals.unpriced_models} 个` : ''}。`}
        {status.data?.error && ` 上次刷新失败：${status.data.error}`}
      </Typography.Paragraph>
      {refresh.isError && <Typography.Text type="danger">{String(refresh.error)}</Typography.Text>}
      {map.isError && <Typography.Text type="danger">{String(map.error)}</Typography.Text>}
      {rows.length === 0 ? (
        <Typography.Text type="secondary">这个区间还没有用量。</Typography.Text>
      ) : (
        <>
          <Table<PricingCostRow>
            rowKey="model"
            size="small"
            columns={columns}
            dataSource={rows.slice(0, 8)}
            pagination={false}
            style={{ marginTop: 8 }}
          />
          {unpriced.length > 0 && (
            <div className="pricing-map">
              <Typography.Text type="secondary">为未定价模型指定 OpenRouter ID（例如 z-ai/glm-5.3-flash），保存后立刻重新计价。</Typography.Text>
              {unpriced.slice(0, 10).map((r) => (
                <div className="pricing-map-row" key={r.model}>
                  <span className="mono pricing-map-model">{r.model}</span>
                  <Input
                    size="small"
                    placeholder="OpenRouter ID"
                    value={mapping[r.model] ?? ''}
                    onChange={(e) => setMapping({ ...mapping, [r.model]: e.target.value })}
                    onPressEnter={() => mapping[r.model]?.trim() && map.mutate({ model: r.model, or_id: mapping[r.model].trim() })}
                  />
                  <Button
                    size="small"
                    type="primary"
                    disabled={!mapping[r.model]?.trim() || map.isPending}
                    onClick={() => map.mutate({ model: r.model, or_id: mapping[r.model].trim() })}
                  >
                    映射
                  </Button>
                </div>
              ))}
              {unpriced.length > 10 && <Typography.Text type="secondary">还有 {unpriced.length - 10} 个未定价模型，先处理最常用的这些。</Typography.Text>}
            </div>
          )}
        </>
      )}
    </Card>
  )
}

function money(v: number) {
  return v < 0.01 && v > 0 ? v.toFixed(4) : v.toFixed(2)
}
