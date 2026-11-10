<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import NotificationPanel from '@/layout/NotificationPanel.vue'
import { logout, session } from '@/stores/session'
import { isDark, toggleTheme, ui } from '@/stores/ui'
import { toast } from '@/stores/toast'
import {
  unread, panelOpen, loadNotifications,
  startNotificationPolling, stopNotificationPolling,
} from '@/stores/notifications'

const route = useRoute()
const router = useRouter()
const menuOpen = ref(false)
const menuRef = ref<HTMLElement | null>(null)
const notifyRef = ref<HTMLElement | null>(null)

/**
 * 顶栏右侧改成「鼠标滑过即展开」。
 * 之前只能点，公网/隧道访问时每次点都要等一轮请求 + 弹层模糊重绘，感觉很卡。
 * 点击仍然有用：点一下 = 固定住，鼠标移开不收起，方便点面板里的按钮。
 */
const pinned = ref<'none' | 'notify' | 'user'>('none')
let hoverTimer: number | undefined

function clearHover() {
  if (hoverTimer) {
    window.clearTimeout(hoverTimer)
    hoverTimer = undefined
  }
}

function canHover() {
  // 触屏 / 只有粗指针的设备不要 hover 展开，保持点击语义。
  return window.matchMedia('(hover: hover) and (pointer: fine)').matches
}

function hoverOpen(which: 'notify' | 'user') {
  clearHover()
  if (pinned.value === which || !canHover()) return
  hoverTimer = window.setTimeout(() => {
    // 之前点固定的另一个面板让位给这次悬停预览，两个面板始终互斥。
    pinned.value = 'none'
    if (which === 'notify') {
      menuOpen.value = false
      panelOpen.value = true
      void loadNotifications()
    } else {
      panelOpen.value = false
      menuOpen.value = true
    }
  }, 110)
}

function hoverClose(which: 'notify' | 'user') {
  clearHover()
  if (pinned.value !== 'none' || !canHover()) return
  // 留一点时间给鼠标从按钮走到面板，别一离开按钮就闪没。
  hoverTimer = window.setTimeout(() => {
    if (which === 'notify') panelOpen.value = false
    else menuOpen.value = false
  }, 220)
}

const title = computed(() => (route.meta.title as string) || '总览面板')
const desc = computed(() => (route.meta.desc as string) || '')
const initials = computed(() => (session.user || 'A').slice(0, 1).toUpperCase())
const unreadCount = computed(() => unread.value)
const badgeText = computed(() => (unread.value > 99 ? '99+' : String(unread.value)))

function onDocClick(e: MouseEvent) {
  const t = e.target as Node
  // 捕获阶段监听:下拉按钮上的 .stop 拦不住它,两个面板天然互斥,点外部一起关。
  if (menuRef.value && !menuRef.value.contains(t)) {
    menuOpen.value = false
    if (pinned.value === 'user') pinned.value = 'none'
  }
  if (notifyRef.value && !notifyRef.value.contains(t)) {
    panelOpen.value = false
    if (pinned.value === 'notify') pinned.value = 'none'
  }
}

function toggleNotifications() {
  clearHover()
  menuOpen.value = false
  // 悬停已经把面板打开时，点一下的意图是「固定住」，不是关掉；
  // 只有本来就是点击固定的，再点才是收起。
  if (panelOpen.value && pinned.value === 'notify') {
    panelOpen.value = false
    pinned.value = 'none'
    return
  }
  panelOpen.value = true
  pinned.value = 'notify'
  void loadNotifications()
}

function toggleMenu() {
  clearHover()
  panelOpen.value = false
  if (menuOpen.value && pinned.value === 'user') {
    menuOpen.value = false
    pinned.value = 'none'
    return
  }
  menuOpen.value = true
  pinned.value = 'user'
}

function onKey(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    ui.commandOpen = true
  }
}

async function doLogout() {
  menuOpen.value = false
  await logout()
  toast.info('已退出登录')
  router.replace({ name: 'login' })
}

onMounted(() => {
  document.addEventListener('click', onDocClick, true)
  window.addEventListener('keydown', onKey)
  startNotificationPolling()
})
onBeforeUnmount(() => {
  clearHover()
  document.removeEventListener('click', onDocClick, true)
  window.removeEventListener('keydown', onKey)
  stopNotificationPolling()
})
</script>

