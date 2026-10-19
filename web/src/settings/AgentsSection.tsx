import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Col, Input, Row, Space, Tag, Typography } from 'antd'
import { get, post, type AgentClient, type AgentsView } from '../api'

/**
 * Point installed AI CLIs at this gateway. Every file is backed up before it is
 * touched and restore puts the pre-integration bytes back. Writes are only
 * accepted from loopback, so the panel must be opened on the host itself.
 */
export function AgentsSection() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['agents'], queryFn: () => get<AgentsView>('/api/v2/agents'), retry: false })
  const [key, setKey] = useState('')
  const [model, setModel] = useState('')
  const apply = useMutation({
    mutationFn: (v: { client: string }) =>
      post<{ clients: AgentClient[] }>('/api/v2/agents/apply', {
        client: v.client,
        base_url: q.data?.base_url,
        api_key: key,
        model,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['agents'] }),
  })
  const restore = useMutation({
    mutationFn: (client: string) => post<{ clients: AgentClient[] }>('/api/v2/agents/restore', { client }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['agents'] }),
  })
  const list = q.data?.clients ?? []
  return (
    <Card title="智能体配置">
      <Typography.Paragraph type="secondary">
        把 Claude Code / Codex / OpenCode / DSH / Crush / ZCode 指向本网关。接管前会备份原文件，恢复时按备份还原；只接受本机请求。ZCode 会边运行边重写配置，接管前请先退出它。
      </Typography.Paragraph>
      <Row gutter={[12, 8]} style={{ marginBottom: 12 }}>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">网关地址</Typography.Text>
          <Input value={q.data?.base_url ?? ''} readOnly aria-label="网关地址" />
        </Col>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">智能体 API Key</Typography.Text>
          <Input.Password value={key} onChange={(e) => setKey(e.target.value)} placeholder="sk-…" aria-label="智能体使用的 API Key" />
        </Col>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">默认模型（可选）</Typography.Text>
          <Input className="mono" value={model} onChange={(e) => setModel(e.target.value)} placeholder="glm-5.3-flash" aria-label="智能体默认模型" />
        </Col>
      </Row>
      {q.data && !q.data.loopback && <Alert type="warning" showIcon message="当前不是本机访问，接管/恢复会被服务端拒绝。" style={{ marginBottom: 12 }} />}
      {apply.isError && <Alert type="error" showIcon message={String(apply.error)} style={{ marginBottom: 12 }} />}
      {restore.isError && <Alert type="error" showIcon message={String(restore.error)} style={{ marginBottom: 12 }} />}
      <Space direction="vertical" size={6} style={{ width: '100%' }}>
        {list.map((c) => (
          <div key={c.id} className="catline" style={{ alignItems: 'flex-start' }}>
            <div style={{ minWidth: 220 }}>
              <b>{c.label}</b>
              <div className="dim hint">{c.desc}</div>
              <div className="mono" style={{ fontSize: 11, color: 'var(--ant-color-text-tertiary)', wordBreak: 'break-all' }} title={c.config_paths.join('\n')}>
                {c.config_paths.join(' · ')}
              </div>
            </div>
            <Tag color={c.installed ? 'success' : 'default'}>{c.installed ? '已安装' : '未检测到'}</Tag>
            {c.configured && <Tag color="processing">已接管</Tag>}
            {c.applied?.external_change && <Tag color="warning">接管后被改过</Tag>}
            {c.applied && <Typography.Text type="secondary">模型 {c.applied.model || '默认'}</Typography.Text>}
            <span className="spacer" />
            <Button
              size="small"
              type="primary"
              loading={apply.isPending && apply.variables?.client === c.id}
              disabled={!key.trim() || !q.data?.loopback}
              onClick={() => apply.mutate({ client: c.id })}
            >
              {c.configured ? '重新接管' : '接管'}
            </Button>
            <Button
              size="small"
              loading={restore.isPending && restore.variables === c.id}
              disabled={!c.configured || !q.data?.loopback}
              onClick={() => restore.mutate(c.id)}
            >
              恢复
            </Button>
          </div>
        ))}
      </Space>
      {q.isError && <Alert type="error" showIcon message={String(q.error)} style={{ marginTop: 12 }} />}
    </Card>
  )
}
