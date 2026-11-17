import React from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App } from './App'
import 'antd/dist/reset.css'
import './styles.css'
import './ui/ui.css'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      retry: (count, err) => !(err as { status?: number }).status && count < 2,
      refetchOnWindowFocus: false,
    },
  },
})

// A rebuild replaces the hashed route chunks, so a tab opened before it fails
// to lazy-load any page ("打不开"). Reload once when that happens; the
// sessionStorage guard keeps a broken deploy from looping.
window.addEventListener('vite:preloadError', (e) => {
  e.preventDefault()
  const key = 'wb-chunk-reload'
  const last = Number(sessionStorage.getItem(key) ?? 0)
  if (Date.now() - last < 30_000) return
  sessionStorage.setItem(key, String(Date.now()))
  window.location.reload()
})

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </React.StrictMode>,
)
