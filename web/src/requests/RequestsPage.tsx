import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Badge, Button, Card, Descriptions, Drawer, Grid, Input, Segmented, Select, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { Download, RefreshCw, Search } from 'lucide-react'
import { get, type UsageRequest, type UsageRequestPage } from '../api'
import { credit, ms, num, pct, realmLabel, tokens } from '../format'

const RANGES = [
  { value: 'today', label: '今天' },
  { value: 'week', label: '本周' },
  { value: 'month', label: '本月' },
  { value: 'all', label: '全部' },
]

/** One row per gateway request, newest first, with a detail drawer. */
export function RequestsPage() {
  const screens = Grid.useBreakpoint()
  const isMobile = screens.md === false
  const [range, setRange] = useState('week')
  const [outcome, setOutcome] = useState<'all' | 'ok' | 'failed' | 'aborted'>('all')
  const [realm, setRealm] = useState('')
  const [model, setModel] = useState('')
  const [account, setAccount] = useState('')
  const [active, setActive] = useState<UsageRequest | null>(null)
  const params = new URLSearchParams({ range, limit: '200' })
  if (realm) params.set('realm', realm)
  if (model.trim()) params.set('model', model.trim())
  if (account.trim()) params.set('account', account.trim())
  const key = params.toString()
  const q = useQuery({
    queryKey: ['requests', key],
    queryFn: () => get<UsageRequestPage>(`/api/v2/usage/recent?${key}`),
    refetchInterval: 15_000,
  })
  const rows = q.data?.requests ?? []
  const filtered = useMemo(
    () =>
      rows.filter((r) => {
        if (outcome === 'all') return true
        if (outcome === 'ok') return r.outcome === 'completed' || r.outcome === ''
        if (outcome === 'aborted') return r.outcome === 'client_aborted'
        return r.outcome !== 'completed' && r.outcome !== '' && r.outcome !== 'client_aborted'
      }),
    [rows, outcome],
  )
  const exportCSV = () => {
    const header = ['time', 'outcome', 'status', 'model', 'account', 'key', 'stream', 'prompt_tokens', 'completion_tokens', 'reasoning_tokens', 'cached_tokens', 'total_tokens', 'credit', 'elapsed_ms', 'ttft_ms', 'gen_ms', 'tokens_per_sec', 'reasoning_effort', 'estimated', 'error']
    const lines = filtered.map((r) =>
      [
        new Date(r.at).toISOString(),
        r.outcome,
        r.status,
        r.model,
        r.account,
        r.key,
        r.stream,
        r.prompt_tokens,
        r.completion_tokens,
        r.reasoning_tokens,
        r.cached_tokens,
        r.total_tokens,
        r.credit,
        r.elapsed_ms,
        r.ttft_ms ?? '',
        r.gen_ms ?? '',
        r.tokens_per_sec,
        r.reasoning_effort,
        r.usage_estimated,
        r.error,
      ]
        .map((v) => `"${String(v).replaceAll('"', '""')}"`)
        .join(','),
    )
    const blob = new Blob(['\ufeff' + [header.join(','), ...lines].join('\n')], { type: 'text/csv;charset=utf-8' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `requests-${range}-${new Date().toISOString().slice(0, 19).replaceAll(':', '-')}.csv`
    a.click()
    URL.revokeObjectURL(a.href)
  }

  const columns: ColumnsType<UsageRequest> = useMemo(
    () => [
      {
        title: '时间',
        dataIndex: 'at',
        width: 150,
        sorter: (a, b) => a.at - b.at,
        defaultSortOrder: 'descend',
        render: (v: number) => <span className="mono">{new Date(v).toLocaleString()}</span>,
      },
      {
        title: '结果',
        dataIndex: 'outcome',
        width: 96,
        render: (v: string) => {
          const ok = v === 'completed' || v === ''
          return <Badge status={ok ? 'success' : v === 'client_aborted' ? 'default' : 'error'} text={ok ? '成功' : v === 'client_aborted' ? '客户端中断' : v === 'failed' ? '失败' : v} />
        },
      },
      {
        title: '模型',
        dataIndex: 'model',
        ellipsis: true,
        render: (v: string) => <span className="mono">{v || '—'}</span>,
      },
      {
        title: '账号',
        dataIndex: 'account',
        width: 150,
        ellipsis: true,
        render: (v: string) => <span className="mono">{v ? v.slice(0, 8) : '—'}</span>,
      },
      {
        title: 'Token',
        dataIndex: 'total_tokens',
        width: 96,
        align: 'right',
        sorter: (a, b) => a.total_tokens - b.total_tokens,
        render: (v: number) => tokens(v),
      },
      {
        title: '缓存',
        dataIndex: 'cached_tokens',
        width: 80,
        align: 'right',
        render: (v: number, r) => pct(r.prompt_tokens ? (v * 100) / r.prompt_tokens : 0, 0),
      },
      { title: '耗时', dataIndex: 'elapsed_ms', width: 90, align: 'right', render: (v: number) => ms(v) },
      { title: '首字', dataIndex: 'ttft_ms', width: 90, align: 'right', render: (v?: number) => ms(v) },
      { title: '速度', dataIndex: 'tokens_per_sec', width: 86, align: 'right', render: (v: number) => (v ? v.toFixed(1) + ' t/s' : '—') },
      { title: '积分', dataIndex: 'credit', width: 80, align: 'right', render: (v: number) => credit(v) },
    ],
    [],
  )

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            请求记录
          </Typography.Title>
          <Typography.Text type="secondary">最近的网关请求；点一行看 token、耗时、缓存与错误详情。</Typography.Text>
        </div>
        <Space wrap>
          <Select value={range} onChange={setRange} options={RANGES} style={{ width: 110 }} />
          <Segmented
            value={outcome}
            onChange={(v) => setOutcome(v as typeof outcome)}
            options={[
              { value: 'all', label: '全部' },
              { value: 'ok', label: '成功' },
              { value: 'failed', label: '失败' },
              { value: 'aborted', label: '中断' },
            ]}
          />
          <Select
            value={realm}
            onChange={setRealm}
            style={{ width: 126 }}
            options={[
              { value: '', label: '全部区域' },
              { value: 'intl', label: '国际版' },
              { value: 'cn', label: '国内版' },
            ]}
          />
          <Input allowClear prefix={<Search size={14} />} placeholder="模型" value={model} onChange={(e) => setModel(e.target.value)} style={{ width: 180 }} />
          <Input allowClear placeholder="账号 UID" value={account} onChange={(e) => setAccount(e.target.value)} style={{ width: 220 }} />
          <Button icon={<Download size={14} />} onClick={exportCSV} disabled={filtered.length === 0}>
            导出 CSV
          </Button>
          <Button icon={<RefreshCw size={14} />} onClick={() => q.refetch()}>
            刷新
          </Button>
        </Space>
      </div>

      <Card styles={{ body: { padding: isMobile ? 12 : 0 } }}>
        {isMobile ? (
          <Space direction="vertical" size={8} style={{ width: '100%' }}>
            {filtered.slice(0, 50).map((r) => (
              <Card key={r.id} size="small" onClick={() => setActive(r)} style={{ cursor: 'pointer' }}>
                <Space style={{ width: '100%', justifyContent: 'space-between' }}>
                  <span className="mono" style={{ fontSize: 12 }}>
                    {new Date(r.at).toLocaleString()}
                  </span>
                  <Badge status={r.outcome === 'completed' || !r.outcome ? 'success' : 'error'} text={r.outcome === 'completed' || !r.outcome ? '成功' : r.outcome === 'failed' ? '失败' : r.outcome} />
                </Space>
                <div style={{ marginTop: 6 }}>
                  <span className="mono">{r.model || '—'}</span>
                  <Typography.Text type="secondary" style={{ marginLeft: 8 }}>
                    {r.account ? r.account.slice(0, 8) : '—'}
                  </Typography.Text>
                </div>
                <Space wrap size={[12, 2]} style={{ marginTop: 4 }}>
                  <Typography.Text type="secondary">{tokens(r.total_tokens)} tokens</Typography.Text>
                  <Typography.Text type="secondary">{ms(r.elapsed_ms)}</Typography.Text>
                  <Typography.Text type="secondary">缓存 {pct(r.prompt_tokens ? (r.cached_tokens * 100) / r.prompt_tokens : 0, 0)}</Typography.Text>
                </Space>
              </Card>
            ))}
            {filtered.length === 0 && <Typography.Text type="secondary">这个范围里没有请求。</Typography.Text>}
            {filtered.length > 50 && <Typography.Text type="secondary">仅显示最近 50 条，更多请用筛选。</Typography.Text>}
          </Space>
        ) : (
          <Table<UsageRequest>
            rowKey="id"
            size="middle"
            columns={columns}
            dataSource={filtered}
            loading={q.isFetching}
            onRow={(r) => ({ onClick: () => setActive(r), style: { cursor: 'pointer' } })}
            pagination={{ pageSize: 20, showSizeChanger: false, showTotal: (n) => `共 ${num(n)} 条（最多 200）` }}
            locale={{ emptyText: '这个范围里没有请求' }}
            scroll={{ x: 1180 }}
          />
        )}
      </Card>

      <Drawer open={!!active} width={460} onClose={() => setActive(null)} title={active ? `请求 #${active.id}` : ''} extra={active && <Badge status={active.outcome === 'completed' || !active.outcome ? 'success' : 'error'} text={active.outcome || 'completed'} />}>
        {active && (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            {active.error && <Typography.Text type="danger">{active.error}</Typography.Text>}
            <Descriptions
              size="small"
              column={1}
              items={[
                { key: 'time', label: '时间', children: new Date(active.at).toLocaleString() },
                { key: 'model', label: '模型', children: <span className="mono">{active.model || '—'}</span> },
                { key: 'account', label: '账号', children: <span className="mono">{active.account || '—'}</span> },
                { key: 'realm', label: '区域', children: realmLabel[active.realm] ?? (active.realm || '—') },
                { key: 'key', label: 'API Key', children: <span className="mono">{active.key || '（未使用 Key）'}</span> },
                { key: 'stream', label: '流式', children: active.stream ? '是' : '否' },
                { key: 'status', label: 'HTTP 状态', children: String(active.status || '—') },
                { key: 'prompt', label: '输入 Token', children: num(active.prompt_tokens) },
                { key: 'completion', label: '输出 Token', children: num(active.completion_tokens) },
                { key: 'reasoning', label: '推理 Token', children: num(active.reasoning_tokens) },
                { key: 'cached', label: '缓存命中', children: `${num(active.cached_tokens)}（${pct(active.prompt_tokens ? (active.cached_tokens * 100) / active.prompt_tokens : 0, 1)}）` },
                { key: 'total', label: '总 Token', children: num(active.total_tokens) },
                { key: 'credit', label: '积分', children: credit(active.credit) },
                { key: 'elapsed', label: '总耗时', children: ms(active.elapsed_ms) },
                { key: 'ttft', label: '首字延迟', children: ms(active.ttft_ms) },
                { key: 'gen', label: '生成耗时', children: ms(active.gen_ms) },
                { key: 'held', label: '排队等待', children: ms(active.held_ms) },
                { key: 'tps', label: '生成速度', children: active.tokens_per_sec ? active.tokens_per_sec.toFixed(1) + ' t/s' : '—' },
                { key: 'effort', label: '思考档位', children: active.reasoning_effort || '—' },
                { key: 'estimated', label: '用量来源', children: active.usage_estimated ? <Tag color="warning">估算</Tag> : '上游回报' },
              ]}
            />
          </Space>
        )}
      </Drawer>
    </div>
  )
}
