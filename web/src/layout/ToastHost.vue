<script setup lang="ts">
import { computed } from 'vue'
import { dismissToast, toasts } from '@/stores/toast'
import AppIcon from '@/components/AppIcon.vue'

const icon = (tone: string) =>
  tone === 'success' ? 'check' : tone === 'danger' ? 'alert' : tone === 'warning' ? 'alert' : 'info'

const items = computed(() => toasts)
</script>

<template>
  <Teleport to="body">
    <div class="toaster">
      <TransitionGroup name="fade">
        <div v-for="t in items" :key="t.id" class="toast" :class="`toast-${t.tone}`">
          <span class="tone"><AppIcon :name="icon(t.tone)" :size="16" /></span>
          <div class="grow">
            <div>{{ t.text }}</div>
            <div v-if="t.desc" class="muted small">{{ t.desc }}</div>
          </div>
          <button class="btn btn-icon btn-sm" aria-label="关闭" @click="dismissToast(t.id)">
            <AppIcon name="close" :size="14" />
          </button>
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>
