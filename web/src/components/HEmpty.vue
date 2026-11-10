<script setup lang="ts">
import AppIcon from './AppIcon.vue'

withDefaults(defineProps<{ title?: string; desc?: string; icon?: string }>(), {
  title: '还没有内容',
  desc: '',
  icon: 'info',
})
</script>

<template>
  <div class="empty">
    <div class="empty-icon"><AppIcon :name="icon" :size="20" /></div>
    <p class="empty-title">{{ title }}</p>
    <p v-if="desc" class="empty-desc">{{ desc }}</p>
    <div v-if="$slots.default" style="margin-top: 6px"><slot /></div>
  </div>
</template>
