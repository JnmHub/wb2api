import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { App as AntdApp, ConfigProvider, theme as antdTheme } from 'antd'
import zhCN from 'antd/locale/zh_CN'

export type ThemeMode = 'light' | 'dark'

const ThemeCtx = createContext<{ mode: ThemeMode; setMode: (m: ThemeMode) => void }>({
  mode: 'light',
  setMode: () => {},
})

export function useThemeMode() {
  return useContext(ThemeCtx)
}

const FONT =
  '"Sarasa Mono SC", "Sarasa UI SC", "更纱黑体 SC", "Berkeley Mono", "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace'

function token(mode: ThemeMode) {
  if (mode === 'dark') {
    return {
      colorPrimary: '#00a2fb',
      colorInfo: '#00a2fb',
      colorSuccess: '#10b981',
      colorWarning: '#f59e0b',
      colorError: '#ef4444',
      colorBgLayout: '#121214',
      colorBgContainer: '#1c1c1f',
      colorBgElevated: '#222226',
      colorBorder: '#2c2c30',
      colorBorderSecondary: '#212124',
      colorText: '#f4f4f6',
      colorTextSecondary: '#a1a1aa',
      colorTextTertiary: '#71717a',
      colorFillAlter: '#1c1c1f',
    }
  }
  return {
    colorPrimary: '#005d8f',
    colorInfo: '#0077b6',
    colorSuccess: '#059669',
    colorWarning: '#b45309',
    colorError: '#dc2626',
    colorBgLayout: '#f6f6f8',
    colorBgContainer: '#ffffff',
    colorBgElevated: '#ffffff',
    colorBorder: '#e4e4e7',
    colorBorderSecondary: '#ececf0',
    colorText: '#1c1c1e',
    colorTextSecondary: '#52525b',
    colorTextTertiary: '#71717a',
  }
}

/** Ant Design theme matching oh-my-cpa's tokens, with the two OMC palettes. */
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setMode] = useState<ThemeMode>(() => {
    const stored = localStorage.getItem('wb-theme')
    if (stored === 'dark' || stored === 'light') return stored
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  })
  useEffect(() => {
    localStorage.setItem('wb-theme', mode)
    document.documentElement.dataset.theme = mode
    document.documentElement.style.colorScheme = mode
  }, [mode])
  const cfg = useMemo(
    () => ({
      algorithm: mode === 'dark' ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
      token: {
        ...token(mode),
        fontFamily: FONT,
        fontFamilyCode: FONT,
        borderRadius: 6,
        borderRadiusSM: 4,
        borderRadiusLG: 8,
        fontSize: 13,
        controlHeight: 32,
      },
      components: {
        Layout: {
          siderBg: mode === 'dark' ? '#1c1c1f' : '#ffffff',
          headerBg: mode === 'dark' ? '#1c1c1f' : '#ffffff',
          bodyBg: mode === 'dark' ? '#121214' : '#f6f6f8',
        },
        Menu: {
          itemHeight: 38,
          itemMarginInline: 6,
          itemSelectedBg: mode === 'dark' ? '#2e2e37' : '#eef4f8',
          itemSelectedColor: mode === 'dark' ? '#00a2fb' : '#005d8f',
          groupTitleColor: mode === 'dark' ? '#71717a' : '#8b8b93',
          groupTitleFontSize: 11,
        },
        Card: { paddingLG: 18 },
        Table: { headerBg: mode === 'dark' ? '#1c1c1f' : '#f6f6f8', cellPaddingBlock: 8 },
      },
    }),
    [mode],
  )
  return (
    <ThemeCtx.Provider value={{ mode, setMode }}>
      <ConfigProvider locale={zhCN} theme={cfg}>
        <AntdApp>{children}</AntdApp>
      </ConfigProvider>
    </ThemeCtx.Provider>
  )
}
