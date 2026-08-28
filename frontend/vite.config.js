import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { execSync } from 'node:child_process'

// 版本号单一真相: 优先 git tag (git describe), 其次 CI 环境变量 VITE_APP_VERSION, 最后回退 dev。
// 注意: 这里返回的值**不带前缀 v**(如 1.8.36), 前缀 v 由前端模板 `v{{ version }}` 统一添加,
// 避免"双 v"(vv1.8.36)问题。任何来源的前导 v 都会被 strip。
function resolveVersion() {
  const fromEnv = (process.env.VITE_APP_VERSION || '').replace(/^v/, '').trim()
  if (fromEnv) return fromEnv
  try {
    const tag = execSync('git describe --tags --always', { stdio: ['ignore', 'pipe', 'ignore'] })
      .toString().trim().replace(/^v/, '')
    if (tag) return tag
  } catch { /* git 不可用则回退 */ }
  return 'dev'
}

// 构建输出到 backend/internal/server/web/dist (被 Go //go:embed all:web 包含)。
export default defineConfig({
  plugins: [vue()],
  base: './',
  define: {
    'import.meta.env.VITE_APP_VERSION': JSON.stringify(resolveVersion()),
  },
  build: {
    outDir: '../backend/internal/server/web/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:18092',
        changeOrigin: true,
      },
    },
  },
})
