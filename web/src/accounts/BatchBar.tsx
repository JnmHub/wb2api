import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Card, Dropdown, Progress, Space, Tag, Typography } from 'antd'
import { ChevronUp } from 'lucide-react'
import { ApiError, del, get, post, type Job } from '../api'
import { num } from '../format'
import { Confirm, PromptDialog } from '../components/Modal'
import type { Filters } from './AccountsPage'

export interface Selection {
  uids: Set<string>
  allMatching: boolean
}

type Dialog =
  | { kind: 'weight' }
  | { kind: 'note' }
  | { kind: 'proxy' }
  | { kind: 'product' }
  | { kind: 'confirm'; action: string; title: string; text: string }
  | null

/** The request body that targets the current selection. */
export function targetOf(sel: Selection, f: Filters) {
  if (sel.allMatching) {
    const filter: Record<string, string> = {}
    if (f.realm) filter.realm = f.realm
    if (f.status) filter.status = f.status
    if (f.q) filter.q = f.q
    return { filter }
  }
  return { uids: [...sel.uids] }
}

const MORE_ACTIONS = [
  { key: 'set_weight', label: '设置权重…' },
  { key: 'set_note', label: '设置备注…' },
  { key: 'set_proxy_slot', label: '绑定代理槽…' },
  { key: 'set_product', label: '切换出站身份…' },
  { key: 'refresh_token', label: '刷新 Token' },
  { key: 'refresh_credits', label: '刷新积分' },
  { key: 'test', label: '测试账号' },
  { key: 'checkin', label: '签到（仅国内版）' },
  { key: 'daily_chat', label: '每日活跃（仅国际版）' },
  { key: 'clear_cooldown', label: '清除冷却' },
  { key: 'clear_restriction', label: '解除风控…', danger: true },
  { key: 'delete', label: '删除账号…', danger: true },
]

export function BatchBar(props: {
  selection: Selection
  total: number
  filters: Filters
  onClear: () => void
  onSelectAllMatching: () => void
  onExport?: () => void
}) {
  const { selection, total, filters } = props
  const qc = useQueryClient()
  const [dialog, setDialog] = useState<Dialog>(null)
  const [jobId, setJobId] = useState<string | null>(null)
  const [error, setError] = useState('')
  const n = selection.allMatching ? total : selection.uids.size

  const run = async (action: string, params?: Record<string, unknown>, confirm?: boolean) => {
    setError('')
    try {
      const job = await post<Job>('/api/v2/accounts/batch', { action, ...targetOf(selection, filters), params, confirm })
      qc.setQueryData(['job', job.id], job)
      setJobId(job.id)
      setDialog(null)
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e))
      setDialog(null)
    }
  }

  const more = (key: string) => {
    if (key === 'set_weight') return setDialog({ kind: 'weight' })
    if (key === 'set_note') return setDialog({ kind: 'note' })
    if (key === 'set_proxy_slot') return setDialog({ kind: 'proxy' })
    if (key === 'set_product') return setDialog({ kind: 'product' })
    if (key === 'clear_restriction')
      return setDialog({
        kind: 'confirm',
        action: 'clear_restriction',
        title: '解除风控停放',
        text: '仅在确认上游已解除限制（例如申诉通过）后再这样做；否则会把受限设备上的账号重新放回池中。',
      })
    if (key === 'delete')
      return setDialog({
        kind: 'confirm',
        action: 'delete',
        title: `删除 ${num(n)} 个账号`,
        text: '账号文件会被永久删除，无法恢复。',
      })
    void run(key)
  }

  return (
    <>
      <Card size="small" className="batchbar" role="region" aria-label="批量操作">
        <Space wrap size={[8, 6]}>
          <Tag color="processing">已选 {num(n)}</Tag>
          {!selection.allMatching && total > selection.uids.size && (
            <Button type="link" size="small" onClick={props.onSelectAllMatching}>
              选择全部 {num(total)} 个符合筛选的账号
            </Button>
          )}
          <Button size="small" onClick={() => run('enable')}>
            启用
          </Button>
          <Button size="small" onClick={() => run('disable')}>
            停用
          </Button>
          {props.onExport && (
            <Button size="small" onClick={props.onExport}>
              导出选中
            </Button>
          )}
          <Dropdown
            trigger={['click']}
            menu={{
              items: MORE_ACTIONS.map((a) => ({ key: a.key, label: a.label, danger: a.danger })),
              onClick: ({ key }) => more(key),
            }}
          >
            <Button size="small">
              更多操作 <ChevronUp size={13} />
            </Button>
          </Dropdown>
          <Button size="small" type="text" onClick={props.onClear}>
            取消选择
          </Button>
          {error && <Typography.Text type="danger">{error}</Typography.Text>}
        </Space>
      </Card>

      {dialog?.kind === 'weight' && (
        <PromptDialog
          title="设置权重"
          label="权重（1–100，1 = 不干预）"
          initial="1"
          type="number"
          onCancel={() => setDialog(null)}
          onOk={(v) => run('set_weight', { weight: Number(v) })}
        />
      )}
      {dialog?.kind === 'note' && (
        <PromptDialog title="设置备注" label="备注（最多 200 字，留空即清除）" initial="" onCancel={() => setDialog(null)} onOk={(v) => run('set_note', { note: v })} />
      )}
      {dialog?.kind === 'proxy' && (
        <PromptDialog
          title="绑定代理槽"
          label="代理槽名称（留空即解除绑定）"
          initial=""
          onCancel={() => setDialog(null)}
          onOk={(v) => run('set_proxy_slot', { proxy_slot: v })}
        />
      )}
      {dialog?.kind === 'product' && (
        <PromptDialog
          title="出站身份"
          label="workbuddy（桌面端）/ vscode / cli"
          initial="workbuddy"
          onCancel={() => setDialog(null)}
          onOk={(v) => run('set_product', { product: v })}
        />
      )}
      {dialog?.kind === 'confirm' && (
        <Confirm title={dialog.title} text={dialog.text} onCancel={() => setDialog(null)} onOk={() => run(dialog.action, undefined, true)} />
      )}
      {jobId && <JobToast id={jobId} onClose={() => setJobId(null)} />}
    </>
  )
}

