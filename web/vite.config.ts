import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    fs: {
      // Phase 1 将 docs/contracts 中的 Page View fixtures 直接打包进前端，避免复制后漂移。
      allow: [fileURLToPath(new URL('..', import.meta.url))],
    },
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
