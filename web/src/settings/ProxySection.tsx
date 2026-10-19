import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Button, Card, Input, Space, Switch, Tag, Typography } from 'antd'
import { Plus, Trash2 } from 'lucide-react'
import { put, type ProxySlot, type SettingsView } from '../api'

type Row = Pick<ProxySlot, 'id' | 'name' | 'url' | 'enabled'> & Partial<ProxySlot>

export function ProxySection({ s }: { s: SettingsView }) {
  const qc = useQueryClient()
  const [rows, setRows] = useState<Row[]>(s.proxy_slots.map((x) => ({ ...x })))
  const [dirty, setDirty] = useState(false)
  const save = useMutation({
    mutationFn: () => put<{ slots: ProxySlot[] }>('/api/v2/proxy-slots', { slots: rows.filter((r) => r.url.trim()) }),
    onSuccess: (r) => {
      setRows(r.slots.map((x) => ({ ...x })))
      setDirty(false)
      qc.invalidateQueries({ queryKey: ['settings'] })
    },
  })
  const edit = (i: number, patch: Partial<Row>) => {
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)))
    setDirty(true)
  }
  return (
    <Card
      title="代理槽"
      extra={
        <Space>
          {dirty && <Tag color="warning">未保存</Tag>}
          <Button
            size="small"
            icon={<Plus size={14} />}
            onClick={() => {
              setRows((rs) => [...rs, { id: '', name: '', url: '', enabled: true }])
              setDirty(true)
            }}
          >
            添加
          </Button>
          <Button size="small" type="primary" disabled={!dirty} loading={save.isPending} onClick={() => save.mutate()}>
            保存代理槽
          </Button>
        </Space>
      }
    >
      <Typography.Paragraph type="secondary">
        给账号绑定出口代理（http / https / socks5）。账号在“账号详情”或批量操作里选择代理槽编号绑定；槽位编号只增不回收，删除一个槽不会让账号悄悄换到别的出口。
      </Typography.Paragraph>
      {rows.length === 0 && <Typography.Text type="secondary">还没有代理槽，所有账号直连。</Typography.Text>}
      <Space direction="vertical" size={8} style={{ width: '100%' }}>
        {rows.map((r, i) => (
          <Space.Compact key={r.id || 'new' + i} block>
            <span className="mono dim" style={{ width: 40, textAlign: 'center', lineHeight: '32px' }}>
              {r.id || '新'}
            </span>
            <Input style={{ width: 180 }} placeholder="名称" value={r.name} onChange={(e) => edit(i, { name: e.target.value })} />
            <Input
              className="mono"
              style={{ flex: 1 }}
              placeholder="http://host:port 或 socks5://host:port"
              value={r.url}
              onChange={(e) => edit(i, { url: e.target.value })}
            />
            <span style={{ padding: '0 8px', lineHeight: '32px' }}>
              <Switch size="small" checked={r.enabled} onChange={(v) => edit(i, { enabled: v })} aria-label="启用" />
            </span>
            <Button danger icon={<Trash2 size={14} />} aria-label="删除代理槽" onClick={() => {
              setRows((rs) => rs.filter((_, j) => j !== i))
              setDirty(true)
            }} />
          </Space.Compact>
        ))}
      </Space>
      {save.isError && <Typography.Text type="danger">{String(save.error)}</Typography.Text>}
    </Card>
  )
}