const kindLabel: Record<string, string> = {
  enable: '启用',
  disable: '停用',
  set_weight: '设置权重',
  set_note: '设置备注',
  set_proxy_slot: '绑定代理槽',
  set_product: '切换出站身份',
  refresh_token: '刷新 Token',
  clear_cooldown: '清除冷却',
  clear_restriction: '解除风控',
  delete: '删除',
  refresh_credits: '刷新积分',
  checkin: '签到',
  daily_chat: '每日活跃',
  report_activity: '活跃上报',
  sync_profile: '同步昵称',
  test: '测试账号',
}

/** Progress of a background batch job (also fed by the event stream). */
export function JobToast({ id, onClose }: { id: string; onClose: () => void }) {
  const qc = useQueryClient()
  const job = useQuery({
    queryKey: ['job', id],
    queryFn: () => get<Job>(`/api/v2/jobs/${id}`),
    refetchInterval: (q) => (q.state.data?.state === 'running' ? 800 : false),
  })
  const j = job.data
  useEffect(() => {
    if (j && j.state !== 'running') {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['accounts-summary'] })
    }
  }, [j?.state, qc]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!j) return null
  const pct = j.total ? Math.round((j.done / j.total) * 100) : 100
  return (
    <Card size="small" className="jobtoast" role="status">
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <b>{kindLabel[j.kind] ?? j.kind}</b>
        <Typography.Text type="secondary">
          {num(j.done)} / {num(j.total)}
          {j.failed ? ` · 失败 ${num(j.failed)}` : ''}
        </Typography.Text>
        {j.state === 'running' ? (
          <Button size="small" onClick={() => del(`/api/v2/jobs/${id}`)}>
            取消
          </Button>
        ) : (
          <Button size="small" onClick={onClose}>
            关闭
          </Button>
        )}
      </Space>
      <Progress percent={pct} size="small" status={j.failed ? 'exception' : j.state === 'running' ? 'active' : 'success'} />
      {j.state === 'cancelled' && <Typography.Text type="secondary">已取消</Typography.Text>}
      {j.state === 'done' && !j.failed && <Typography.Text type="success">完成</Typography.Text>}
      {j.errors && j.errors.length > 0 && (
        <div className="dim mono" style={{ maxHeight: 90, overflow: 'auto' }}>
          {j.errors.slice(0, 5).map((e) => (
            <div key={e.item}>
              {e.item.slice(0, 10)}: {e.error}
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}
