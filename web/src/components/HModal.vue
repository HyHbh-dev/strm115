<script setup lang="ts">
import { onBeforeUnmount, watch } from 'vue'
import AppIcon from './AppIcon.vue'

const props = withDefaults(
  defineProps<{ modelValue: boolean; title?: string; size?: 'md' | 'lg'; closable?: boolean }>(),
  { title: '', size: 'md', closable: true },
)
const emit = defineEmits<{ 'update:modelValue': [boolean] }>()

function close() {
  if (props.closable) emit('update:modelValue', false)
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') close()
}

watch(
  () => props.modelValue,
  (v) => {
    document.body.style.overflow = v ? 'hidden' : ''
    if (v) window.addEventListener('keydown', onKey)
    else window.removeEventListener('keydown', onKey)
  },
)

onBeforeUnmount(() => {
  document.body.style.overflow = ''
  window.removeEventListener('keydown', onKey)
})
</script>

<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="modelValue" class="overlay" @click.self="close">
        <div class="modal" :class="{ 'modal-lg': size === 'lg' }" role="dialog" aria-modal="true">
          <header class="modal-head">
            <div>
              <h2>{{ title }}</h2>
              <p v-if="$slots.desc" class="muted small" style="margin-top: 4px">
                <slot name="desc" />
              </p>
            </div>
            <button v-if="closable" class="btn btn-icon" aria-label="关闭" @click="close">
              <AppIcon name="close" />
            </button>
          </header>
          <div class="modal-body"><slot /></div>
          <footer v-if="$slots.footer" class="modal-foot"><slot name="footer" /></footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
