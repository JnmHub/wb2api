import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Badge,
  Button,
  Card,
  Col,
  Dropdown,
  Input,
  Row,
  Segmented,
  Select,
  Space,
  Statistic,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { Download, MoreHorizontal, Plus, RefreshCw, Search, Upload, UserPlus } from 'lucide-react'
import { get, post, type Account, type AccountPage, type AccountSummary, type Status } from '../api'
import { credit, debounce, num, realmLabel, statusLabel, tokens, until } from '../format'
import { AccountDrawer } from './AccountDrawer'
import { AddAccountModal } from './AddAccountModal'
import { BatchBar, type Selection } from './BatchBar'
import { DesktopImportModal } from './DesktopImportModal'
import { ExportModal } from './ExportModal'
import { ImportModal } from './ImportModal'

export interface Filters {
  q: string
  realm: '' | 'intl' | 'cn'
  status: Status | ''
  sort: string
  order: 'asc' | 'desc'
}

const defaultFilters: Filters = { q: '', realm: '', status: '', sort: 'default', order: 'asc' }

export function queryString(f: Filters, extra: Record<string, string | number> = {}): string {  const p = new URLSearchParams()
  if (f.q) p.set('q', f.q)
  if (f.realm) p.set('realm', f.realm)
  if (f.status) p.set('status', f.status)
  if (f.sort !== 'default') {
    p.set('sort', f.sort)
    p.set('order', f.order)
  }
  for (const [k, v] of Object.entries(extra)) p.set(k, String(v))
  return p.toString()
}

const PAGE_SIZE = 100

const SORTS = [
  { value: 'default', label: '默认顺序' },
  { value: 'added', label: '新增时间' },
  { value: 'weight', label: '权重' },
  { value: 'credits', label: '剩余积分' },
  { value: 'expires', label: '有效期' },
  { value: 'tokens', label: '今日 Token' },
  { value: 'requests', label: '今日请求' },
]

