import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Col, Input, Row, Select, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { Play } from 'lucide-react'
import {
  ApiError,
  get,
  patch,
  post,
  type ActivityList,
  type ActivityRow,
  type GrowthRun,
  type SchedulerStatus,
  type TasksSnapshot,
} from '../api'
import { Toggle } from '../components/Modal'

const taskLabel: Record<string, string> = { checkin: '国内签到', daily_chat: '国际每日活跃', activity_report: '国内活跃上报' }
const triggerLabel: Record<string, string> = {
  scheduler: '定时',
  manual: '手动',
  account_add: '新增账号',
  account_import: '导入账号',
  unknown: '—',
}

/** "9, 21" -> [9, 21]; anything outside 0–23 is dropped. */
export function parseHours(text: string): number[] {
  const seen = new Set<number>()
  for (const part of text.split(/[\s,，、]+/)) {
    if (!/^\d{1,2}$/.test(part)) continue
    const h = Number(part)
    if (h >= 0 && h <= 23) seen.add(h)
  }
  return [...seen].sort((a, b) => a - b)
}

export function TasksPage() {
  return (
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            任务
          </Typography.Title>
          <Typography.Text type="secondary">定时巡检、国内成长任务与活动历史。</Typography.Text>
        </div>
      </div>
      <SchedulerCard />
      <GrowthCard />
      <ActivityCard />
    </div>
  )
}

const taskStatusLabel: Record<string, string> = {
  not_accepted: '未接取',
  accepted: '进行中',
  completed: '待领奖',
  claimed: '已领取',
}

/** CN growth centre: snapshot, the full auto-run, cat travel and the streak keeper. */
function GrowthCard() {
  const qc = useQueryClient()
  const [uid, setUid] = useState('')
  const [output, setOutput] = useState<string[] | null>(null)
  const q = useQuery({
    queryKey: ['tasks-snapshot', uid],
    queryFn: () => get<TasksSnapshot>('/api/v2/tasks' + (uid ? '?uid=' + encodeURIComponent(uid) : '')),
    retry: false,
    refetchInterval: 60_000,
  })
  const act = useMutation({
    mutationFn: (what: 'run' | 'travel' | 'streak-bonus') => post<GrowthRun>('/api/v2/tasks/' + what, { uid }),
    onSuccess: (r) => {
      const lines = r.logs?.length ? r.logs : (r.msg ?? '').split('\n').filter(Boolean)
      if (r.credit_added) lines.unshift(`本轮共获得 ${r.credit_added} 积分`)
      setOutput(lines)
      qc.invalidateQueries({ queryKey: ['tasks-snapshot'] })
    },
    onError: (e) => setOutput([(e as Error).message]),
  })
  const snap = q.data
  if (q.isError) {
    return <Card style={{ marginBottom: 12 }}>{String(q.error)}</Card>
  }
  const travelState: Record<string, string> = { idle: '在家', traveling: '旅行中', arrived: '已归来（待领奖）', unknown: '未知' }
  const columns: ColumnsType<TasksSnapshot['tasks'][number]> = [
    { title: '任务', dataIndex: 'title', ellipsis: true, render: (v: string, t) => v || t.code },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (v: string) => (
        <Tag color={v === 'claimed' ? 'success' : v === 'completed' ? 'warning' : 'processing'}>{taskStatusLabel[v] ?? v}</Tag>
      ),
    },
    { title: '进度', width: 110, align: 'right', render: (_, t) => `${t.current}/${t.target || '—'}` },
    { title: '奖励', dataIndex: 'reward_credit', width: 90, align: 'right', render: (v: number) => `+${v}` },
  ]
  return (
    <Card
      title="国内成长任务"
      style={{ marginBottom: 12 }}
      extra={
        <Space wrap>
          {snap && snap.accounts.length > 1 && (
            <Select
              size="small"
              style={{ width: 160 }}
              value={uid || snap.account.uid}
              onChange={setUid}
              options={snap.accounts.map((a) => ({ value: a.uid, label: a.nickname || a.uid.slice(0, 8) }))}
            />
          )}
          <Button size="small" disabled={act.isPending || !snap} onClick={() => act.mutate('travel')}>
            猫猫旅行
          </Button>
          <Button size="small" disabled={act.isPending || !snap} onClick={() => act.mutate('streak-bonus')}>
            连登管家
          </Button>
          <Button size="small" type="primary" loading={act.isPending} disabled={!snap} onClick={() => act.mutate('run')}>
            全自动跑一轮
          </Button>
        </Space>
      }
      styles={{ body: { padding: snap ? 0 : 16 } }}
    >
      {!snap && <Typography.Text type="secondary">加载中…</Typography.Text>}
      {snap && (
        <>
          <Space wrap size={[16, 6]} style={{ padding: '12px 16px' }}>
            <Typography.Text>连登 {snap.summary.streak_days} 天</Typography.Text>
            <Typography.Text>能量 {snap.summary.energy}</Typography.Text>
            <Typography.Text>猫猫：{travelState[snap.summary.travel.state] ?? snap.summary.travel.state}</Typography.Text>
            {snap.summary.travel.daily_limit_reached && <Tag>今日旅行额度已用完</Tag>}
            {snap.account.enterprise && <Tag color="warning">企业版无成长体系</Tag>}
          </Space>
          <Table<TasksSnapshot['tasks'][number]>
            rowKey="code"
            size="small"
            columns={columns}
            dataSource={snap.tasks}
            pagination={false}
            locale={{ emptyText: '没有可执行的成长任务' }}
          />
          <Typography.Paragraph type="secondary" style={{ padding: '10px 16px 14px', margin: 0 }}>
            全自动跑一轮会接取任务并上报最小事件（每次写入间隔 1 秒），需要桌面端操作的任务会跳过。
          </Typography.Paragraph>
        </>
      )}
      {output && (
        <Alert
          style={{ margin: 12 }}
          type={output.some((l) => /失败|错误/.test(l)) ? 'warning' : 'info'}
          showIcon
          message="执行结果"
          description={output.map((l, i) => (
            <div key={i}>{l}</div>
          ))}
        />
      )}
    </Card>
  )
}

