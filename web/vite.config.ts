import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发时前端跑在 5173，把 /api 代理到 Go 服务（默认 8080）。
// 这样浏览器看到的是同源请求，前端代码里不需要写死后端地址。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: { '/api': { target: 'http://127.0.0.1:8123', changeOrigin: true } },
  },
  build: { outDir: 'dist', sourcemap: true },
})
