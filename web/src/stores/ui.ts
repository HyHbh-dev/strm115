import { reactive, watch } from 'vue'

export type ThemeMode = 'dark' | 'light' | 'auto'
export type Palette = 'warm' | 'fresh'

const root = document.documentElement

function media() {
  return window.matchMedia('(prefers-color-scheme: dark)')
}

export const ui = reactive({
  theme: (localStorage.getItem('ui.theme') as ThemeMode) || 'dark',
  palette: (localStorage.getItem('ui.palette') as Palette) || 'warm',
  /** 桌面端折叠侧栏 */
  railCollapsed: localStorage.getItem('ui.rail') === '1',
  /** 移动端抽屉 */
  mobileNav: false,
  /** 全局搜索面板 */
  commandOpen: false,
})

export function applyTheme() {
  const dark = ui.theme === 'dark' || (ui.theme === 'auto' && media().matches)
  root.classList.toggle('dark', dark)
  root.dataset.palette = ui.palette
  localStorage.setItem('ui.theme', ui.theme)
  localStorage.setItem('ui.palette', ui.palette)
}

export function setTheme(mode: ThemeMode) {
  ui.theme = mode
  applyTheme()
}

export function toggleTheme() {
  setTheme(root.classList.contains('dark') ? 'light' : 'dark')
}

export function setPalette(p: Palette) {
  ui.palette = p
  applyTheme()
}

export function toggleRail() {
  ui.railCollapsed = !ui.railCollapsed
  localStorage.setItem('ui.rail', ui.railCollapsed ? '1' : '0')
}

export function isDark(): boolean {
  return root.classList.contains('dark')
}

watch(
  () => ui.palette,
  () => applyTheme(),
)

applyTheme()
media().addEventListener('change', () => {
  if (ui.theme === 'auto') applyTheme()
})
