<script setup lang="ts">
/**
 * 移动端底部导航。
 *
 * 手机上顶部那排按钮够不着（而且侧栏是抽屉，点开还要再收），所以把
 * 四个最常用的去处钉在拇指能够到的位置：总览 / 影视库 / 自动整理 / 任务。
 * 其余页面走「更多」，直接把侧栏抽屉打开 —— 不另做一套移动端菜单，
 * 否则两边迟早不同步。
 *
 * 只在 <=900px 显示，桌面端由 CSS 藏掉。
 */
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import { ui } from '@/stores/ui'

const route = useRoute()

const tabs = [
  { path: '/', label: '总览', icon: 'dashboard', match: (p: string) => p === '/' },
  { path: '/library', label: '影视库', icon: 'local', match: (p: string) => p.startsWith('/library') },
  { path: '/organize', label: '整理', icon: 'organize', match: (p: string) => p.startsWith('/organize') },
  { path: '/tasks', label: '任务', icon: 'tasks', match: (p: string) => p.startsWith('/tasks') },
]

const activePath = computed(() => route.path)
</script>

<template>
  <nav class="mtab" aria-label="主导航">
    <RouterLink
      v-for="t in tabs"
      :key="t.path"
      class="mtab-item"
      :class="{ 'is-active': t.match(activePath) }"
      :to="t.path"
    >
      <AppIcon :name="t.icon" :size="19" />
      <span>{{ t.label }}</span>
    </RouterLink>
    <button class="mtab-item" :class="{ 'is-active': ui.mobileNav }" @click="ui.mobileNav = true">
      <AppIcon name="list" :size="19" />
      <span>更多</span>
    </button>
  </nav>
</template>

<style scoped>
.mtab {
  display: none;
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 40;
  padding: 6px 4px calc(6px + env(safe-area-inset-bottom));
  background: color-mix(in oklab, var(--surface) 88%, transparent);
  backdrop-filter: blur(14px);
  border-top: 1px solid var(--border);
}
.mtab-item {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
  height: 46px;
  border: 0;
  background: none;
  color: var(--muted);
  font-size: 11px;
  font-family: inherit;
  cursor: pointer;
  border-radius: var(--r-sm);
  transition: color 0.14s;
}
.mtab-item.is-active {
  color: var(--accent);
}
.mtab-item:active {
  background: var(--surface-secondary);
}
@media (max-width: 900px) {
  .mtab {
    display: flex;
  }
}
</style>
