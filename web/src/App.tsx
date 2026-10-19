import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, get, onUnauthorized, post, setCsrf, type SessionInfo } from './api'
import { AccountsPage } from './accounts/AccountsPage'
import { LogsPage } from './logs/LogsPage'
import { SettingsPage } from './settings/SettingsPage'
import { TasksPage } from './tasks/TasksPage'
import { UsagePage } from './usage/UsagePage'
import { useEventStream } from './events'

const loggedOut: SessionInfo = { authenticated: false, using_default_password: false }

function useSession() {
  return useQuery({
    queryKey: ['session'],
    staleTime: Infinity,
    retry: false,
    queryFn: async (): Promise<SessionInfo> => {
      try {
        const s = await get<SessionInfo>('/api/v2/session')
        if (s.csrf) setCsrf(s.csrf)
        return s
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) return loggedOut
        throw e
      }
    },
  })
}

export function App() {
  const qc = useQueryClient()
  const session = useSession()

  useEffect(
    () =>
      onUnauthorized(() => {
        qc.clear()
        qc.setQueryData(['session'], loggedOut)
      }),
    [qc],
  )

  if (session.isPending) return <div className="login dim">加载中…</div>
  if (session.isError)
    return (
      <div className="login">
        <div className="card">
          <b>无法连接到网关</b>
          <span className="dim">{String(session.error)}</span>
          <button className="btn" onClick={() => session.refetch()}>
            重试
          </button>
        </div>
      </div>
    )
  if (!session.data.authenticated) return <Login defaultPassword={session.data.using_default_password} />
  if (session.data.using_default_password) return <ChangePassword forced />
  return <Shell />
}

// ------------------------------------------------------------------- login

function Login({ defaultPassword }: { defaultPassword: boolean }) {
  const qc = useQueryClient()
  const [password, setPassword] = useState('')
  const [remember, setRemember] = useState(true)
  const m = useMutation({
    mutationFn: () => post<SessionInfo>('/api/v2/session/login', { password, remember }),
    onSuccess: (s) => {
      if (s.csrf) setCsrf(s.csrf)
      qc.setQueryData(['session'], s)
    },
  })
  return (
    <form
      className="login"
      onSubmit={(e) => {
        e.preventDefault()
        m.mutate()
      }}
    >
      <div className="card">
        <div className="brand">
          <i /> WorkBuddy2API Hub
        </div>
        {defaultPassword && <div className="dim">首次使用：默认密码是 <span className="mono">admin</span>，登录后需要先修改。</div>}
        <input
          className="input"
          type="password"
          autoFocus
          placeholder="面板密码"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
          记住我（30 天，重启网关也不会掉线）
        </label>
        <div className="err" role="alert">
          {m.error ? (m.error as Error).message : ''}
        </div>
        <button className="btn primary" disabled={!password || m.isPending}>
          {m.isPending ? '登录中…' : '登录'}
        </button>
      </div>
    </form>
  )
}

function ChangePassword({ forced, onDone }: { forced?: boolean; onDone?: () => void }) {
  const qc = useQueryClient()
  const [cur, setCur] = useState(forced ? 'admin' : '')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const m = useMutation({
    mutationFn: () => post('/api/v2/session/password', { current: cur, new: next }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['session'] })
      onDone?.()
    },
  })
  const bad = next.length < 4 ? '新密码至少 4 个字符' : next !== again ? '两次输入不一致' : ''
  return (
    <form
      className="login"
      onSubmit={(e) => {
        e.preventDefault()
        if (!bad) m.mutate()
      }}
    >
      <div className="card">
        <b>{forced ? '请先修改默认密码' : '修改面板密码'}</b>
        {forced && <span className="dim">在改掉默认密码之前，面板的其它功能保持锁定。</span>}
        <input className="input" type="password" placeholder="当前密码" value={cur} onChange={(e) => setCur(e.target.value)} />
        <input className="input" type="password" placeholder="新密码" autoFocus value={next} onChange={(e) => setNext(e.target.value)} />
        <input className="input" type="password" placeholder="再输一次" value={again} onChange={(e) => setAgain(e.target.value)} />
        <div className="err" role="alert">
          {(m.error as Error | null)?.message ?? (next || again ? bad : '')}
        </div>
        <button className="btn primary" disabled={!!bad || m.isPending}>
          保存
        </button>
        {!forced && (
          <button type="button" className="btn" onClick={onDone}>
            取消
          </button>
        )}
      </div>
    </form>
  )
}

// ------------------------------------------------------------------- shell

type Tab = 'accounts' | 'usage' | 'settings' | 'logs' | 'tasks'

const tabs: { id: Tab; label: string }[] = [
  { id: 'accounts', label: '账号' },
  { id: 'usage', label: '用量' },
  { id: 'settings', label: '设置' },
  { id: 'logs', label: '日志' },
  { id: 'tasks', label: '任务' },
]

function tabFromHash(): Tab {
  const h = location.hash.replace(/^#\/?/, '')
  return tabs.find((t) => t.id === h)?.id ?? 'accounts'
}

function Shell() {
  const qc = useQueryClient()
  const [tab, setTab] = useState<Tab>(tabFromHash)
  const [pw, setPw] = useState(false)
  useEventStream()
  useEffect(() => {
    const on = () => setTab(tabFromHash())
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  const logout = useMutation({
    mutationFn: () => post('/api/v2/session/logout'),
    onSettled: () => {
      qc.clear()
      qc.setQueryData(['session'], loggedOut)
    },
  })
  if (pw) return <ChangePassword onDone={() => setPw(false)} />
  const go = (t: Tab) => {
    location.hash = '#/' + t
    setTab(t)
  }
  return (
    <div className="shell">
      <header className="topbar">
        <div className="brand">
          <i /> Hub
        </div>
        <nav className="tabs" role="tablist">
          {tabs.map((t) => (
            <button key={t.id} className="tab" role="tab" aria-selected={tab === t.id} onClick={() => go(t.id)}>
              {t.label}
            </button>
          ))}
        </nav>
        <span className="spacer" />
        <button className="btn sm" onClick={() => setPw(true)}>
          修改密码
        </button>
        <button className="btn sm" onClick={() => logout.mutate()}>
          退出
        </button>
      </header>
      {tab === 'accounts' && <AccountsPage />}
      {tab === 'usage' && <UsagePage />}
      {tab === 'settings' && <SettingsPage />}
      {tab === 'logs' && <LogsPage />}
      {tab === 'tasks' && <TasksPage />}
    </div>
  )
}
