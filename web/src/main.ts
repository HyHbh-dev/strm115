import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import { APP_VERSION } from './build'

import './styles/tokens.css'
import './styles/base.css'
import './styles/ui.css'

createApp(App).use(router).mount('#app')

/**
 * 版本自检。
 *
 * 前端是 go:embed 进二进制的，所以「服务端已经更新、浏览器还在跑旧 JS」是
 * 常态：标签页开着不刷新，怎么改都看不到（用户反馈「搜索并没有变」就是这个）。
 * index.html 虽然是 no-cache，但没人会为了看更新专门去点刷新。
 *
 * 这里每 5 分钟、以及切回标签页时问一次 /api/session，版本对不上就挂一条
 * 横幅让人点一下。只在服务端更新后出现，平时完全静默。
 */

function mountUpdateBar(serverVersion: string) {
  if (document.getElementById('app-ver-bar')) return
  const bar = document.createElement('div')
  bar.id = 'app-ver-bar'
  bar.style.cssText = [
    'position:fixed', 'left:12px', 'right:12px', 'bottom:12px', 'z-index:9999',
    'display:flex', 'align-items:center', 'gap:10px', 'flex-wrap:wrap',
    'padding:10px 14px', 'border-radius:12px',
    'background:var(--surface,#fff)', 'color:var(--text,#111)',
    'border:1px solid var(--border,#ddd)', 'box-shadow:0 8px 24px rgba(0,0,0,.16)',
    'font-size:13px',
  ].join(';')
  bar.textContent = `服务端已更新到 ${serverVersion}，当前页面还是 ${APP_VERSION}。`
  const btn = document.createElement('button')
  btn.textContent = '刷新页面'
  btn.style.cssText = [
    'margin-left:auto', 'padding:6px 12px', 'border-radius:8px', 'cursor:pointer',
    'border:1px solid var(--accent,#2563eb)', 'background:transparent',
    'color:var(--accent,#2563eb)', 'font:inherit', 'font-weight:600',
  ].join(';')
  btn.onclick = () => location.reload()
  bar.appendChild(btn)
  document.body.appendChild(bar)
}

async function checkVersion() {
  if (!APP_VERSION) return
  try {
    const r = await fetch('/api/session', { credentials: 'same-origin' })
    if (!r.ok) return
    const s = (await r.json()) as { version?: string }
    const v = String(s?.version || '')
    if (v && v !== APP_VERSION) mountUpdateBar(v)
  } catch {
    // 离线、接口异常都不打扰用户
  }
}

void checkVersion()
setInterval(() => void checkVersion(), 5 * 60 * 1000)
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') void checkVersion()
})
