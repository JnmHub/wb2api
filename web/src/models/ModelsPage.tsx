import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button, Card, Input, Segmented, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { RefreshCw, Search } from 'lucide-react'
import { get, type CatalogModel, type ModelCatalogue } from '../api'
import { num, tokens } from '../format'

const SOURCE_LABEL: Record<string, string> = {
  remote: '上游 /v3/config',
  cache: '本机桌面缓存',
  legacy: '宽松解析缓存',
  bundled: '内置快照',
  'not built yet': '尚未加载',
}

/** The model catalogue the gateway advertises on /v1/models. */
export function ModelsPage() {
  const [realm, setRealm] = useState<'intl' | 'cn'>('intl')
  const [q, setQ] = useState('')
  const query = useQuery({
    queryKey: ['models', realm],
    queryFn: () => get<ModelCatalogue>(`/api/v2/models?realm=${realm}`),
    retry: false,
  })
  const rows = useMemo(() => {
    const list = query.data?.models ?? []
    const needle = q.trim().toLowerCase()
    return needle ? list.filter((m) => (m.id + ' ' + (m.name ?? '') + ' ' + (m.description ?? '')).toLowerCase().includes(needle)) : list
  }, [query.data, q])

  const columns: ColumnsType<CatalogModel> = [
    {
      title: '模型',
      dataIndex: 'id',
      render: (_, m) => (
        <div>
          <b className="mono">{m.id}</b>
          {m.name && m.name !== m.id && <div className="dim">{m.name}</div>}
          {m.description && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }} ellipsis={{ tooltip: m.description }}>
              {m.description}
            </Typography.Text>
          )}
        </div>
      ),
    },
    {
      title: '能力',
      width: 220,
      render: (_, m) => (
        <Space wrap size={4}>
          {m.supports_vision && <Tag color="purple">视觉</Tag>}
          {m.supports_tool_calls && <Tag color="blue">工具</Tag>}
          {m.supports_reasoning && <Tag color="cyan">推理</Tag>}
          {!m.supports_vision && !m.supports_tool_calls && !m.supports_reasoning && <Tag>纯文本</Tag>}
        </Space>
      ),
    },
    {
      title: '上下文',
      dataIndex: 'context_length',
      width: 110,
      align: 'right',
      render: (v?: number, m?: CatalogModel) =>
        v ? (
          <span title={m?.context_windows?.length ? `窗口：${m.context_windows.map((x) => tokens(x)).join(' / ')}` : undefined}>{tokens(v)}</span>
        ) : (
          '—'
        ),
    },
    {
      title: '最大输出',
      dataIndex: 'max_output_tokens',
      width: 110,
      align: 'right',
      render: (v?: number) => (v ? tokens(v) : '—'),
    },
    {
      title: '思考档位',
      dataIndex: 'reasoning_efforts',
      width: 190,
      render: (v: CatalogModel['reasoning_efforts']) => {
        const list = Array.isArray(v) ? v : v ? Object.keys(v) : []
        return list.length ? (
          <Space wrap size={4}>
            {list.map((x) => (
              <Tag key={x}>{x}</Tag>
            ))}
          </Space>
        ) : (
          <Typography.Text type="secondary">固定</Typography.Text>
        )
      },
    },
  ]

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            模型
          </Typography.Title>
          <Typography.Text type="secondary">
            {query.data ? `${num(query.data.models.length)} 个模型 · ${SOURCE_LABEL[query.data.source] ?? query.data.source}` : '网关对外宣告的模型目录'}
            {query.data?.fetched_at ? ` · 抓取于 ${new Date(query.data.fetched_at * 1000).toLocaleString()}` : ''}
          </Typography.Text>
        </div>
        <Space wrap>
          <Segmented
            value={realm}
            onChange={(v) => setRealm(v as 'intl' | 'cn')}
            options={[
              { value: 'intl', label: '国际版' },
              { value: 'cn', label: '国内版' },
            ]}
          />
          <Input allowClear prefix={<Search size={14} />} placeholder="搜索模型 / 名称 / 描述" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 260 }} />
          <Button icon={<RefreshCw size={14} />} loading={query.isFetching} onClick={() => query.refetch()}>
            刷新
          </Button>
        </Space>
      </div>

      {query.isError && <Card>{String(query.error)}</Card>}
      <Card styles={{ body: { padding: 0 } }}>
        <Table<CatalogModel>
          rowKey="id"
          size="middle"
          columns={columns}
          dataSource={rows}
          loading={query.isFetching}
          pagination={{ pageSize: 20, showSizeChanger: false, showTotal: (n) => `共 ${num(n)} 个模型` }}
          locale={{ emptyText: '没有符合条件的模型' }}
          scroll={{ x: 900 }}
        />
      </Card>
    </div>
  )
}
