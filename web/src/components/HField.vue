<script setup lang="ts">
defineProps<{
  label: string
  hint?: string
  tip?: string
  required?: boolean
  error?: string
}>()
</script>

<template>
  <div class="field">
    <div class="field-label">
      <span>{{ label }}<span v-if="required" class="req"> *</span></span>
      <span v-if="tip" class="help-tip" :title="tip">?</span>
    </div>
    <div class="field-control">
      <slot />
      <p v-if="error" class="field-error">{{ error }}</p>
      <p v-else-if="hint" class="field-hint">{{ hint }}</p>
    </div>
  </div>
</template>