function SchedulerCard() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['scheduler'], queryFn: () => get<SchedulerStatus>('/api/v2/scheduler'), refetchInterval: 5000 })
  const [hours, setHours] = useState<{ checkin: string; keepalive: string } | null>(null)
  const [msg, setMsg] = useState('')
  const update = useMutation({
    mutationFn: (body: Record<string, unknown>) => patch<SchedulerStatus>('/api/v2/scheduler', body),
    onSuccess: (s) => {
      qc.setQueryData(['scheduler'], s)
      setHours(null)
      setMsg('')
    },
    onError: (e) => setMsg((e as Error).message),
  })
  const trigger = useMutation({
    mutationFn: async () => {
      try {
        return await post<{ msg: string }>('/api/v2/scheduler/trigger', {})
      } catch (e) {
        if (e instanceof ApiError) return { msg: e.message }
        throw e
      }
    },
    onSuccess: (r) => {
      setMsg(r.msg)
      setTimeout(() => qc.invalidateQueries({ queryKey: ['scheduler'] }), 1500)
    },
  })
  const s = q.data
  if (!s) {
    return <Card style={{ marginBottom: 12 }}>{q.isError ? String(q.error) : '加载中…'}</Card>
  }
  const edit = hours ?? { checkin: (s.schedule.checkin ?? []).join(', '), keepalive: (s.schedule.keepalive ?? []).join(', ') }
  const dirty = hours !== null
  return (
    <Card
      title="后台任务"
      style={{ marginBottom: 12 }}
      extra={
        <Space>
          <Tag color={s.running ? 'processing' : s.enabled ? 'success' : 'default'}>{s.running ? '执行中' : s.enabled ? '已启用' : '已停用'}</Tag>
          <Button size="small" icon={<Play size={13} />} disabled={trigger.isPending || s.running} onClick={() => trigger.mutate()}>
            立即巡检
          </Button>
        </Space>
      }
    >
      <Typography.Paragraph type="secondary">
        Token 保活、国内版每日签到与活跃上报、国际版每日活跃对话，账号之间随机错峰 2–5 秒；受风控限制的账号和被暂停的区域会被跳过。
      </Typography.Paragraph>
      <Toggle label="启用定时任务" checked={s.enabled} onChange={(v) => update.mutate({ enabled: v })} />
      <Row gutter={12} style={{ marginTop: 12 }}>
        <Col xs={24} md={12}>
          <Typography.Text type="secondary">签到 / 活跃巡检的整点（本地时间，逗号分隔）</Typography.Text>
          <Input value={edit.checkin} onChange={(e) => setHours({ ...edit, checkin: e.target.value })} placeholder="9, 21" />
        </Col>
        <Col xs={24} md={12}>
          <Typography.Text type="secondary">Token 保活巡检的整点</Typography.Text>
          <Input value={edit.keepalive} onChange={(e) => setHours({ ...edit, keepalive: e.target.value })} placeholder="22" />
        </Col>
      </Row>
      <Space style={{ marginTop: 12 }} wrap>
        <Typography.Text type="secondary">
          上次：{s.last_run_time || '—'} · 下次：{s.next_run_time || '—'}
          {s.last_summary ? ` · ${s.last_summary}` : ''}
        </Typography.Text>
        {dirty && (
          <Button
            size="small"
            type="primary"
            loading={update.isPending}
            onClick={() => update.mutate({ schedule: { checkin: parseHours(edit.checkin), keepalive: parseHours(edit.keepalive) } })}
          >
            保存时间表
          </Button>
        )}
      </Space>
      <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 8 }}>
        积分后台刷新：间隔 {s.credits.ttl_hours} 小时{s.credits.ttl_hours === 0 ? '（已关闭）' : '，每次刷新 1 个最久未更新的账号'}
        {s.credits.last_run ? ` · 最近 ${s.credits.last_run}` : ''}
        {s.credits.last_uid ? ` · 账号 ${s.credits.last_uid.slice(0, 8)}` : ''}
        {s.credits.last_error ? ` · 最近失败：${s.credits.last_error}` : ''}
      </Typography.Paragraph>
      {msg && <Alert type="info" showIcon message={msg} style={{ marginBottom: 8 }} />}
      {s.logs.length > 0 && (
        <div className="logbox small mono" aria-label="调度器日志">
          {s.logs.slice(-12).map((l, i) => (
            <div key={i}>{l}</div>
          ))}
        </div>
      )}
    </Card>
  )
}

