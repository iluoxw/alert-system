import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // 后端地址（cmd/server 装配的 Router）；开发态同源代理省去 CORS 配置。除管理
    // API 外必须代理 /ingest，否则数据源页面按 window.location.origin 生成的
    // Webhook URL 会落到 Vite 自己并返回 404。
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/ingest': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
