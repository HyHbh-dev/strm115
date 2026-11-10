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

/* 用户主动关掉横幅后本次会话不再弹：checkVersion 每 5 分钟还会跑，
   不记这个状态就会关掉几分钟又冒出来。 */
let updateBarDismissed = false

function mountUpdateBar(serverVersion: string) {
  if (updateBarDismissed || document.getElementById('app-ver-bar')) return
  const bar = document.createElement('div')
  bar.id = 'app-ver-bar'
  bar.style.cssText = [
    'position:fixed', 'left:12px', 'right:12px',
    /* 宽屏 12px；窄屏由 ui.css 给 #app-ver-bar 设 --app-ver-bar-bottom，
       抬到移动端底部导航之上。用变量而非 !important 覆盖 inline，
       CSS media query 天然响应式，窗口旋转/缩放也会跟着变。 */
    'bottom:var(--app-ver-bar-bottom,12px)', 'z-index:9999',
    'display:flex', 'align-items:center', 'gap:10px',
    /* 宽屏保持 wrap；窄屏 ui.css 设 --app-ver-bar-wrap:nowrap，
       文字折行、按钮不换行，横幅才是一条细条而不是被按钮撑高的方块。 */
    'flex-wrap:var(--app-ver-bar-wrap,wrap)',
    'padding:10px 14px', 'border-radius:12px',
    /* 颜色全走设计令牌：亮/暗主题自动跟随。原来用 var(--text,#111)，
       tokens.css 并没有 --text，暗色下会变成深底黑字。 */
    'background:var(--surface)', 'color:var(--foreground)',
    'border:1px solid var(--border)', 'box-shadow:var(--overlay-shadow)',
    'font-size:13px',
  ].join(';')

  // 文字单独成 span：窄屏 nowrap 下它要能 min-width:0 收缩折行，
  // 匿名文本 flex item 收缩不了会把按钮挤出横幅。
  const msg = document.createElement('span')
  msg.textContent = `服务端已更新到 ${serverVersion}，当前页面还是 ${APP_VERSION}。`
  bar.appendChild(msg)

  const btn = document.createElement('button')
  btn.className = 'ver-refresh'
  btn.textContent = '刷新页面'
  btn.style.cssText = [
    /* 宽屏靠右；窄屏 ui.css 不再给它 width:100%，按钮只占自身宽度。 */
    'margin-left:var(--app-ver-btn-ml,auto)', 'padding:6px 12px', 'border-radius:8px', 'cursor:pointer',
    'border:1px solid var(--accent)', 'background:transparent',
    'color:var(--accent)', 'font:inherit', 'font-weight:600',
  ].join(';')
  btn.onclick = () => location.reload()
  bar.appendChild(btn)

  const close = document.createElement('button')
  close.className = 'ver-close'
  close.type = 'button'
  close.setAttribute('aria-label', '关闭更新提示')
  close.textContent = '×'
  close.style.cssText = [
    /* 宽屏横幅不挡内容，默认隐藏；窄屏 ui.css 才把它显示出来。
       颜色同样只用变量，暗色主题不会刺眼。 */
    'display:var(--app-ver-close-display,none)', 'flex:none',
    'place-items:center', 'width:24px', 'height:24px', 'padding:0',
    'border:0', 'border-radius:var(--r-xs)', 'background:transparent',
    'color:var(--muted)', 'font-size:17px', 'line-height:1', 'cursor:pointer',
  ].join(';')
  close.onclick = () => {
    updateBarDismissed = true
    document.body.classList.remove('ver-bar-on')
    bar.remove()
  }
  bar.appendChild(close)

  document.body.appendChild(bar)
  // 横幅是 fixed 浮层：窄屏内容区要靠这个 class 补底部留白（见 ui.css），
  // 否则滚到底时最后一张卡片的按钮会被横幅盖住。
  document.body.classList.add('ver-bar-on')
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
