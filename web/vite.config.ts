import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
import { readFileSync, writeFileSync } from 'node:fs'

// 把 VERSION 注入前端：页面靠它和 /api/session 的版本比对，发现「服务器换了
// 新版、这个标签页还在跑旧 JS」时提示刷新。前端是 go:embed 进二进制的，
// 浏览器不重新加载 HTML 就永远看不到新界面，这个自检是唯一的兜底。
const appVersion = readFileSync(
  fileURLToPath(new URL('../VERSION', import.meta.url)),
  'utf-8',
).trim()

// emptyOutDir 会连 webdist/.gitkeep 一起删掉，而 go:embed all:webdist
// 要求这个目录在源码树里始终存在（哪怕本地还没构建过前端）。
// 构建结束时补回占位文件，省掉每次手工 git checkout。
const keepWebdist: Plugin = {
  name: 'keep-webdist-gitkeep',
  apply: 'build',
  closeBundle() {
    writeFileSync(
      fileURLToPath(new URL('../internal/httpapi/webdist/.gitkeep', import.meta.url)),
      '',
    )
  },
}

// 构建产物直接落到 Go 包内，go:embed 就能把它打进二进制：
// 部署时仍然只有一个二进制 + 一个端口，不需要额外的静态目录。
export default defineConfig({
  plugins: [vue(), keepWebdist],
  define: { __APP_VERSION__: JSON.stringify(appVersion) },
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: '../internal/httpapi/webdist',
    emptyOutDir: true,
    assetsInlineLimit: 2048,
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5273,
    proxy: { '/api': { target: 'http://127.0.0.1:6060', changeOrigin: true } },
  },
})
