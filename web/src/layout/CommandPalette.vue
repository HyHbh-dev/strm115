<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import { navFlat } from '@/data/nav'
import { ui } from '@/stores/ui'

const router = useRouter()
const keyword = ref('')
const inputRef = ref<HTMLInputElement | null>(null)

const results = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return navFlat.filter((n) => !n.hideInNav)
  return navFlat.filter(
    (n) =>
      !n.hideInNav &&
      (n.label.toLowerCase().includes(k) ||
        n.desc.toLowerCase().includes(k) ||
        n.path.includes(k)),
  )
})

watch(
  () => ui.commandOpen,
  async (open) => {
    if (open) {
      keyword.value = ''
      await nextTick()
      inputRef.value?.focus()
    }
  },
)

function go(path: string) {
  ui.commandOpen = false
  router.push(path)
}

function submit() {
  if (results.value[0]) go(results.value[0].path)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="ui.commandOpen" class="overlay cmd-overlay" @click.self="ui.commandOpen = false">
        <div class="cmd glass" role="dialog" aria-label="全局搜索">
          <div class="cmd-input">
            <AppIcon name="search" :size="17" />
            <input
              ref="inputRef"
              v-model="keyword"
              placeholder="跳到页面、搜索库或片目…"
              @keydown.enter.prevent="submit"
              @keydown.esc="ui.commandOpen = false"
            />
            <kbd>ESC</kbd>
          </div>
          <div class="cmd-list">
            <p class="cmd-group">页面</p>
            <button v-for="r in results" :key="r.name" class="cmd-item" @click="go(r.path)">
              <span class="avatar"><AppIcon :name="r.icon" :size="15" /></span>
              <span class="col" style="gap: 0">
                <strong>{{ r.label }}</strong>
                <small class="muted">{{ r.desc }}</small>
              </span>
              <span class="mono muted cmd-path">{{ r.path }}</span>
            </button>
            <p v-if="!results.length" class="muted small" style="padding: 18px 12px">
              没有匹配的页面
            </p>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.cmd-overlay {
  place-items: start center;
  padding-top: 12vh;
}
.cmd {
  width: min(600px, 100%);
  border-radius: var(--r-xl);
  overflow: hidden;
}
.cmd-input {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 16px;
  border-bottom: 1px solid var(--separator);
  color: var(--muted);
}
.cmd-input input {
  flex: 1;
  border: 0;
  background: none;
  outline: none;
  font-size: 14px;
  color: var(--foreground);
}
.cmd-input kbd {
  padding: 1px 5px;
  border-radius: 5px;
  border: 1px solid var(--border);
  font-family: var(--font-mono);
  font-size: 10.5px;
}
.cmd-list {
  max-height: 52vh;
  overflow: auto;
  padding: 6px;
}
.cmd-group {
  padding: 8px 10px 4px;
  font-size: 10.5px;
  font-weight: 700;
  letter-spacing: 0.08em;
  color: var(--muted);
}
.cmd-item {
  display: flex;
  align-items: center;
  gap: 11px;
  width: 100%;
  padding: 9px 10px;
  border: 0;
  border-radius: var(--r-sm);
  background: none;
  text-align: left;
}
.cmd-item:hover {
  background: var(--surface-secondary);
}
.cmd-item .avatar {
  width: 30px;
  height: 30px;
  border-radius: 9px;
}
.cmd-item strong {
  font-size: 13.5px;
  font-weight: 600;
}
.cmd-path {
  margin-left: auto;
  font-size: 11px;
}
</style>