function ActivityCard() {
  const [range, setRange] = useState('7d')
  const [task, setTask] = useState('')
  const [result, setResult] = useState('')
  const [uid, setUid] = useState('')
  const q = useQuery({
    queryKey: ['activity', range, task, result, uid],
    queryFn: () => {
      const p = new URLSearchParams({ range, limit: '200' })
      if (task) p.set('task', task)
      if (result) p.set('result', result)
      if (uid) p.set('uid', uid)
      return get<ActivityList>('/api/v2/activity?' + p)
    },
    refetchInterval: 15_000,
  })
  const rows: ActivityRow[] = q.data?.rows ?? []
  const columns: ColumnsType<ActivityRow> = [
    { title: '时间', dataIndex: 'ts', width: 150, render: (v: string) => <span className="mono">{v}</span> },
    { title: '账号', dataIndex: 'nickname', width: 150, ellipsis: true, render: (v: string, r) => v || r.uid.slice(0, 8) },
    { title: '任务', dataIndex: 'task', width: 130, render: (v: string) => taskLabel[v] ?? v },
    { title: '来源', dataIndex: 'trigger', width: 100, render: (v: string) => <Typography.Text type="secondary">{triggerLabel[v] ?? v}</Typography.Text> },
    {
      title: '结果',
      dataIndex: 'ok',
      width: 90,
      render: (v: boolean) => <Tag color={v ? 'success' : 'error'}>{v ? '成功' : '失败'}</Tag>,
    },
    { title: '说明', dataIndex: 'message', ellipsis: true },
  ]
  return (
    <Card title="签到与活跃记录" extra={<Typography.Text type="secondary">共 {q.data?.total ?? 0} 条</Typography.Text>} styles={{ body: { padding: 0 } }}>
      <Space wrap style={{ padding: 12 }}>
        <Select
          style={{ width: 120 }}
          value={range}
          onChange={setRange}
          options={[
            { value: 'today', label: '今天' },
            { value: '1d', label: '24 小时' },
            { value: '7d', label: '7 天' },
            { value: '30d', label: '30 天' },
            { value: '90d', label: '90 天' },
            { value: 'all', label: '全部' },
          ]}
        />
        <Select
          style={{ width: 150 }}
          value={task}
          onChange={setTask}
          options={[{ value: '', label: '全部任务' }, ...Object.entries(taskLabel).map(([value, label]) => ({ value, label }))]}
        />
        <Select
          style={{ width: 120 }}
          value={result}
          onChange={setResult}
          options={[
            { value: '', label: '全部结果' },
            { value: 'ok', label: '成功' },
            { value: 'failed', label: '失败' },
          ]}
        />
        <Input.Search allowClear placeholder="账号 UID" style={{ width: 220 }} onChange={(e) => setUid(e.target.value.trim())} />
        {q.data?.truncated && <Tag>显示最近 {q.data.limit} 条</Tag>}
      </Space>
      <Table<ActivityRow>
        rowKey={(r) => r.ts + r.uid + r.task + r.message}
        size="small"
        columns={columns}
        dataSource={rows}
        loading={q.isFetching}
        pagination={{ pageSize: 20, showSizeChanger: false }}
        locale={{ emptyText: '这个范围里没有记录' }}
        scroll={{ x: 900 }}
      />
    </Card>
  )
}