<template>
  <header class="topbar">
    <button class="btn btn-icon only-mobile" aria-label="菜单" @click="ui.mobileNav = !ui.mobileNav">
      <AppIcon name="list" />
    </button>

    <div class="page-id">
      <h1>{{ title }}</h1>
      <p v-if="desc" class="muted">{{ desc }}</p>
    </div>

    <div class="grow" />

    <button class="search-trigger" @click="ui.commandOpen = true">
      <AppIcon name="search" :size="16" />
      <span>搜索页面、库、片目…</span>
      <kbd>⌘K</kbd>
    </button>

    <button class="btn btn-icon" title="切换主题" @click="toggleTheme">
      <AppIcon :name="isDark() ? 'sun' : 'moon'" />
    </button>

    <div
      ref="notifyRef"
      class="notif-wrap"
      @mouseenter="hoverOpen('notify')"
      @mouseleave="hoverClose('notify')"
    >
      <button class="btn btn-icon bell" title="消息" @click.stop="toggleNotifications()">
        <AppIcon name="bell" />
        <span v-if="unreadCount" class="badge-count">{{ badgeText }}</span>
      </button>
      <NotificationPanel v-if="panelOpen" />
    </div>

    <div
      ref="menuRef"
      class="user"
      @mouseenter="hoverOpen('user')"
      @mouseleave="hoverClose('user')"
    >
      <button class="user-btn" @click.stop="toggleMenu">
        <span class="avatar">{{ initials }}</span>
        <span class="user-name">{{ session.user || 'admin' }}</span>
        <AppIcon name="chevronDown" :size="14" />
      </button>
      <Transition name="fade">
        <div v-if="menuOpen" class="menu">
          <div class="menu-head">
            <strong>{{ session.user || 'admin' }}</strong>
            <span class="muted small">管理员 · v{{ session.version || '0.5.2' }}</span>
          </div>
          <hr class="divider" />
          <RouterLink class="menu-item" to="/settings" @click="menuOpen = false">
            <AppIcon name="settings" :size="16" />系统配置
          </RouterLink>
          <RouterLink class="menu-item" to="/logs" @click="menuOpen = false">
            <AppIcon name="logs" :size="16" />实时日志
          </RouterLink>
          <hr class="divider" />
          <button class="menu-item danger" @click="doLogout">
            <AppIcon name="logout" :size="16" />退出登录
          </button>
        </div>
      </Transition>
    </div>
  </header>
</template>

<style scoped>
.topbar {
  position: sticky;
  top: 0;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 10px;
  height: var(--topbar-h);
  padding: 0 clamp(14px, 2.4vw, 32px);
  background: var(--surface);
  border-bottom: 1px solid var(--separator);
}
.page-id {
  min-width: 0;
}
.page-id h1 {
  font-size: 16px;
  font-weight: 640;
  letter-spacing: -0.01em;
}
.page-id p {
  font-size: 11.5px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 46vw;
}
.search-trigger {
  display: flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  padding: 0 8px 0 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
  color: var(--muted);
  font-size: 12.5px;
  transition: border-color 0.16s, background 0.16s;
}
.search-trigger:hover {
  border-color: var(--border-secondary);
  background: var(--surface-secondary);
}
.search-trigger kbd {
  padding: 1px 5px;
  border-radius: 5px;
  background: var(--surface-secondary);
  border: 1px solid var(--border);
  font-family: var(--font-mono);
  font-size: 10.5px;
}
.bell {
  position: relative;
}
.bell .badge-count {
  position: absolute;
  top: -3px;
  right: -5px;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: var(--r-full);
  background: var(--danger);
  color: #fff;
  font-size: 10px;
  font-weight: 700;
  line-height: 16px;
  text-align: center;
  box-shadow: 0 0 0 2px var(--background);
}
.notif-wrap {
  position: relative;
}
.user {
  position: relative;
}
.user-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  height: 34px;
  padding: 0 8px 0 4px;
  border: 1px solid transparent;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--muted);
}
.user-btn:hover {
  background: var(--surface-secondary);
}
.user-name {
  font-size: 12.5px;
  font-weight: 550;
  color: var(--foreground);
  max-width: 110px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.menu {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  width: 208px;
  padding: 6px;
  border-radius: var(--r-md);
  z-index: 50;
  /* 实色不用 backdrop-filter：弹层一展开就大面积采样背景重绘，
     走公网域名 / 远程桌面时是「点右侧很卡」的主要来源。 */
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--overlay-shadow);
}
.menu-head {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px 10px;
}
.menu-item {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  height: 32px;
  padding: 0 10px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--foreground);
  font-size: 13px;
  text-align: left;
}
.menu-item:hover {
  background: var(--surface-secondary);
}
.menu-item.danger {
  color: var(--danger);
}
.only-mobile {
  display: none;
}
@media (max-width: 900px) {
  .only-mobile {
    display: inline-flex;
  }
  .search-trigger span,
  .search-trigger kbd,
  .user-name {
    display: none;
  }
  .search-trigger {
    width: 34px;
    justify-content: center;
    padding: 0;
  }
}
</style>
