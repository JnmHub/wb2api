import { useMemo, useState } from 'react'
import { Breadcrumb, Button, Drawer, Layout, Menu, Tooltip } from 'antd'
import { Activity, Boxes, FileClock, Gauge, ListChecks, Menu as MenuIcon, Moon, ScrollText, Settings, Sun, Users } from 'lucide-react'
import { useThemeMode } from './theme'

export type Tab = 'accounts' | 'usage' | 'models' | 'requests' | 'settings' | 'logs' | 'tasks'

const NAV = [
  {
    key: 'operate',
    label: '运行',
    items: [
      { key: 'accounts', label: '账号', icon: <Users size={16} /> },
      { key: 'usage', label: '用量', icon: <Gauge size={16} /> },
      { key: 'models', label: '模型', icon: <Boxes size={16} /> },
      { key: 'tasks', label: '任务', icon: <ListChecks size={16} /> },
    ],
  },
  {
    key: 'observe',
    label: '观测',
    items: [
      { key: 'requests', label: '请求', icon: <FileClock size={16} /> },
      { key: 'logs', label: '日志', icon: <ScrollText size={16} /> },
    ],
  },
  {
    key: 'control',
    label: '控制',
    items: [{ key: 'settings', label: '设置', icon: <Settings size={16} /> }],
  },
]

function Brand() {
  return (
    <div className="app-brand">
      <span className="app-brand-mark">
        <Activity size={16} />
      </span>
      <span className="app-brand-name">WB2API</span>
      <span className="app-brand-tag">HUB</span>
    </div>
  )
}

function NavMenus({ tab, onTab }: { tab: Tab; onTab: (t: Tab) => void }) {
  return (
    <div className="app-nav">
      {NAV.map((g) => (
        <Menu
          key={g.key}
          mode="inline"
          selectedKeys={[tab]}
          onClick={(e) => onTab(e.key as Tab)}
          style={{ background: 'transparent' }}
          items={[
            {
              type: 'group',
              label: g.label,
              children: g.items.map((i) => ({ key: i.key, label: i.label, icon: i.icon })),
            },
          ]}
        />
      ))}
    </div>
  )
}

export function AppShell({
  tab,
  onTab,
  onPassword,
  onLogout,
  children,
}: {
  tab: Tab
  onTab: (t: Tab) => void
  onPassword: () => void
  onLogout: () => void
  children: React.ReactNode
}) {
  const { mode, setMode } = useThemeMode()
  const [mobile, setMobile] = useState(false)
  const [navOpen, setNavOpen] = useState(false)
  const current = useMemo(() => NAV.flatMap((g) => g.items).find((i) => i.key === tab), [tab])
  const group = useMemo(() => NAV.find((g) => g.items.some((i) => i.key === tab))?.label ?? '运行', [tab])
  const go = (t: Tab) => {
    setNavOpen(false)
    onTab(t)
  }
  return (
    <Layout className="app-shell">
      <Layout.Sider
        width={214}
        className="app-sider app-sider-desktop"
        theme={mode}
        breakpoint="lg"
        collapsedWidth={0}
        trigger={null}
        onBreakpoint={setMobile}
      >
        <Brand />
        <NavMenus tab={tab} onTab={go} />
        <div className="app-sider-foot">WorkBuddy 账号池与用量网关</div>
      </Layout.Sider>
      <Drawer
        placement="left"
        width={248}
        open={mobile && navOpen}
        onClose={() => setNavOpen(false)}
        closable={false}
        styles={{ body: { padding: 0 } }}
      >
        <Brand />
        <NavMenus tab={tab} onTab={go} />
      </Drawer>
      <Layout>
        <Layout.Header className="app-header">
          {mobile && (
            <Button type="text" aria-label="打开导航" icon={<MenuIcon size={18} />} onClick={() => setNavOpen(true)} />
          )}
          <Breadcrumb items={[{ title: group }, { title: current?.label ?? '' }]} />
          <span className="spacer" />
          <Tooltip title={mode === 'dark' ? '切换浅色' : '切换深色'}>
            <Button
              type="text"
              aria-label="切换主题"
              icon={mode === 'dark' ? <Sun size={16} /> : <Moon size={16} />}
              onClick={() => setMode(mode === 'dark' ? 'light' : 'dark')}
            />
          </Tooltip>
          <Button type="text" size="small" onClick={onPassword}>
            修改密码
          </Button>
          <Button type="text" size="small" danger onClick={onLogout}>
            退出
          </Button>
        </Layout.Header>
        <Layout.Content className="app-content">
          <div className="app-page">{children}</div>
        </Layout.Content>
      </Layout>
    </Layout>
  )
}
