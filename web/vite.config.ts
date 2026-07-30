import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // 后端地址（cmd/server 装配的 Router）；开发态同源代理省去 CORS 配置，cookie
    // 随请求天然携带（浏览器视 /api 为与前端同源）。
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
