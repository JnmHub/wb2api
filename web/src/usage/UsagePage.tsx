import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Card, Col, DatePicker, Progress, Row, Segmented, Select, Space, Statistic, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import dayjs from 'dayjs'
import { Line } from '@ant-design/charts'
import { get, type RemainingPayload, type RemainingRow, type SeriesPoint, type UsageAgg, type UsageSummary } from '../api'
import { credit, ms, num, pct, span, tokens } from '../format'
import { useThemeMode } from '../ui/theme'
import { PricingCard } from './PricingCard'

type Range = 'today' | 'week' | 'month' | 'all'
const RANGES: { value: Range; label: string }[] = [
  { value: 'today', label: '今日' },
  { value: 'week', label: '本周' },
  { value: 'month', label: '本月' },
  { value: 'all', label: '全部' },
]

export function UsagePage() {
  const { mode } = useThemeMode()
  const [range, setRange] = useState<Range>('today')
  const [realm, setRealm] = useState('')
  const [dates, setDates] = useState<[string, string] | null>(null)
  const base = dates
    ? `range=custom&since=${dates[0]}&until=${dates[1]}${realm ? `&realm=${realm}` : ''}`
    : `range=${range}${realm ? `&realm=${realm}` : ''}`
  const live = { refetchInterval: 10_000, refetchIntervalInBackground: false }
  const sum = useQuery({ queryKey: ['usage', base], queryFn: () => get<UsageSummary>(`/api/v2/usage/summary?${base}`), ...live })
  const spanDays = dates ? Math.max(1, (Date.parse(dates[1]) - Date.parse(dates[0])) / 86_400_000) : 0
  const bucket = dates ? (spanDays <= 2 ? 'hour' : 'day') : range === 'today' ? 'hour' : 'day'
  const series = useQuery({
    queryKey: ['usage-series', base, bucket],
    queryFn: () => get<{ points: SeriesPoint[] }>(`/api/v2/usage/series?${base}&bucket=${bucket}`),
    ...live,
  })
  const t = sum.data?.totals
  const success = t && t.requests ? (t.ok / t.requests) * 100 : undefined
  const points = (series.data?.points ?? []).map((p) => ({
    bucket: bucket === 'hour' ? p.bucket.slice(11, 13) + ':00' : p.bucket.slice(5),
    tokens: p.total_tokens,
    requests: p.requests,
  }))
  return (
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            用量
          </Typography.Title>
          <Typography.Text type="secondary">
            {num(t?.requests)} 次请求 · {tokens(t?.total_tokens)} tokens · 成功率 {pct(success)}
          </Typography.Text>
        </div>
        <Space wrap>
          <Segmented
            value={dates ? 'custom' : range}
            onChange={(v) => {
              if (v === 'custom') return
              setDates(null)
              setRange(v as Range)
            }}
            options={RANGES}
          />
          <DatePicker.RangePicker
            value={dates ? [dayjs(dates[0]), dayjs(dates[1])] : null}
            onChange={(v) => {
              if (v && v[0] && v[1]) setDates([v[0].format('YYYY-MM-DD'), v[1].format('YYYY-MM-DD')])
              else setDates(null)
            }}
            allowClear
            placeholder={['开始日期', '结束日期']}
          />
          <Select
            value={realm}
            style={{ width: 128 }}
            onChange={setRealm}
            options={[
              { value: '', label: '全部区域' },
              { value: 'intl', label: '国际版' },
              { value: 'cn', label: '国内版' },
            ]}
          />
        </Space>
      </div>

      <Row gutter={[12, 12]} className="stat-row">
        <Kpi label="请求数" value={num(t?.requests)} dot="#00a2fb" />
        <Kpi label="成功率" value={pct(success)} dot="#10b981" />
        <Kpi label="总 Token" value={tokens(t?.total_tokens)} dot="#7c3aed" />
        <Kpi label="输出 Token" value={tokens(t?.completion_tokens)} dot="#e11d48" />
        <Kpi label="缓存命中" value={pct(t?.cache_hit_pct)} dot="#f59e0b" />
        <Kpi label="平均首字" value={ms(t?.avg_ttft_ms)} dot="#0891b2" />
        <Kpi label="生成速度" value={t?.tokens_per_sec ? t.tokens_per_sec.toFixed(1) + ' t/s' : '—'} dot="#059669" />
        <Kpi label="积分消耗" value={credit(t?.credit)} dot="#b45309" />
      </Row>

      <Card
        title={bucket === 'hour' ? '每小时 Token' : '每日 Token'}
        extra={<Typography.Text type="secondary">{points.length ? `共 ${points.length} 个时间点` : '暂无数据'}</Typography.Text>}
        styles={{ body: { paddingTop: 8 } }}
        style={{ marginBottom: 12 }}
      >
        {points.length > 0 ? (
          <Line
            data={points}
            xField="bucket"
            yField="tokens"
            height={230}
            theme={mode === 'dark' ? 'classicDark' : 'classic'}
            style={{ lineWidth: 2 }}
            axis={{ y: { labelFormatter: (v: number) => tokens(v) } }}
            tooltip={{ items: [{ channel: 'y', valueFormatter: (v: number) => tokens(v) + ' tokens' }] }}
          />
        ) : (
          <div className="chart-empty">这个区间还没有请求。</div>
        )}
      </Card>

      <PricingCard base={base} />

      {sum.data && sum.data.queue.dropped > 0 && (
        <Tag color="warning" style={{ marginBottom: 12 }}>
          已丢弃 {num(sum.data.queue.dropped)} 条用量记录
        </Tag>
      )}

      <Row gutter={[12, 12]}>
        <Breakdown title="按模型" rows={sum.data?.by_model} />
        <Breakdown title="按账号" rows={sum.data?.by_account} mono note={sum.data?.by_account_truncated ? '仅显示前 50 个' : undefined} />
        <Breakdown title="按 API Key" rows={sum.data?.by_key} blank="（未使用 Key）" />
        <Breakdown title="按区域" rows={sum.data?.by_realm} blank="（未知）" />
      </Row>

      <RemainingCard />
    </div>
  )
}

