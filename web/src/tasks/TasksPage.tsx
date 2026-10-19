import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
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
    return <section className="card sec dim">{(q.error as Error).message}</section>
  }
  const travelState: Record<string, string> = { idle: '在家', traveling: '旅行中', arrived: '已归来（待领奖）', unknown: '未知' }
  return (
    <section className="card sec">
      <div className="row">
        <h3>国内成长任务</h3>
        {snap && <span className="dim">{snap.account.nickname || snap.account.uid.slice(0, 8)}</span>}
        {snap && snap.accounts.length > 1 && (
          <select className="select" value={uid || snap.account.uid} onChange={(e) => setUid(e.target.value)} aria-label="选择账号">
            {snap.accounts.map((a) => (
              <option key={a.uid} value={a.uid}>
                {a.nickname || a.uid.slice(0, 8)}
              </option>
            ))}
          </select>
        )}
        <span className="spacer" />
        <button className="btn sm" disabled={act.isPending || !snap} onClick={() => act.mutate('travel')}>
          猫猫旅行
        </button>
        <button className="btn sm" disabled={act.isPending || !snap} onClick={() => act.mutate('streak-bonus')}>
          连登管家
        </button>
        <button className="btn sm primary" disabled={act.isPending || !snap} onClick={() => act.mutate('run')}>
          {act.isPending ? '执行中…' : '全自动跑一轮'}
        </button>
      </div>
      {!snap && !q.isError && <div className="dim">加载中…</div>}
      {snap && (
        <>
          <div className="row dim">
            <span>连登 {snap.summary.streak_days} 天</span>
            <span>能量 {snap.summary.energy}</span>
            <span>猫猫：{travelState[snap.summary.travel.state] ?? snap.summary.travel.state}</span>
            {snap.summary.travel.daily_limit_reached ? <span>今日旅行额度已用完</span> : null}
            {snap.account.enterprise ? <span className="badge cooling">企业版无成长体系</span> : null}
          </div>
          <div className="scroll">
            <table className="t">
              <thead>
                <tr>
                  <th>任务</th>
                  <th>状态</th>
                  <th className="num">进度</th>
                  <th className="num">奖励</th>
                </tr>
              </thead>
              <tbody>
                {snap.tasks.map((t) => (
                  <tr key={t.code}>
                    <td title={t.description}>{t.title || t.code}</td>
                    <td>
                      <span className={'badge ' + (t.status === 'claimed' ? 'ready' : t.status === 'completed' ? 'cooling' : 'intl')}>
                        {taskStatusLabel[t.status] ?? t.status}
                      </span>
                    </td>
                    <td className="num">
                      {t.current}/{t.target || '—'}
                    </td>
                    <td className="num">+{t.reward_credit}</td>
                  </tr>
                ))}
                {snap.tasks.length === 0 && (
                  <tr>
                    <td colSpan={4} className="dim" style={{ textAlign: 'center', padding: 24 }}>
                      没有可执行的成长任务
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
          <div className="dim">全自动跑一轮会接取任务并上报最小事件（每次写入间隔 1 秒），需要桌面端操作的任务会跳过。</div>
        </>
      )}
      {output && (
        <div className="actionresult" role="status">
          {output.map((l, i) => (
            <div key={i}>{l}</div>
          ))}
        </div>
      )}
    </section>
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
    return <section className="card sec dim">{q.isError ? (q.error as Error).message : '加载中…'}</section>
  }
  const edit = hours ?? { checkin: (s.schedule.checkin ?? []).join(', '), keepalive: (s.schedule.keepalive ?? []).join(', ') }
  const dirty = hours !== null
  return (
    <section className="card sec">
      <div className="row">
        <h3>后台任务</h3>
        <span className={'badge ' + (s.running ? 'cooling' : s.enabled ? 'ready' : 'disabled')}>
          {s.running ? '执行中' : s.enabled ? '已启用' : '已停用'}
        </span>
        <span className="spacer" />
        <button className="btn sm" disabled={trigger.isPending || s.running} onClick={() => trigger.mutate()}>
          立即巡检
        </button>
      </div>
      <p className="dim" style={{ margin: 0 }}>
        Token 保活、国内版每日签到与活跃上报、国际版每日活跃对话，账号之间随机错峰 2–5 秒；受风控限制的账号和被暂停的区域会被跳过。
      </p>
      <Toggle label="启用定时任务" checked={s.enabled} onChange={(v) => update.mutate({ enabled: v })} />
      <div className="grid2">
        <div className="field">
          <label>签到 / 活跃巡检的整点（本地时间，逗号分隔）</label>
          <input className="input" value={edit.checkin} onChange={(e) => setHours({ ...edit, checkin: e.target.value })} placeholder="9, 21" />
        </div>
        <div className="field">
          <label>Token 保活巡检的整点</label>
          <input className="input" value={edit.keepalive} onChange={(e) => setHours({ ...edit, keepalive: e.target.value })} placeholder="22" />
        </div>
      </div>
      <div className="row">
        <span className="dim">
          上次：{s.last_run_time || '—'} · 下次：{s.next_run_time || '—'}
          {s.last_summary ? ` · ${s.last_summary}` : ''}
        </span>
        <span className="spacer" />
        {dirty && (
          <button
            className="btn primary sm"
            disabled={update.isPending}
            onClick={() => update.mutate({ schedule: { checkin: parseHours(edit.checkin), keepalive: parseHours(edit.keepalive) } })}
          >
            保存时间表
          </button>
        )}
      </div>
      <div className="dim">
        积分后台刷新：间隔 {s.credits.ttl_hours} 小时{s.credits.ttl_hours === 0 ? '（已关闭）' : '，每次刷新 1 个最久未更新的账号'}
        {s.credits.last_run ? ` · 最近 ${s.credits.last_run}` : ''}
        {s.credits.last_uid ? ` · 账号 ${s.credits.last_uid.slice(0, 8)}` : ''}
        {s.credits.last_error ? ` · 最近失败：${s.credits.last_error}` : ''}
      </div>
      {msg && <div className="dim">{msg}</div>}
      {s.logs.length > 0 && (
        <div className="logbox small mono" aria-label="调度器日志">
          {s.logs.slice(-12).map((l, i) => (
            <div key={i}>{l}</div>
          ))}
        </div>
      )}
    </section>
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
  return (
    <section className="card sec">
      <h3>签到与活跃记录</h3>
      <div className="toolbar">
        <select className="select" value={range} onChange={(e) => setRange(e.target.value)} aria-label="时间范围">
          <option value="today">今天</option>
          <option value="1d">24 小时</option>
          <option value="7d">7 天</option>
          <option value="30d">30 天</option>
          <option value="90d">90 天</option>
          <option value="all">全部</option>
        </select>
        <select className="select" value={task} onChange={(e) => setTask(e.target.value)} aria-label="任务">
          <option value="">全部任务</option>
          {Object.entries(taskLabel).map(([k, v]) => (
            <option key={k} value={k}>
              {v}
            </option>
          ))}
        </select>
        <select className="select" value={result} onChange={(e) => setResult(e.target.value)} aria-label="结果">
          <option value="">全部结果</option>
          <option value="ok">成功</option>
          <option value="failed">失败</option>
        </select>
        <input className="input" placeholder="账号 UID" value={uid} onChange={(e) => setUid(e.target.value.trim())} />
        <span className="spacer" />
        <span className="dim">
          共 {q.data?.total ?? 0} 条{q.data?.truncated ? `，显示最近 ${q.data.limit} 条` : ''}
        </span>
      </div>
      <div className="scroll">
        <table className="t">
          <thead>
            <tr>
              <th>时间</th>
              <th>账号</th>
              <th>任务</th>
              <th>来源</th>
              <th>结果</th>
              <th>说明</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={r.ts + r.uid + i}>
                <td className="mono">{r.ts}</td>
                <td title={r.uid}>{r.nickname || r.uid.slice(0, 8)}</td>
                <td>{taskLabel[r.task] ?? r.task}</td>
                <td className="dim">{triggerLabel[r.trigger] ?? r.trigger}</td>
                <td>
                  <span className={'badge ' + (r.ok ? 'ready' : 'restricted')}>{r.ok ? '成功' : '失败'}</span>
                </td>
                <td className="dim" style={{ whiteSpace: 'normal' }}>
                  {r.message}
                </td>
              </tr>
            ))}
            {q.data && rows.length === 0 && (
              <tr>
                <td colSpan={6} className="dim" style={{ textAlign: 'center', padding: 24 }}>
                  这个范围里没有记录
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </section>
  )
}
