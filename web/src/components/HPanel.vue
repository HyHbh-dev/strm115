<script setup lang="ts">
defineProps<{
  title?: string
  desc?: string
  /** 去掉默认内边距，给表格/网格自己控制 */
  flush?: boolean
}>()
</script>

<template>
  <section class="panel">
    <header v-if="title || $slots.actions" class="panel-head">
      <div>
        <h2 v-if="title">{{ title }}</h2>
        <p v-if="desc" class="desc">{{ desc }}</p>
      </div>
      <div class="row" style="flex: none">
        <slot name="actions" />
      </div>
    </header>
    <div :class="flush ? '' : 'panel-body'">
      <slot />
    </div>
    <footer v-if="$slots.footer" class="panel-foot">
      <slot name="footer" />
    </footer>
  </section>
</template>