function Kpi({ label, value, dot }: { label: string; value: string; dot: string }) {
  return (
    <Col xs={12} sm={12} md={6} xl={3} flex="1 1 160px">
      <Card styles={{ body: { padding: '12px 16px' } }}>
        <Statistic
          title={
            <span className="stat-title">
              <i style={{ background: dot }} /> {label}
            </span>
          }
          value={value}
          valueStyle={{ fontSize: 22, fontWeight: 600 }}
        />
      </Card>
    </Col>
  )
}

function Breakdown({ title, rows, mono, note, blank }: { title: string; rows?: (UsageAgg & { key: string })[]; mono?: boolean; note?: string; blank?: string }) {
  const list = rows ?? []
  const columns: ColumnsType<UsageAgg & { key: string }> = [
    {
      title: '名称',
      dataIndex: 'key',
      ellipsis: true,
      render: (v: string) => <span className={mono ? 'mono' : undefined}>{v || blank || '—'}</span>,
    },
    { title: '请求', dataIndex: 'requests', width: 76, align: 'right', render: (v: number) => num(v) },
    { title: 'Token', dataIndex: 'total_tokens', width: 92, align: 'right', render: (v: number) => tokens(v) },
    { title: '缓存', dataIndex: 'cache_hit_pct', width: 72, align: 'right', render: (v: number) => pct(v, 0) },
    { title: '积分', dataIndex: 'credit', width: 72, align: 'right', render: (v: number) => credit(v) },
  ]
  return (
    <Col xs={24} xl={12}>
      <Card title={title} extra={note && <Typography.Text type="secondary">{note}</Typography.Text>} styles={{ body: { padding: 0 } }}>
        <Table<UsageAgg & { key: string }>
          rowKey="key"
          size="small"
          columns={columns}
          dataSource={list.slice(0, 8)}
          pagination={false}
          locale={{ emptyText: '没有数据' }}
        />
      </Card>
    </Col>
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
  const columns: ColumnsType<RemainingRow> = [
    { title: '账号', dataIndex: 'nickname', width: 150, ellipsis: true, render: (v: string, r) => v || r.uid.slice(0, 8) },
    { title: '模型', dataIndex: 'model', ellipsis: true, render: (v: string) => <span className="mono">{v}</span> },
    { title: '已用', dataIndex: 'used', width: 96, align: 'right', render: (v: number) => tokens(v) },
    { title: '预算', dataIndex: 'budget', width: 96, align: 'right', render: (v: number | null) => (v == null ? '—' : tokens(v)) },
    { title: '剩余', dataIndex: 'remaining', width: 96, align: 'right', render: (v: number | null) => (v == null ? '—' : tokens(v)) },
    {
      title: '占用',
      dataIndex: 'used_pct',
      width: 140,
      render: (v: number | null) => (v == null ? '—' : <Progress percent={Math.round(v)} size="small" status={v > 90 ? 'exception' : 'normal'} />),
    },
    {
      title: '状态',
      width: 150,
      render: (_, r) => (
        <Typography.Text type="secondary">
          {r.cooling ? `冷却中 · ${r.reset_at ? span(r.reset_at - Date.now() / 1000) + '后重置' : '待重置'}` : r.estimated ? '按 24h 估算' : '本窗口'}
        </Typography.Text>
      ),
    },
  ]
  return (
    <Card
      title="剩余用量估算"
      extra={
        <Typography.Text type="secondary">
          {q.data ? `更新于 ${new Date(q.data.generated_at * 1000).toLocaleTimeString()}` : '按 24 小时窗口估算'}
        </Typography.Text>
      }
      style={{ marginTop: 12 }}
      styles={{ body: { padding: rows.length ? 0 : 16 } }}
    >
      {q.isError && <Typography.Text type="secondary">暂无估算：需要先有一次带重置时刻的 429 撞线记录。</Typography.Text>}
      {!q.isError && rows.length === 0 && <Typography.Text type="secondary">还没有可估算的「账号×模型」配额线。</Typography.Text>}
      {rows.length > 0 && (
        <Table<RemainingRow>
          rowKey={(r) => r.uid + r.model}
          size="small"
          columns={columns}
          dataSource={rows}
          pagination={{ pageSize: 8, hideOnSinglePage: true, size: 'small' }}
          scroll={{ x: 900 }}
        />
      )}
      {models.length > 0 && (
        <div style={{ padding: '8px 16px 12px' }}>
          <Space wrap size={[6, 6]}>
            {models.map((m) => (
              <Tag key={m.realm + m.model}>
                {m.model} · 预算 {m.budget == null ? '—' : tokens(m.budget)} · 剩余 {m.total_remaining == null ? '—' : tokens(m.total_remaining)}
              </Tag>
            ))}
          </Space>
        </div>
      )}
    </Card>
  )
}