export function AccountsPage() {
  const qc = useQueryClient()
  const [filters, setFilters] = useState<Filters>(defaultFilters)
  const [qInput, setQInput] = useState('')
  const setQ = useDebouncedSetter((v: string) => setFilters((f) => ({ ...f, q: v })), 250)
  const [selection, setSelection] = useState<Selection>({ uids: new Set(), allMatching: false })
  const [open, setOpen] = useState<string | null>(null)
  const [dialog, setDialog] = useState<'add' | 'import' | 'export' | 'desktop' | null>(null)
  const [page, setPage] = useState(1)

  useEffect(() => {
    setPage(1)
    setSelection({ uids: new Set(), allMatching: false })
  }, [filters])

  const summary = useQuery({
    queryKey: ['accounts-summary'],
    queryFn: () => get<AccountSummary>('/api/v2/accounts/summary'),
    refetchInterval: 30_000,
  })
  const list = useQuery({
    queryKey: ['accounts', filters, page],
    queryFn: () => get<AccountPage>(`/api/v2/accounts?${queryString(filters, { offset: (page - 1) * PAGE_SIZE, limit: PAGE_SIZE })}`),
    placeholderData: (prev) => prev,
    staleTime: 15_000,
  })

  const sum = summary.data
  const count = (s: Status) => sum?.by_status[s] ?? 0
  const items = list.data?.items ?? []
  const total = list.data?.total ?? 0
  const selected = selection.uids
  const rows = useMemo(() => items.map((a) => ({ ...a, key: a.uid })), [items])

  const statusOptions: { value: Status | ''; label: string }[] = [
    { value: '', label: `全部 ${sum?.total ?? 0}` },
    { value: 'ready', label: `可用 ${count('ready')}` },
    { value: 'cooling', label: `冷却 ${count('cooling')}` },
    { value: 'restricted', label: `风控 ${count('restricted')}` },
    { value: 'no_credit', label: `无积分 ${count('no_credit')}` },
    { value: 'need_login', label: `需登录 ${count('need_login')}` },
    { value: 'disabled', label: `停用 ${count('disabled')}` },
  ]

  const columns: ColumnsType<Account> = [
    {
      title: '账号',
      dataIndex: 'uid',
      width: 260,
      render: (_, a) => (
        <div className="acct-cell">
          <span className="acct-avatar">{a.realm === 'cn' ? 'CN' : 'GL'}</span>
          <div>
            <div className="acct-name">
              <a onClick={() => setOpen(a.uid)}>{a.nickname || a.short_id}</a>
              {a.inflight > 0 && <Tag color="processing">进行中 {a.inflight}</Tag>}
            </div>
            <div className="acct-sub mono">{a.short_id}</div>
          </div>
        </div>
      ),
    },
    {
      title: '区域',
      dataIndex: 'realm',
      width: 96,
      render: (r: Account['realm']) => <Tag color={r === 'cn' ? 'volcano' : 'geekblue'}>{realmLabel[r] ?? r}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 120,
      render: (_: unknown, a: Account) => (
        <Badge
          status={a.status === 'ready' ? 'success' : a.status === 'cooling' || a.status === 'expiring' || a.status === 'no_credit' ? 'warning' : a.status === 'restricted' || a.status === 'need_login' ? 'error' : 'default'}
          text={statusLabel[a.status] ?? a.status}
        />
      ),
    },
    { title: '身份', dataIndex: 'product', width: 96 },
    { title: '权重', dataIndex: 'weight', width: 70, align: 'right' },
    {
      title: '今日 Token',
      dataIndex: ['today', 'tokens'],
      width: 110,
      align: 'right',
      render: (_: unknown, a: Account) => <span className="mono">{tokens(a.today.tokens)}</span>,
    },
    {
      title: '积分',
      width: 110,
      align: 'right',
      render: (_: unknown, a: Account) =>
        a.credits?.remain != null ? (
          <span className="mono">{credit(a.credits.remain)}</span>
        ) : (
          <Typography.Text type="secondary">—</Typography.Text>
        ),
    },
    {
      title: '有效期',
      dataIndex: 'expires_at',
      width: 100,
      render: (v: number) => <span className={v && v * 1000 < Date.now() + 86400000 ? 'acct-warn' : 'mono dim'}>{until(v)}</span>,
    },
    {
      title: '',
      key: 'ops',
      width: 56,
      render: (_, a) => (
        <Dropdown
          trigger={['click']}
          menu={{
            items: [
              { key: 'open', label: '详情 / 操作' },
              { key: 'enable', label: '启用', disabled: a.enabled },
              { key: 'disable', label: '停用', disabled: !a.enabled },
            ],
            onClick: ({ key, domEvent }) => {
              domEvent.stopPropagation()
              if (key === 'open') setOpen(a.uid)
              else void batch([a.uid], key)
            },
          }}
        >
          <Button type="text" size="small" icon={<MoreHorizontal size={16} />} aria-label="更多操作" />
        </Dropdown>
      ),
    },
  ]

  async function batch(uids: string[], action: string) {
    await qc.cancelQueries({ queryKey: ['accounts'] })
    await post('/api/v2/accounts/batch', { action, uids })
    await qc.invalidateQueries({ queryKey: ['accounts'] })
    await qc.invalidateQueries({ queryKey: ['accounts-summary'] })
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            账号
          </Typography.Title>
          <Typography.Text type="secondary">
            {num(sum?.total ?? total)} 个账号 · 今日 {tokens(sum?.today.tokens)} tokens · {num(sum?.today.requests)} 次请求
          </Typography.Text>
        </div>
        <Space wrap>
          <Button icon={<RefreshCw size={14} />} onClick={() => qc.invalidateQueries({ queryKey: ['accounts'] })}>
            刷新
          </Button>
          <Button icon={<Upload size={14} />} onClick={() => setDialog('import')}>
            导入
          </Button>
          <Button icon={<Download size={14} />} onClick={() => setDialog('export')}>
            导出
          </Button>
          <Button icon={<UserPlus size={14} />} onClick={() => setDialog('desktop')}>
            桌面导入
          </Button>
          <Button type="primary" icon={<Plus size={14} />} onClick={() => setDialog('add')}>
            添加账号
          </Button>
        </Space>
      </div>

      <Row gutter={12} className="stat-row">
        <StatTile label="全部账号" value={num(sum?.total)} dot="var(--ant-color-text-tertiary)" />
        <StatTile label="可用" value={num(count('ready'))} dot="#10b981" />
        <StatTile label="无积分" value={num(count('no_credit'))} dot="#ef4444" />
        <StatTile label="冷却中" value={num(count('cooling'))} dot="#f59e0b" />
        <StatTile label="风控限制" value={num(count('restricted'))} dot="#ef4444" />
        <StatTile label="需重新登录" value={num(count('need_login'))} dot="#ef4444" />
      </Row>

      <Row gutter={[12, 12]} className="credit-row">
        <Col xs={24} lg={8}>
          <Card size="small" className="credit-card" styles={{ body: { padding: 14 } }}>
            <div className="credit-head">
              <Typography.Text type="secondary">总剩余积分</Typography.Text>
              <Tag>{sum?.keys ?? 0} 个 Key</Tag>
            </div>
            <div className="credit-big">{credit(sum?.credits?.remain ?? 0)}</div>
            <div className="credit-sub">
              可用 <b>{credit(sum?.credits?.available ?? 0)}</b>
            </div>
            <div className="credit-meta">
              今日消耗 {credit(sum?.credits?.today_used ?? 0)} · 7 天到期 {credit(sum?.credits?.expiring_7d ?? 0)} · 0 积分{' '}
              {sum?.credits?.zero ?? 0} 个
            </div>
          </Card>
        </Col>
        {(['intl', 'cn'] as const).map((r) => {
          const c = sum?.by_realm_credits?.[r]
          const current = filters.realm === r
          return (
            <Col xs={24} lg={8} key={r}>
              <Card
                size="small"
                className={'credit-card' + (current ? ' current' : '')}
                styles={{ body: { padding: 14 } }}
                onClick={() => setFilters((f) => ({ ...f, realm: f.realm === r ? '' : r }))}
                style={{ cursor: 'pointer' }}
              >
                <div className="credit-head">
                  <Space size={6}>
                    <b>{realmLabel[r]}</b>
                    {current && <Tag color="processing">当前视图</Tag>}
                  </Space>
                  <Tag>{c?.keys ?? 0} 个 Key</Tag>
                </div>
                <div className="credit-big">{credit(c?.remain ?? 0)}</div>
                <div className="credit-sub">
                  可用 <b>{credit(c?.available ?? 0)}</b>
                </div>
                <div className="credit-meta">
                  可用账号 {c?.ready ?? 0}/{c?.total ?? 0} · 0 积分 {c?.zero ?? 0} 个 · 7 天到期 {credit(c?.expiring_7d ?? 0)}
                </div>
              </Card>
            </Col>
          )
        })}
      </Row>

      <Card className="toolbar-card" styles={{ body: { padding: 12 } }}>
        <Space wrap size={10} style={{ width: '100%' }}>
          <Input
            allowClear
            prefix={<Search size={14} />}
            style={{ width: 280 }}
            placeholder="搜索 UID / 昵称 / 备注 / 代理槽"
            value={qInput}
            onChange={(e) => {
              setQInput(e.target.value)
              setQ(e.target.value)
            }}
          />
          <Select
            value={filters.realm}
            style={{ width: 128 }}
            onChange={(v) => setFilters((f) => ({ ...f, realm: v }))}
            options={[
              { value: '', label: '全部区域' },
              { value: 'intl', label: '国际版' },
              { value: 'cn', label: '国内版' },
            ]}
          />
          <Select
            value={filters.sort}
            style={{ width: 132 }}
            onChange={(v) => setFilters((f) => ({ ...f, sort: v, order: v === 'tokens' || v === 'requests' ? 'desc' : 'asc' }))}
            options={SORTS}
          />
          <span className="spacer" />
          <Segmented
            value={filters.status}
            onChange={(v) => setFilters((f) => ({ ...f, status: v as Status | '' }))}
            options={statusOptions}
          />
        </Space>
      </Card>

      <Card styles={{ body: { padding: 0 } }}>
        <Table<Account>
          rowKey="uid"
          size="middle"
          loading={list.isFetching}
          columns={columns}
          dataSource={rows}
          scroll={{ x: 1080 }}
          rowSelection={{
            selectedRowKeys: items.filter((a) => selected.has(a.uid)).map((a) => a.uid),
            onChange: (keys) => setSelection({ uids: new Set(keys as string[]), allMatching: false }),
          }}
          onRow={(a) => ({ onDoubleClick: () => setOpen(a.uid) })}
          pagination={{
            current: page,
            pageSize: PAGE_SIZE,
            total,
            showSizeChanger: false,
            showTotal: (n) => `共 ${num(n)} 个账号`,
            onChange: setPage,
          }}
          locale={{ emptyText: '没有符合条件的账号' }}
        />
      </Card>

      {open && <AccountDrawer uid={open} onClose={() => setOpen(null)} />}
      {dialog === 'add' && <AddAccountModal onClose={() => setDialog(null)} />}
      {dialog === 'import' && <ImportModal onClose={() => setDialog(null)} />}
      {dialog === 'export' && <ExportModal uids={selection.allMatching ? [] : [...selection.uids]} onClose={() => setDialog(null)} />}
      {dialog === 'desktop' && <DesktopImportModal onClose={() => setDialog(null)} />}

      {(selected.size > 0 || selection.allMatching) && (
        <BatchBar
          selection={selection}
          total={total}
          filters={filters}
          onClear={() => setSelection({ uids: new Set(), allMatching: false })}
          onSelectAllMatching={() => setSelection({ uids: new Set(), allMatching: true })}
          onExport={() => setDialog('export')}
        />
      )}
    </div>
  )
}

function StatTile({ label, value, dot }: { label: string; value: string; dot: string }) {
  return (
    <Col xs={12} sm={8} md={4} flex="1 1 160px">
      <Card styles={{ body: { padding: '12px 16px' } }} className="stat-tile">
        <Statistic
          title={
            <span className="stat-title">
              <i style={{ background: dot }} /> {label}
            </span>
          }
          value={value}
          valueStyle={{ fontSize: 24, fontWeight: 600 }}
        />
      </Card>
    </Col>
  )
}

function useDebouncedSetter<T>(fn: (v: T) => void, wait: number) {
  const ref = useRef(fn)
  ref.current = fn
  return useMemo(() => debounce((v: T) => ref.current(v), wait), [wait])
}
