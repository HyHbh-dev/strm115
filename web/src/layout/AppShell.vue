<script setup lang="ts">
import AppSidebar from './AppSidebar.vue'
import AppTopbar from './AppTopbar.vue'
import CommandPalette from './CommandPalette.vue'
import MobileTabBar from './MobileTabBar.vue'
import ToastHost from './ToastHost.vue'
import { ui } from '@/stores/ui'
</script>

<template>
  <div class="shell">
    <AppSidebar />
    <Transition name="fade">
      <div v-if="ui.mobileNav" class="nav-scrim" @click="ui.mobileNav = false" />
    </Transition>

    <div class="main" :class="{ 'is-rail': ui.railCollapsed }">
      <AppTopbar />
      <main class="content">
        <!-- 页面切换只做淡入，不做「先淡出再淡入」：
             0.22s 的 out-in 叠起来是 0.44s 的空白，点菜单时会被感知成「卡了一下」。
             旧页面直接移除、新页面 0.14s 淡入，既没有突兀的跳变，也不拖时间。

             KeepAlive 只保资源搜索页：搜出结果 → 点开一部片子 → 返回，
             结果和滚动位置都还在。别的页面进来就该是新的，不需要缓存。 -->
        <RouterView v-slot="{ Component }">
          <Transition name="page">
            <KeepAlive :include="['SearchPage']">
              <component :is="Component" />
            </KeepAlive>
          </Transition>
        </RouterView>
      </main>
    </div>

    <MobileTabBar />
    <CommandPalette />
    <ToastHost />
  </div>
</template>

<style scoped>
.shell {
  min-height: 100%;
}
.main {
  min-height: 100%;
  padding-left: var(--sidebar-w);
  transition: padding-left 0.28s var(--ease-out-fluid);
}
/* 折叠态以前靠 html:has(.sidebar.is-collapsed) 匹配。:has() 会在每次 DOM
   变动时重新扫整棵树，列表页卡片多时样式重算很贵；直接用状态 class 更稳。 */
.main.is-rail {
  padding-left: 72px;
}
.content {
  max-width: 1360px;
  margin: 0 auto;
  padding: 20px clamp(14px, 2.4vw, 32px) 56px;
}
.nav-scrim {
  position: fixed;
  inset: 0;
  z-index: 35;
  background: rgb(10 8 6 / 0.5);
}
@media (max-width: 900px) {
  .main,
  .main.is-rail {
    padding-left: 0;
  }
  /* 底部导航是 fixed 的，内容区得自己让出位置，否则最后一排卡片
     永远被那条 bar 盖住。 */
  .content {
    padding: 12px 12px calc(74px + env(safe-area-inset-bottom));
  }
}
</style>
