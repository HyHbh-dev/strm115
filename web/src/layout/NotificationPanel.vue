<script setup lang="ts">
import { useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import HEmpty from '@/components/HEmpty.vue'
import {
  items, unread, panelOpen, loading, markRead, markAllRead,
} from '@/stores/notifications'
import type { NotificationItem } from '@/stores/notifications'
import { fmtClock as formatTime } from '@/utils/format'

const router = useRouter()

function onItemClick(n: NotificationItem) {
  if (!n.read_at) markRead([n.id])
  if (n.link) router.push(n.link)
  panelOpen.value = false
}
</script>

<template>
  <section class="notif-panel">
    <header class="notif-head">
      <div class="notif-title">
        <strong>通知</strong>
        <span v-if="unread" class="badge badge-danger">{{ unread }}</span>
      </div>
      <button class="notif-all" :disabled="!unread" @click="markAllRead">
        <AppIcon name="check" :size="14" />全部已读
      </button>
    </header>

    <div class="notif-list">
      <div v-if="loading && !items.length" class="notif-loading">加载中…</div>
      <HEmpty
        v-else-if="!items.length"
        title="暂无通知"
        desc="有新的入库、同步或下载结果会显示在这里"
        icon="bell"
      />
      <template v-else>
        <button
          v-for="n in items"
          :key="n.id"
          class="notif-item"
          :class="{ 'is-unread': !n.read_at }"
          @click="onItemClick(n)"
        >
          <span class="notif-dot" :class="'lv-' + n.level" />
          <span class="notif-body">
            <span class="notif-row">
              <span class="notif-item-title">{{ n.title }}</span>
              <span class="notif-time">{{ formatTime(n.created_at) }}</span>
            </span>
            <span v-if="n.content" class="notif-content">{{ n.content }}</span>
          </span>
        </button>
      </template>
    </div>
  </section>
</template>

<style scoped>
.notif-panel {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  z-index: 50;
  width: 340px;
  max-width: calc(100vw - 28px);
  border-radius: var(--r-md);
  overflow: hidden;
  /* 和顶栏用户菜单一样用实色，避免 backdrop-filter 在公网访问时掉帧 */
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--overlay-shadow);
}
.notif-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 12px 14px;
  border-bottom: 1px solid var(--separator);
}
.notif-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 14px;
}
.notif-all {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 4px 6px;
  border: 0;
  border-radius: var(--r-sm);
  background: transparent;
  color: var(--muted);
  font-size: 12px;
  cursor: pointer;
}
.notif-all:hover:not(:disabled) {
  color: var(--accent);
  background: var(--surface-secondary);
}
.notif-all:disabled {
  opacity: 0.45;
  cursor: default;
}
.notif-list {
  max-height: 380px;
  overflow-y: auto;
}
.notif-item {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  width: 100%;
  padding: 11px 14px;
  border: 0;
  border-bottom: 1px solid var(--separator);
  background: transparent;
  color: var(--foreground);
  text-align: left;
  cursor: pointer;
}
.notif-item:last-child {
  border-bottom: 0;
}
.notif-item:hover {
  background: var(--surface-secondary);
}
.notif-item.is-unread {
  background: color-mix(in oklab, var(--accent) 6%, transparent);
}
.notif-dot {
  flex: none;
  width: 8px;
  height: 8px;
  margin-top: 5px;
  border-radius: 50%;
  background: var(--muted);
}
.notif-dot.lv-danger {
  background: var(--danger);
}
.notif-dot.lv-warning {
  background: var(--warning);
}
.notif-dot.lv-success {
  background: var(--success);
}
.notif-dot.lv-info {
  background: var(--info);
}
.notif-body {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.notif-row {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}
.notif-item-title {
  font-size: 13px;
  font-weight: 550;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.notif-time {
  flex: none;
  font-size: 11px;
  color: var(--muted);
}
.notif-content {
  font-size: 12px;
  color: var(--muted);
  line-height: 1.5;
  word-break: break-word;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.notif-loading {
  padding: 30px;
  text-align: center;
  font-size: 12.5px;
  color: var(--muted);
}

/* 窄屏：改成视口级定位。
   宽屏仍相对 .notif-wrap（absolute + right:0），定位父级是那个很窄的铃铛
   图标容器；393px 下 340px 宽的面板向左展开会顶出视口左边界，铃铛徽章和
   面板左半边被裁掉、右侧反而留白。fixed + left/right 让它吃满视口宽度，
   上面那套宽屏规则一字不改，DOM 结构也不变（点击外部关闭 / hover 逻辑照旧）。*/
@media (max-width: 900px) {
  .notif-panel {
    position: fixed;
    top: calc(62px + env(safe-area-inset-top));
    left: 12px;
    right: 12px;
    width: auto;
    max-width: none;
    /* 顶栏 62 + 底部导航 74 + 横幅余量 48 + 留白 24。
       多留的一段把面板底部停在版本横幅上方，避免被更高层级的浮层压住。 */
    max-height: calc(
      100dvh - 62px - 74px - 48px - 24px
      - env(safe-area-inset-top) - env(safe-area-inset-bottom)
    );
    display: flex;
    flex-direction: column;
  }
  .notif-head {
    flex: none;
  }
  .notif-list {
    flex: 1 1 auto;
    min-height: 0;
    max-height: none;
    overflow-y: auto;
    -webkit-overflow-scrolling: touch;
    overscroll-behavior: contain;
  }
}
</style>
