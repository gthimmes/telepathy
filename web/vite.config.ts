import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The production build is written into the Go module so the server can embed it.
// In dev (`npm run dev`), API and OTLP calls are proxied to a running `telepathy serve`.
export default defineConfig({
  plugins: [react()],
  build: { outDir: '../internal/ui/dist', emptyOutDir: true },
  server: { proxy: { '/api': 'http://localhost:4318', '/v1': 'http://localhost:4318' } },
})
