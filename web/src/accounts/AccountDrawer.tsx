import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Button,
  Descriptions,
  Divider,
  Drawer,
  Input,
  InputNumber,
  Popconfirm,
  Select,
  Space,
  Switch,
  Tag,
  Typography,
} from 'antd'
import { del, get, patch, post, type Account, type ActionResult, type Job } from '../api'
import { applyPatch } from '../events'
import { credit, num, realmLabel, span, statusLabel, tokens, until } from '../format'
import { JobToast } from './BatchBar'

const actionLabel: Record<string, string> = {
  credits: '刷新积分',
  checkin: '签到',
  'report-activity': '活跃上报',
  'daily-chat': '每日活跃',
  'daily-chat-web': '网页活跃',
  'sync-profile': '同步昵称',
  test: '测试账号',
}

const PRODUCTS = [
  { value: 'workbuddy', label: '桌面端 (workbuddy)' },
  { value: 'vscode', label: 'VSCode 扩展' },
  { value: 'cli', label: 'CodeBuddy CLI' },
]

export function AccountDrawer({ uid, onClose }: { uid: string; onClose: () => void }) {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['account', uid], queryFn: () => get<Account>(`/api/v2/accounts/${uid}`) })
  const a = q.data
  const [form, setForm] = useState({ enabled: true, tasks: true, weight: 1, note: '', models: '', proxySlot: '', product: 'workbuddy' })
  const [dirty, setDirty] = useState(false)
  const [job, setJob] = useState<string | null>(null)
  const [busy, setBusy] = useState('')
  const [result, setResult] = useState<{ name: string; ok: boolean; text: string } | null>(null)
  const [testModel, setTestModel] = useState('')

  useEffect(() => {
    if (a && !dirty) {
      setForm({
        enabled: a.enabled,
        tasks: a.tasks_enabled,
        weight: a.weight,
        note: a.note,
        models: a.disabled_models.join(', '),
        proxySlot: a.proxy_slot,
        product: a.product,
      })
    }
  }, [a, dirty])

  const save = useMutation({
    mutationFn: () =>
      patch<Account>(`/api/v2/accounts/${uid}`, {
        enabled: form.enabled,
        tasks_enabled: form.tasks,
        weight: Number(form.weight),
        note: form.note,
        disabled_models: form.models.split(/[,\s]+/).filter(Boolean),
        proxy_slot: form.proxySlot,
        product: form.product,
      }),
    onSuccess: (acc) => {
      setDirty(false)
      qc.setQueryData(['account', uid], acc)
      applyPatch(qc, { accounts: [acc] })
    },
  })
  const act = async (action: string, confirm = false) => {
    const j = await post<Job>('/api/v2/accounts/batch', { action, uids: [uid], confirm })
    qc.setQueryData(['job', j.id], j)
    setJob(j.id)
  }
  const runAction = async (name: string) => {
    setBusy(name)
    setResult(null)
    try {
      const body = name === 'test' && testModel.trim() ? { model: testModel.trim() } : {}
      const r = await post<ActionResult>(`/api/v2/accounts/${uid}/actions/${name}`, body)
      const ok = r.ok !== false && !r.error
      let text = ok ? r.reply || r.msg || '成功' : r.error || r.msg || '失败'
      if (r.elapsed_ms) text += ` · ${r.elapsed_ms} ms`
      if (r.status) text += ` · HTTP ${r.status}`
      if (r.restricted) text += ' · 已被上游风控'
      if (r.network) text += ' · 网络不通'
      setResult({ name, ok, text })
      if (ok) {
        qc.invalidateQueries({ queryKey: ['account', uid] })
        qc.invalidateQueries({ queryKey: ['accounts'] })
        qc.invalidateQueries({ queryKey: ['accounts-summary'] })
      }
    } catch (e) {
      setResult({ name, ok: false, text: (e as Error).message })
    } finally {
      setBusy('')
    }
  }
  const remove = useMutation({
    mutationFn: () => del(`/api/v2/accounts/${uid}`),
    onSuccess: () => {
      applyPatch(qc, { removed: [uid] })
      onClose()
    },
  })
  const set = <K extends keyof typeof form>(k: K, v: (typeof form)[K]) => {
    setForm((f) => ({ ...f, [k]: v }))
    setDirty(true)
  }

  const statusColor = a?.status === 'ready' ? 'success' : a?.status === 'cooling' || a?.status === 'expiring' ? 'warning' : 'error'

  return (
    <>
      <Drawer
        open
        width={440}
        onClose={onClose}
        destroyOnHidden={false}
        title={
          <div style={{ minWidth: 0 }}>
            <b>{a?.nickname ?? '账号详情'}</b>
            <div className="mono dim" style={{ fontSize: 11 }}>
              {uid}
            </div>
          </div>
        }
        extra={a && <Tag color={statusColor}>{statusLabel[a.status] ?? a.status}</Tag>}
        footer={
          <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
            {save.isError && <Typography.Text type="danger">{String(save.error)}</Typography.Text>}
            <Button onClick={onClose}>关闭</Button>
            <Button type="primary" disabled={!dirty} loading={save.isPending} onClick={() => save.mutate()}>
              保存
            </Button>
          </Space>
        }
      >
        {q.isError && <Alert type="error" showIcon message={String(q.error)} />}
        {a && (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Space>
              <Switch size="small" checked={form.enabled} onChange={(v) => set('enabled', v)} />
              <Typography.Text>参与调度</Typography.Text>
            </Space>
            <Space>
              <Switch size="small" checked={form.tasks} onChange={(v) => set('tasks', v)} />
              <Typography.Text>参与定时任务（签到 / 活跃上报 / 每日对话 / 保活）</Typography.Text>
            </Space>
            <div>
              <Typography.Text type="secondary">权重（1–100）</Typography.Text>
              <InputNumber min={1} max={100} style={{ width: '100%' }} value={form.weight} onChange={(v) => set('weight', v ?? 1)} />
            </div>
            <div>
              <Typography.Text type="secondary">备注</Typography.Text>
              <Input maxLength={200} value={form.note} onChange={(e) => set('note', e.target.value)} />
            </div>
            <div>
              <Typography.Text type="secondary">禁用的模型（逗号分隔，精确匹配）</Typography.Text>
              <Input value={form.models} onChange={(e) => set('models', e.target.value)} placeholder="gpt-5.5, grok-4.7" />
            </div>
            <div>
              <Typography.Text type="secondary">代理槽</Typography.Text>
              <Input value={form.proxySlot} onChange={(e) => set('proxySlot', e.target.value)} />
            </div>
            <div>
              <Typography.Text type="secondary">出站身份</Typography.Text>
              <Select style={{ width: '100%' }} value={form.product} onChange={(v) => set('product', v)} options={PRODUCTS} />
            </div>

            <Divider style={{ margin: '4px 0' }} />
            <Descriptions
              size="small"
              column={1}
              items={[
                { key: 'realm', label: '区域', children: realmLabel[a.realm] ?? a.realm },
                { key: 'expires', label: 'Token 有效期', children: until(a.expires_at) },
                { key: 'today', label: '今日', children: `${num(a.today.requests)} 次 · ${tokens(a.today.tokens)} tokens` },
                ...(a.credits && (a.credits.remain != null || a.credits.total != null)
                  ? [{ key: 'credits', label: '积分', children: `${credit(a.credits.remain)} / ${credit(a.credits.total)}` }]
                  : []),
                { key: 'inflight', label: '进行中', children: String(a.inflight) },
                ...(a.cooldown_for ? [{ key: 'cool', label: '冷却', children: `剩余 ${span(a.cooldown_for)}` }] : []),
                ...(a.unavailable_reason ? [{ key: 'reason', label: '当前不可用', children: <Typography.Text type="warning">{a.unavailable_reason}</Typography.Text> }] : []),
                ...(a.restricted_for ? [{ key: 'restricted', label: '风控限制', children: `剩余 ${span(a.restricted_for)}` }] : []),
                ...(a.refresh ? [{ key: 'refresh', label: '刷新失败', children: `${a.refresh.failures} 次：${a.refresh.last_error}` }] : []),
                { key: 'machine', label: '机器码', children: <span className="mono">{a.machine_id}</span> },
                { key: 'session', label: '会话 ID', children: <span className="mono">{a.session_id}</span> },
                { key: 'source', label: '来源', children: `${a.source}${a.enterprise_id ? ` · 企业 ${a.enterprise_id}` : ''}` },
              ]}
            />

            {a.model_cooldowns && a.model_cooldowns.length > 0 && (
              <div>
                <Typography.Text type="secondary">受限模型</Typography.Text>
                <div style={{ marginTop: 4 }}>
                  <Space wrap size={4}>
                    {a.model_cooldowns.map((m) => (
                      <Tag key={m.model} color="warning">
                        {m.model} · {span(m.expires_at - Date.now() / 1000)}
                      </Tag>
                    ))}
                  </Space>
                </div>
              </div>
            )}

            <Divider style={{ margin: '4px 0' }}>账号操作</Divider>
            <Space wrap size={[6, 6]}>
              <Button size="small" loading={busy === 'credits'} disabled={!!busy} onClick={() => runAction('credits')}>
                刷新积分
              </Button>
              {a.realm === 'cn' && (
                <Button size="small" loading={busy === 'checkin'} disabled={!!busy} onClick={() => runAction('checkin')}>
                  签到
                </Button>
              )}
              {a.realm === 'cn' && (
                <Button size="small" loading={busy === 'report-activity'} disabled={!!busy} onClick={() => runAction('report-activity')}>
                  活跃上报
                </Button>
              )}
              {a.realm === 'intl' && (
                <Button size="small" loading={busy === 'daily-chat'} disabled={!!busy} onClick={() => runAction('daily-chat')}>
                  每日活跃
                </Button>
              )}
              {a.realm === 'intl' && (
                <Button size="small" loading={busy === 'daily-chat-web'} disabled={!!busy} onClick={() => runAction('daily-chat-web')}>
                  网页活跃
                </Button>
              )}
              <Button size="small" loading={busy === 'sync-profile'} disabled={!!busy} onClick={() => runAction('sync-profile')}>
                同步昵称
              </Button>
              <Button size="small" disabled={!!busy} onClick={() => act('refresh_token')}>
                刷新 Token
              </Button>
              <Button size="small" disabled={!!busy} onClick={() => act('clear_cooldown')}>
                清除冷却
              </Button>
              <Popconfirm title="永久删除这个账号？" okText="删除" cancelText="取消" onConfirm={() => remove.mutate()}>
                <Button size="small" danger>
                  删除账号
                </Button>
              </Popconfirm>
            </Space>
            <Space.Compact style={{ width: '100%' }}>
              <Input placeholder="测试用模型（留空 = 自动选择）" value={testModel} onChange={(e) => setTestModel(e.target.value)} />
              <Button size="small" loading={busy === 'test'} disabled={!!busy} onClick={() => runAction('test')}>
                测试账号
              </Button>
            </Space.Compact>
            {result && (
              <Alert
                type={result.ok ? 'success' : 'error'}
                showIcon
                message={actionLabel[result.name] ?? result.name}
                description={result.text}
              />
            )}
          </Space>
        )}
      </Drawer>
      {job && <JobToast id={job} onClose={() => setJob(null)} />}
    </>
  )
}
