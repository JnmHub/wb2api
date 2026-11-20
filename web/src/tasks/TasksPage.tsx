import { useState } from 'react'
import { useMutation, useQuery, useQueryClient, type UseQueryResult } from '@tanstack/react-query'
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

const taskLabel: Record<string, string> = {
  checkin: '国内签到',
  codearts_checkin: 'CodeArts 每日签到',
  daily_chat: '国际每日活跃',
  activity_report: '国内活跃上报',
}
const triggerLabel: Record<string, string> = {
  scheduler: '定时',
  makeup: '补跑',
  manual: '手动',
  account_add: '新增账号',
  account_import: '导入账号',
  unknown: '—',
}

const dailyStates: Record<string, { label: string; color: string }> = {
  claimed: { label: '已到账', color: 'success' },
  already_claimed: { label: '已领取', color: 'success' },
  unavailable: { label: '暂无活动', color: 'default' },
  pending_confirmation: { label: '待官方确认', color: 'warning' },
  waiting_retry: { label: '等待重试', color: 'processing' },
  needs_login: { label: '需重新登录', color: 'error' },
  attempts_exhausted: { label: '已达尝试上限', color: 'warning' },
  not_run: { label: '尚未执行', color: 'default' },
}

const beijingDateTime = new Intl.DateTimeFormat('zh-CN', {
  timeZone: 'Asia/Shanghai',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hourCycle: 'h23',
})

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
  const scheduler = useQuery({ queryKey: ['scheduler'], queryFn: () => get<SchedulerStatus>('/api/v2/scheduler'), refetchInterval: 5000 })
  return (
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            任务
          </Typography.Title>
          <Typography.Text type="secondary">定时巡检、CodeArts 签到、国内成长任务与活动历史。</Typography.Text>
        </div>
      </div>
      <SchedulerCard query={scheduler} />
      <CodeArtsDailyCard status={scheduler.data} />
      <RaccoonDailyCard status={scheduler.data} />
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

function SchedulerCard({ query: q }: { query: UseQueryResult<SchedulerStatus> }) {
  const qc = useQueryClient()
  const [hours, setHours] = useState<{ checkin: string; daily_chat: string; keepalive: string } | null>(null)
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
  const edit =
    hours ?? {
      checkin: (s.schedule.checkin ?? []).join(', '),
      daily_chat: (s.schedule.daily_chat ?? []).join(', '),
      keepalive: (s.schedule.keepalive ?? []).join(', '),
    }
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
        每类任务只在各自的整点执行，错过最后一个整点后当天后续巡检会补跑；「立即巡检」不受整点限制。
      </Typography.Paragraph>
      <Toggle label="启用定时任务" checked={s.enabled} onChange={(v) => update.mutate({ enabled: v })} />
      <Row gutter={12} style={{ marginTop: 12 }}>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">国内签到 / 活跃上报的整点（本地时间，逗号分隔）</Typography.Text>
          <Input value={edit.checkin} onChange={(e) => setHours({ ...edit, checkin: e.target.value })} placeholder="9, 21" />
        </Col>
        <Col xs={24} md={8}>
          <Typography.Text type="secondary">国际版每日活跃对话的整点</Typography.Text>
          <Input value={edit.daily_chat} onChange={(e) => setHours({ ...edit, daily_chat: e.target.value })} placeholder="9, 21" />
        </Col>
        <Col xs={24} md={8}>
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
            onClick={() =>
              update.mutate({
                schedule: {
                  checkin: parseHours(edit.checkin),
                  daily_chat: parseHours(edit.daily_chat),
                  keepalive: parseHours(edit.keepalive),
                },
              })
            }
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

function RaccoonDailyCard({ status }: { status?: SchedulerStatus }) {
  const qc = useQueryClient()
  const [edit, setEdit] = useState<{ time: string; uids: string[] } | null>(null)
  const [notice, setNotice] = useState<{ text: string; type: 'info' | 'error' } | null>(null)
  const update = useMutation({
    mutationFn: (body: { enabled?: boolean; time?: string; uids?: string[] }) =>
      patch<SchedulerStatus>('/api/v2/scheduler', { raccoon_daily: body }),
    onSuccess: (status, body) => {
      qc.setQueryData(['scheduler'], status)
      if (body.time !== undefined) setEdit(null)
      setNotice(null)
    },
    onError: (error) => setNotice({ text: (error as Error).message, type: 'error' }),
  })
  const trigger = useMutation({
    mutationFn: () => post<{ msg: string }>('/api/v2/scheduler/trigger-raccoon', {}),
    onSuccess: (result) => {
      setNotice({ text: result.msg, type: 'info' })
      void qc.invalidateQueries({ queryKey: ['scheduler'] })
      void qc.invalidateQueries({ queryKey: ['activity'] })
      void qc.invalidateQueries({ queryKey: ['accounts'] })
    },
    onError: (error) => setNotice({ text: (error as Error).message, type: 'error' }),
  })
  const daily = status?.raccoon_daily
  if (!daily?.available) return null
  const value = edit ?? { time: daily.time, uids: daily.uids }
  const automatic = daily.enabled && status?.enabled
  return (
    <Card
      title="小浣熊每日积分"
      style={{ marginBottom: 12 }}
      extra={
        <Space>
          <Tag color={daily.running ? 'processing' : automatic ? 'success' : 'default'}>
            {daily.running ? '执行中' : automatic ? '已启用' : '自动签到已停用'}
          </Tag>
          <Button
            size="small"
            icon={<Play size={13} />}
            loading={trigger.isPending}
            disabled={daily.running || !daily.account_count}
            onClick={() => trigger.mutate()}
          >
            立即签到
          </Button>
        </Space>
      }
    >
      <Toggle label="启用小浣熊自动签到" checked={daily.enabled} onChange={(enabled) => update.mutate({ enabled })} />
      <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
        小浣熊没有签到接口：每日积分是打开桌面端首页时按天发放的，网关会触发一次并回读积分账单确认是否到账。
      </Typography.Paragraph>
      <Row gutter={[12, 12]} style={{ marginTop: 12 }}>
        <Col xs={24} md={6}>
          <Typography.Text type="secondary">每天开始时间（北京时间）</Typography.Text>
          <Input type="time" aria-label="小浣熊每天开始时间" value={value.time} onChange={(e) => setEdit({ ...value, time: e.target.value })} />
        </Col>
        <Col xs={24} md={18}>
          <Typography.Text type="secondary">账号范围</Typography.Text>
          <Select
            mode="tags"
            aria-label="小浣熊签到账号范围"
            tokenSeparators={[',', '，', ' ']}
            style={{ width: '100%' }}
            value={value.uids}
            onChange={(uids) => setEdit({ ...value, uids })}
            placeholder="输入账号 UID，留空时涵盖全部已启用定时任务的小浣熊账号"
          />
        </Col>
      </Row>
      <Space wrap style={{ marginTop: 12 }}>
        <Typography.Text type="secondary">
          今日范围内 {daily.account_count} 个账号
          {daily.last_run_at ? ` · 上次运行 ${daily.last_run_at}` : ''}
        </Typography.Text>
        {Object.entries(raccoonDailyStates).map(([key, state]) => {
          const count = daily.states[key]
          return count ? <Tag key={key} color={state.color}>{state.label} {count}</Tag> : null
        })}
        {edit && (
          <Button size="small" type="primary" loading={update.isPending} disabled={!value.time} onClick={() => update.mutate(value)}>
            保存签到设置
          </Button>
        )}
        {notice && <Typography.Text type={notice.type === 'error' ? 'danger' : 'secondary'}>{notice.text}</Typography.Text>}
      </Space>
    </Card>
  )
}

function CodeArtsDailyCard({ status }: { status?: SchedulerStatus }) {
  const qc = useQueryClient()
  const [edit, setEdit] = useState<{ time: string; uids: string[] } | null>(null)
  const [notice, setNotice] = useState<{ text: string; type: 'info' | 'error' } | null>(null)
  const update = useMutation({
    mutationFn: (body: { enabled?: boolean; time?: string; uids?: string[] }) =>
      patch<SchedulerStatus>('/api/v2/scheduler', { codearts_daily: body }),
    onSuccess: (status, body) => {
      qc.setQueryData(['scheduler'], status)
      if (body.time !== undefined) setEdit(null)
      setNotice(null)
    },
    onError: (error) => setNotice({ text: (error as Error).message, type: 'error' }),
  })
  const trigger = useMutation({
    mutationFn: () => post<{ msg: string }>('/api/v2/scheduler/trigger-codearts', {}),
    onSuccess: (result) => {
      setNotice({ text: result.msg, type: 'info' })
      void qc.invalidateQueries({ queryKey: ['scheduler'] })
      void qc.invalidateQueries({ queryKey: ['activity'] })
    },
    onError: (error) => setNotice({ text: (error as Error).message, type: 'error' }),
  })
  const daily = status?.codearts_daily
  if (!daily?.available) return null
  const value = edit ?? { time: daily.time, uids: daily.uids }
  const automatic = daily.enabled && status?.enabled
  return (
    <Card
      title="CodeArts 每日签到"
      style={{ marginBottom: 12 }}
      extra={
        <Space>
          <Tag color={daily.running ? 'processing' : automatic ? 'success' : 'default'}>
            {daily.running ? '执行中' : automatic ? '已启用' : '自动签到已停用'}
          </Tag>
          <Button size="small" icon={<Play size={13} />} loading={trigger.isPending} disabled={daily.running || !daily.account_count} onClick={() => trigger.mutate()}>
            立即签到
          </Button>
        </Space>
      }
    >
      <Toggle label="启用 CodeArts 自动签到" checked={daily.enabled} onChange={(enabled) => update.mutate({ enabled })} />
      <Row gutter={[12, 12]} style={{ marginTop: 12 }}>
        <Col xs={24} md={6}>
          <Typography.Text type="secondary">每天开始时间（北京时间）</Typography.Text>
          <Input type="time" aria-label="每天开始时间（北京时间）" value={value.time} onChange={(e) => setEdit({ ...value, time: e.target.value })} />
        </Col>
        <Col xs={24} md={18}>
          <Typography.Text type="secondary">账号范围</Typography.Text>
          <Select
            mode="tags"
            aria-label="签到账号范围"
            tokenSeparators={[',', '，', ' ']}
            style={{ width: '100%' }}
            value={value.uids}
            onChange={(uids) => setEdit({ ...value, uids })}
            placeholder="输入账号 UID，留空时涵盖全部已启用定时任务的 CodeArts 账号"
          />
        </Col>
      </Row>
      <Space wrap style={{ marginTop: 12 }}>
        <Typography.Text type="secondary">今日范围内 {daily.account_count} 个账号</Typography.Text>
        {Object.entries(dailyStates).map(([key, state]) => {
          const count = daily.states[key]
          return count ? <Tag key={key} color={state.color}>{state.label} {count}</Tag> : null
        })}
        {edit && (
          <Button size="small" type="primary" loading={update.isPending} disabled={!value.time} onClick={() => update.mutate(value)}>
            保存签到设置
          </Button>
        )}
      </Space>
      <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
        {daily.last_run_at ? `上次触发 ${beijingDateTime.format(new Date(daily.last_run_at))} · ` : ''}
        {daily.next_run_at ? `下次 ${beijingDateTime.format(new Date(daily.next_run_at))} · ` : ''}
        到点后执行，错过会在当天补跑，领取结果以官方确认状态为准。
      </Typography.Paragraph>
      {notice && <Alert style={{ marginTop: 8 }} type={notice.type} showIcon message={notice.text} />}
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
    {
      title: '时间',
      dataIndex: 'ts',
      width: 150,
      render: (value: string, row) => (
        <>
          <span className="mono">{value}</span>
          {row.day && !value.startsWith(row.day) ? <div><Typography.Text type="secondary">签到日 {row.day}</Typography.Text></div> : null}
        </>
      ),
    },
    { title: '账号', dataIndex: 'nickname', width: 150, ellipsis: true, render: (v: string, r) => v || r.uid.slice(0, 8) },
    { title: '任务', dataIndex: 'task', width: 130, render: (v: string) => taskLabel[v] ?? v },
    { title: '来源', dataIndex: 'trigger', width: 100, render: (v: string) => <Typography.Text type="secondary">{triggerLabel[v] ?? v}</Typography.Text> },
    {
      title: '结果',
      dataIndex: 'ok',
      width: 135,
      render: (v: boolean, row) => {
        const state = row.state ? dailyStates[row.state] : undefined
        return <Tag color={state?.color ?? (v ? 'success' : 'error')}>{state?.label ?? (v ? '成功' : '失败')}</Tag>
      },
    },
    {
      title: '说明',
      dataIndex: 'message',
      render: (message: string, row) => (
        <>
          {message}
          {row.next_retry_at ? <div><Typography.Text type="secondary">下次核对 {beijingDateTime.format(row.next_retry_at * 1000)}（北京时间）</Typography.Text></div> : null}
        </>
      ),
    },
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
          onChange={(task) => { setTask(task); setResult('') }}
          options={[{ value: '', label: '全部任务' }, ...Object.entries(taskLabel).map(([value, label]) => ({ value, label }))]}
        />
        <Select
          style={{ width: 120 }}
          value={result}
          onChange={setResult}
          options={[
            { value: '', label: '全部结果' },
            ...(task === 'codearts_checkin'
              ? Object.entries(dailyStates).filter(([state]) => state !== 'not_run').map(([value, state]) => ({ value, label: state.label }))
              : [{ value: 'ok', label: '成功' }, { value: 'failed', label: '未完成' }]),
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

// The raccoon scheduler reports its own state vocabulary: "due" = will run at
// the next tick, "skip" = ineligible (rejected credential / daily task off).
const raccoonDailyStates: Record<string, { label: string; color: string }> = {
  done: { label: '今日已签到', color: 'success' },
  due: { label: '待签到', color: 'processing' },
  retry: { label: '等待重试', color: 'warning' },
  skip: { label: '跳过（需重登/关闭任务）', color: 'default' },
  hold: { label: '风控暂停', color: 'warning' },
}
