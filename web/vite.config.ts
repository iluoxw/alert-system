import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // 仅供 Vite 本地开发：public/runtime-config.js 使用 /api，开发服务器把请求转到
    // 后端。生产镜像不使用这段配置，路径分流由集群入口层负责。
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
