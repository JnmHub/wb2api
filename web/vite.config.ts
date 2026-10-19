/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The production bundle is embedded into the Go binary (internal/webui).
export default defineConfig({
  plugins: [react()],
  build: { outDir: '../internal/webui/dist', emptyOutDir: true, sourcemap: false, target: 'es2022' },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:18788', changeOrigin: false },
      '/v1': { target: 'http://127.0.0.1:18788', changeOrigin: false },
    },
  },
  test: { environment: 'jsdom', globals: true, setupFiles: ['./src/test-setup.ts'] },
})
