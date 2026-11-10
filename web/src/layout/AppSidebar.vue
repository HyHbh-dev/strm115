<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import { navGroups } from '@/data/nav'
import { session } from '@/stores/session'
import { APP_VERSION } from '@/build'
import { toggleRail, toggleTheme, ui, isDark } from '@/stores/ui'

const route = useRoute()
const collapsed = computed(() => ui.railCollapsed)

const activeName = computed(() => route.name as string)

// 显示「页面自己的版本」，不是服务端版本 —— 旧页面配新后端时，只有这个数字
// 能让人意识到该刷新了（服务端版本会被旧 JS 一起显示出来，非常误导）。
const pageVersion = APP_VERSION || session.version
const stale = computed(
  () => !!APP_VERSION && !!session.version && session.version !== APP_VERSION,
)
</script>

<template>
  <aside class="sidebar" :class="{ 'is-collapsed': collapsed, 'is-open': ui.mobileNav }">
    <div class="brand">
      <div class="mark">
        <svg viewBox="0 0 32 32" width="30" height="30" aria-hidden="true">
          <defs>
            <linearGradient id="brandGrad" x1="0" y1="0" x2="1" y2="1">
              <stop stop-color="#ff9d4d" />
              <stop offset="1" stop-color="#d9480f" />
            </linearGradient>
          </defs>
          <rect width="32" height="32" rx="8.5" fill="url(#brandGrad)" />
          <g stroke="#fff" stroke-linecap="round" stroke-width="2.2">
            <path d="M6.5 11.5h4" opacity=".55" />
            <path d="M4.8 16h6.2" opacity=".85" />
            <path d="M6.5 20.5h4" opacity=".55" />
            <path d="M16 11.5h10" opacity=".75" />
            <path d="M16 16h7" opacity=".55" />
            <path d="M16 20.5h10" opacity=".35" />
          </g>
        </svg>
      </div>
      <div v-if="!collapsed" class="brand-text">
        <strong>StrmStation</strong>
        <span>115 → STRM → Emby</span>
      </div>
    </div>

    <nav class="nav">
      <div v-for="g in navGroups" :key="g.label" class="nav-group">
        <p v-if="!collapsed" class="nav-group-label">{{ g.label }}</p>
        <RouterLink
          v-for="item in g.items.filter((i) => !i.hideInNav)"
          :key="item.name"
          :to="item.path"
          class="nav-item"
          :class="{ 'is-active': activeName === item.name }"
          :title="collapsed ? item.label : undefined"
          @click="ui.mobileNav = false"
        >
          <AppIcon :name="item.icon" :size="18" />
          <span v-if="!collapsed" class="nav-label">{{ item.label }}</span>
          <span v-if="!collapsed && item.name === 'tasks'" class="nav-dot" />
        </RouterLink>
      </div>
    </nav>

    <div class="sidebar-foot">
      <button class="rail-btn" :title="collapsed ? '展开侧栏' : '收起侧栏'" @click="toggleRail">
        <AppIcon :name="collapsed ? 'chevronRight' : 'chevronLeft'" :size="16" />
      </button>
      <div v-if="!collapsed" class="row-between" style="width: 100%">
        <span
          class="muted small mono"
          :class="{ 'ver-stale': stale }"
          :title="
            stale
              ? `页面还是 ${APP_VERSION}，服务端已经 ${session.version}，刷新页面即可更新`
              : '前端构建版本'
          "
        >
          v{{ pageVersion }}<template v-if="stale"> → {{ session.version }}</template>
        </span>
        <button class="btn btn-icon" :title="isDark() ? '切到浅色' : '切到深色'" @click="toggleTheme">
          <AppIcon :name="isDark() ? 'sun' : 'moon'" :size="16" />
        </button>
      </div>
    </div>
  </aside>
</template>

<style scoped>
.sidebar {
  position: fixed;
  inset: 0 auto 0 0;
  z-index: var(--z-sidebar);
  display: flex;
  flex-direction: column;
  width: var(--sidebar-w);
  padding: 14px 12px 12px;
  background: var(--surface);
  border-right: 1px solid var(--border);
  transition: width 0.28s var(--ease-out-fluid), transform 0.3s var(--ease-out-fluid);
}
.sidebar.is-collapsed {
  width: 72px;
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 4px 6px 16px;
}
.mark {
  flex: none;
  width: 30px;
  height: 30px;
  border-radius: 9px;
  box-shadow: 0 8px 18px -10px rgb(217 72 15 / 0.9);
}
.brand-text {
  display: flex;
  flex-direction: column;
  line-height: 1.25;
  overflow: hidden;
}
.brand-text strong {
  font-size: 14.5px;
  letter-spacing: -0.01em;
}
.brand-text span {
  font-size: 11px;
  color: var(--muted);
  white-space: nowrap;
}

.nav {
  flex: 1;
  overflow-y: auto;
  margin: 0 -4px;
  padding: 0 4px;
}
.nav-group + .nav-group {
  margin-top: 14px;
}
.nav-group-label {
  padding: 0 10px 6px;
  font-size: 10.5px;
  font-weight: 700;
  letter-spacing: 0.08em;
  color: color-mix(in oklab, var(--muted) 80%, transparent);
}
.nav-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 10px;
  height: 36px;
  padding: 0 10px;
  margin-bottom: 2px;
  border-radius: 10px;
  color: color-mix(in oklab, var(--foreground) 72%, transparent);
  font-size: 13.5px;
  font-weight: 500;
  transition: background 0.16s, color 0.16s, transform 0.16s var(--ease-out-fluid);
}
.is-collapsed .nav-item {
  justify-content: center;
  padding: 0;
}
.nav-item:hover {
  background: var(--surface-secondary);
  color: var(--foreground);
}
.nav-item.is-active {
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 600;
}
.nav-item.is-active::before {
  content: '';
  position: absolute;
  left: -6px;
  top: 9px;
  bottom: 9px;
  width: 3px;
  border-radius: 3px;
  background: var(--accent);
}
.nav-label {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.nav-dot {
  margin-left: auto;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--warning);
}

/* 页面版本落后于服务端：橙色提醒，title 里写明两边版本 */
.ver-stale {
  color: var(--warning, #d97706);
  font-weight: 600;
}
.sidebar-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 4px 2px;
  border-top: 1px solid var(--separator);  margin-top: 8px;
}
.rail-btn {
  display: grid;
  place-items: center;
  flex: none;
  width: 30px;
  height: 30px;
  border: 0;
  border-radius: 9px;
  background: transparent;
  color: var(--muted);
}
.rail-btn:hover {
  background: var(--surface-secondary);
  color: var(--foreground);
}

@media (max-width: 900px) {
  .sidebar {
    transform: translateX(-100%);
    box-shadow: var(--overlay-shadow);
    width: var(--sidebar-w);
  }
  .sidebar.is-open {
    transform: none;
  }
  .sidebar.is-collapsed {
    width: var(--sidebar-w);
  }
  .sidebar.is-collapsed .nav-label,
  .sidebar.is-collapsed .brand-text {
    display: block;
  }
}
</style>
