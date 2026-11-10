<script setup lang="ts">
export interface TabItem {
  value: string
  label: string
  count?: number
  countTone?: 'warning' | 'danger'
}

defineProps<{ modelValue: string; items: TabItem[] }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
</script>

<template>
  <div class="tabs" role="tablist">
    <button
      v-for="t in items"
      :key="t.value"
      role="tab"
      class="tab"
      :class="{ 'is-active': t.value === modelValue }"
      :aria-selected="t.value === modelValue"
      @click="emit('update:modelValue', t.value)"
    >
      {{ t.label }}
      <span
        v-if="t.count !== undefined && t.count > 0"
        class="tab-count"
        :class="t.countTone ? `tone-${t.countTone}` : ''"
        >{{ t.count }}</span
      >
    </button>
  </div>
</template>
