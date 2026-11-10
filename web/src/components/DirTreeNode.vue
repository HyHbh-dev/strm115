<script setup lang="ts">
/**
 * 网盘目录树的一个节点，递归渲染自己。
 *
 * 只负责「画 + 抛事件」，不自己发请求：展开要拉哪一页由父组件决定，
 * 因为同一棵树在文件页被用了两次（左边导航、移动对话框里选目标目录），
 * 数据源不同但节点长一样。
 */
import AppIcon from './AppIcon.vue'
import type { DirNode } from '@/data/tree'

const props = withDefaults(defineProps<{ node: DirNode; current: string; depth?: number }>(), {
  depth: 0,
})

const emit = defineEmits<{
  pick: [node: DirNode]
  toggle: [node: DirNode]
}>()

const indent = () => `${8 + (props.depth ?? 0) * 14}px`
</script>

<template>
  <div>
    <button
      class="node"
      :class="{ 'is-on': current === node.id }"
      :style="{ paddingLeft: indent() }"
      @click="emit('pick', node)"
    >
      <span class="chev" :class="{ 'is-open': node.open }" @click.stop="emit('toggle', node)">
        <AppIcon name="chevronRight" :size="12" />
      </span>
      <AppIcon name="folder" :size="14" />
      <span class="grow truncate">{{ node.name }}</span>
      <span v-if="node.loading" class="muted small">…</span>
    </button>

    <template v-if="node.open">
      <DirTreeNode
        v-for="k in node.kids ?? []"
        :key="k.id"
        :node="k"
        :current="current"
        :depth="(depth ?? 0) + 1"
        @pick="emit('pick', $event)"
        @toggle="emit('toggle', $event)"
      />
      <div
        v-if="node.loaded && !(node.kids ?? []).length"
        class="empty"
        :style="{ paddingLeft: `${8 + (depth ?? 0) * 14 + 20}px` }"
      >
        空目录
      </div>
    </template>
  </div>
</template>

<style scoped>
.node {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  height: 32px;
  padding: 0 8px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--foreground);
  font-size: 13px;
  text-align: left;
}
.node:hover {
  background: var(--surface-secondary);
}
.node.is-on {
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 600;
}
.chev {
  display: grid;
  place-items: center;
  width: 14px;
  color: var(--muted);
  transition: transform 0.2s var(--ease-out-fluid);
}
.chev.is-open {
  transform: rotate(90deg);
}
.empty {
  height: 24px;
  line-height: 24px;
  font-size: 12px;
  color: var(--muted);
}
</style>
