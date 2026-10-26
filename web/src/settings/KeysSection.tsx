import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Button, Card, Collapse, Input, InputNumber, Modal, Popconfirm, Select, Space, Switch, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { Copy, Pencil, Plus, Trash2 } from 'lucide-react'
import { del, get, patch, post, type KeyView, type SettingsView } from '../api'
import { CopyButton } from '../components/Modal'
import { realmLabel } from '../format'

interface Created extends KeyView {
  key: string
}

export function KeysSection({ s }: { s: SettingsView }) {
  const qc = useQueryClient()
  const refresh = () => qc.invalidateQueries({ queryKey: ['settings'] })
  const [editing, setEditing] = useState<KeyView | 'new' | null>(null)
  const [created, setCreated] = useState<Created | null>(null)
  const [revealed, setRevealed] = useState<Record<string, string>>({})
  const toggle = useMutation({
    mutationFn: (k: KeyView) => patch(`/api/v2/keys/${k.id}`, { enabled: !k.enabled }),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: (k: KeyView) => del(`/api/v2/keys/${k.id}`),
    onSuccess: refresh,
  })
  const live = s.keys.filter((k) => !k.deleted)
  const gone = s.keys.filter((k) => k.deleted)

  const reveal = async (k: KeyView) => {
    const r = await get<{ key: string }>(`/api/v2/keys/${k.id}/secret`)
    setRevealed((cur) => ({ ...cur, [k.id]: r.key }))
    try {
      await navigator.clipboard.writeText(r.key)
    } catch {
      /* clipboard may be unavailable outside a secure context */
    }
  }

  const columns: ColumnsType<KeyView> = [
    {
      title: '名称',
      dataIndex: 'name',
      render: (_, k) => (
        <div>
          <b>{k.name}</b>
          <div className="mono dim" style={{ fontSize: 12 }}>
            {revealed[k.id] ?? k.key_preview}
          </div>
        </div>
      ),
    },
    {
      title: '出口',
      dataIndex: 'realm',
      width: 110,
      render: (r: KeyView['realm']) =>
        r ? <Tag color={r === 'cn' ? 'volcano' : 'geekblue'}>{realmLabel[r]}</Tag> : <Typography.Text type="secondary">跟随面板</Typography.Text>,
    },
    {
      title: '模型',
      dataIndex: 'models',
      responsive: ['md'],
      render: (m: string[]) =>
        m.length ? (
          <Typography.Text ellipsis={{ tooltip: m.join('\n') }} style={{ maxWidth: 260 }}>
            {m.length} 条规则
          </Typography.Text>
        ) : (
          <Typography.Text type="secondary">不限模型</Typography.Text>
        ),
    },
    {
      title: 'TPS',
      dataIndex: 'tps_limit',
      width: 96,
      responsive: ['lg'],
      render: (v: number) => (v < 0 ? <Typography.Text type="secondary">关闭</Typography.Text> : v === 0 ? <Typography.Text type="secondary">跟随</Typography.Text> : `${v} t/s`),
    },
    {
      title: '并发',
      dataIndex: 'concurrency_limit',
      width: 80,
      responsive: ['lg'],
      render: (v: number) => (v > 0 ? v : <Typography.Text type="secondary">不限</Typography.Text>),
    },
    {
      title: '启用',
      dataIndex: 'enabled',
      width: 80,
      render: (_, k) => <Switch size="small" checked={k.enabled} onChange={() => toggle.mutate(k)} aria-label={`${k.name} 启用`} />,
    },
    {
      title: '',
      width: 150,
      render: (_, k) => (
        <Space size={4}>
          <Button type="text" size="small" icon={<Copy size={14} />} onClick={() => reveal(k)} aria-label="复制 Key" />
          <Button type="text" size="small" icon={<Pencil size={14} />} onClick={() => setEditing(k)} aria-label="编辑" />
          <Popconfirm title={`删除 Key「${k.name}」`} description="Key 立即失效，不可恢复；历史用量仍归属在这个名字下。" okText="删除" cancelText="取消" onConfirm={() => remove.mutate(k)}>
            <Button type="text" size="small" danger icon={<Trash2 size={14} />} aria-label="删除" />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Card
      title="API Key"
      extra={
        <Button type="primary" size="small" icon={<Plus size={14} />} onClick={() => setEditing('new')}>
          新建 Key
        </Button>
      }
      styles={{ body: { padding: 0 } }}
    >
      {live.length === 0 && (
        <Typography.Paragraph type="warning" style={{ padding: 16, margin: 0 }}>
          还没有 Key：网关此刻不校验调用方，任何能访问端口的客户端都能使用你的账号。建议先建一个。
        </Typography.Paragraph>
      )}
      <Table<KeyView> rowKey="id" size="small" columns={columns} dataSource={live} pagination={false} />
      {gone.length > 0 && (
        <Collapse
          ghost
          items={[
            {
              key: 'gone',
              label: `已删除的 Key（${gone.length}，保留以归属历史用量）`,
              children: gone.map((k) => (
                <div key={k.id} className="mono dim">
                  {k.name} · {k.id} · 删除于 {k.deleted_at}
                </div>
              )),
            },
          ]}
        />
      )}

      {editing && (
        <KeyEditor
          initial={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(c) => {
            setEditing(null)
            if (c) setCreated(c)
            refresh()
          }}
        />
      )}
      <Modal
        title="新 Key 已创建"
        open={!!created}
        onCancel={() => setCreated(null)}
        footer={[
          created && <CopyButton key="copy" text={created.key} />,
          <Button key="ok" type="primary" onClick={() => setCreated(null)}>
            完成
          </Button>,
        ]}
      >
        <Typography.Paragraph type="secondary">完整的 Key 只在这里显示一次（之后仍可在列表里“复制 Key”）。</Typography.Paragraph>
        <Typography.Paragraph copyable className="mono" data-testid="new-key">
          {created?.key}
        </Typography.Paragraph>
      </Modal>
    </Card>
  )
}

function KeyEditor(props: { initial: KeyView | null; onClose: () => void; onSaved: (created: Created | null) => void }) {
  const k = props.initial
  const [name, setName] = useState(k?.name ?? '')
  const [realm, setRealm] = useState<string>(k?.realm ?? '')
  const [models, setModels] = useState((k?.models ?? []).join('\n'))
  const [accounts, setAccounts] = useState((k?.accounts ?? []).join('\n'))
  const [tps, setTps] = useState<number>(k?.tps_limit ?? 0)
  const [conc, setConc] = useState<number>(k?.concurrency_limit ?? 0)
  const [secret, setSecret] = useState('')
  const m = useMutation({
    mutationFn: async () => {
      const body: Record<string, unknown> = {
        name,
        realm,
        models: models.split(/[\s,]+/).filter(Boolean),
        accounts: accounts.split(/[\s,]+/).filter(Boolean),
        tps_limit: tps,
        concurrency_limit: conc,
      }
      if (!k) {
        if (secret.trim()) body.key = secret.trim()
        return { created: await post<Created>('/api/v2/keys', body) }
      }
      await patch(`/api/v2/keys/${k.id}`, body)
      return { created: null }
    },
    onSuccess: (r) => props.onSaved(r.created),
  })
  return (
    <Modal
      title={k ? '编辑 Key' : '新建 Key'}
      open
      onCancel={props.onClose}
      okText="保存"
      cancelText="取消"
      confirmLoading={m.isPending}
      onOk={() => m.mutate()}
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <div>
          <Typography.Text type="secondary">名称</Typography.Text>
          <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：cursor / ci" />
        </div>
        <div>
          <Typography.Text type="secondary">绑定出口</Typography.Text>
          <Select
            style={{ width: '100%' }}
            value={realm}
            onChange={setRealm}
            options={[
              { value: '', label: '跟随面板切换' },
              { value: 'intl', label: '国际版' },
              { value: 'cn', label: '国内版' },
            ]}
          />
        </div>
        {!k && (
          <div>
            <Typography.Text type="secondary">自定义 Key（留空 = 自动生成）</Typography.Text>
            <Input.Password className="mono" value={secret} onChange={(e) => setSecret(e.target.value)} placeholder="至少 4 个字符" />
          </div>
        )}
        <div>
          <Typography.Text type="secondary">流式输出 TPS 上限（0 = 跟随全局，-1 = 此 Key 关闭，{'>'}0 = 覆盖）</Typography.Text>
          <InputNumber style={{ width: '100%' }} min={-1} max={1000} step={0.5} value={tps} onChange={(v) => setTps(v ?? 0)} />
        </div>
        <div>
          <Typography.Text type="secondary">并发上限（0 = 不限；多副本经 Redis 共享，无 Redis 时本机生效）</Typography.Text>
          <InputNumber style={{ width: '100%' }} min={0} max={100000} step={1} value={conc} onChange={(v) => setConc(v ?? 0)} />
        </div>
        <div>
          <Typography.Text type="secondary">允许的模型（每行一条，支持 * 通配；留空 = 不限）</Typography.Text>
          <Input.TextArea className="mono" rows={4} value={models} onChange={(e) => setModels(e.target.value)} placeholder={'glm-*\ndeepseek-v4.1-flash'} />
        </div>
        <div>
          <Typography.Text type="secondary">限定可用账号（每行一个 UID 或短 ID；留空 = 不限，会取消该 Key 的会话粘性）</Typography.Text>
          <Input.TextArea className="mono" rows={3} value={accounts} onChange={(e) => setAccounts(e.target.value)} placeholder={'u00000001…'} />
        </div>
        {m.isError && <Typography.Text type="danger">{String(m.error)}</Typography.Text>}
      </Space>
    </Modal>
  )
}
