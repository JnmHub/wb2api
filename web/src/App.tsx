import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Checkbox, Input, Space, Typography } from 'antd'
import { ApiError, get, onUnauthorized, post, setCsrf, type SessionInfo } from './api'
import { AccountsPage } from './accounts/AccountsPage'
import { LogsPage } from './logs/LogsPage'
import { SettingsPage } from './settings/SettingsPage'
import { TasksPage } from './tasks/TasksPage'
import { UsagePage } from './usage/UsagePage'
import { useEventStream } from './events'
import { AppShell, type Tab } from './ui/AppShell'
import { ThemeProvider } from './ui/theme'

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

  return (
    <ThemeProvider>
      {session.isPending ? (
        <div className="login-screen">
          <Typography.Text type="secondary">加载中…</Typography.Text>
        </div>
      ) : session.isError ? (
        <div className="login-screen">
          <Card className="login-card">
            <Typography.Title level={4}>无法连接到网关</Typography.Title>
            <Typography.Paragraph type="secondary">{String(session.error)}</Typography.Paragraph>
            <Button onClick={() => session.refetch()}>重试</Button>
          </Card>
        </div>
      ) : !session.data.authenticated ? (
        <Login defaultPassword={session.data.using_default_password} />
      ) : session.data.using_default_password ? (
        <ChangePassword forced />
      ) : (
        <Shell />
      )}
    </ThemeProvider>
  )
}

function Brand() {
  return (
    <div className="login-brand">
      <span className="app-brand-mark">W</span>
      <span className="app-brand-name">WB2API</span>
      <span className="app-brand-tag">HUB</span>
    </div>
  )
}

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
    <div className="login-screen">
      <Card className="login-card">
        <Brand />
        <Typography.Title level={3} style={{ marginTop: 18, marginBottom: 4 }}>
          登录面板
        </Typography.Title>
        <Typography.Paragraph type="secondary" style={{ marginBottom: 20 }}>
          WorkBuddy 账号池、协议网关与用量观测。
        </Typography.Paragraph>
        {defaultPassword && <Alert type="warning" showIcon message="首次使用：默认密码是 admin，登录后需要先修改。" style={{ marginBottom: 16 }} />}
        <form
          onSubmit={(e) => {
            e.preventDefault()
            m.mutate()
          }}
        >
          <Space direction="vertical" size={14} style={{ width: '100%' }}>
            <Input.Password
              size="large"
              autoFocus
              placeholder="面板密码"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <Checkbox checked={remember} onChange={(e) => setRemember(e.target.checked)}>
              记住我（30 天，重启网关也不会掉线）
            </Checkbox>
            {m.error && <Alert type="error" showIcon message={(m.error as Error).message} />}
            <Button type="primary" size="large" block htmlType="submit" loading={m.isPending} disabled={!password}>
              登录
            </Button>
          </Space>
        </form>
      </Card>
      <Typography.Text className="login-foot" type="secondary">
        凭据只保存在本机，面板不会把密码发送到第三方。
      </Typography.Text>
    </div>
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
    <div className="login-screen">
      <Card className="login-card">
        <Brand />
        <Typography.Title level={4}>{forced ? '请先修改默认密码' : '修改面板密码'}</Typography.Title>
        {forced && <Typography.Paragraph type="secondary">在改掉默认密码之前，面板的其它功能保持锁定。</Typography.Paragraph>}
        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (!bad) m.mutate()
          }}
        >
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <Input.Password placeholder="当前密码" value={cur} onChange={(e) => setCur(e.target.value)} />
            <Input.Password placeholder="新密码" autoFocus value={next} onChange={(e) => setNext(e.target.value)} />
            <Input.Password placeholder="再输一次" value={again} onChange={(e) => setAgain(e.target.value)} />
            {(m.error || (next && bad)) && <Alert type="error" showIcon message={(m.error as Error | null)?.message ?? bad} />}
            <Button type="primary" htmlType="submit" disabled={!!bad || m.isPending} loading={m.isPending}>
              保存
            </Button>
            {!forced && <Button onClick={onDone}>取消</Button>}
          </Space>
        </form>
      </Card>
    </div>
  )
}

const TABS: Tab[] = ['accounts', 'usage', 'settings', 'logs', 'tasks']

function tabFromHash(): Tab {
  const h = location.hash.replace(/^#\/?/, '') as Tab
  return TABS.includes(h) ? h : 'accounts'
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
    <AppShell tab={tab} onTab={go} onPassword={() => setPw(true)} onLogout={() => logout.mutate()}>
      {tab === 'accounts' && <AccountsPage />}
      {tab === 'usage' && <UsagePage />}
      {tab === 'settings' && <SettingsPage />}
      {tab === 'logs' && <LogsPage />}
      {tab === 'tasks' && <TasksPage />}
    </AppShell>
  )
}
