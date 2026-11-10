<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HPanel from '@/components/HPanel.vue'
import { logs as seed } from '@/data/mock'
import { toast } from '@/stores/toast'

type Line = { time: string; level: 'info' | 'warn' | 'error' | 'debug'; msg: string }

const lines = ref<Line[]>([...seed])
const level = ref<'all' | Line['level']>('all')
const keyword = ref('')
const live = ref(true)
const boxRef = ref<HTMLElement | null>(null)

const levels: { v: 'all' | Line['level']; l: string }[] = [
  { v: 'all', l: '全部' },
  { v: 'info', l: 'INFO' },
  { v: 'warn', l: 'WARN' },
  { v: 'error', l: 'ERROR' },
  { v: 'debug', l: 'DEBUG' },
]

const shown = computed(() =>
  lines.value.filter((l) => {
    if (level.value !== 'all' && l.level !== level.value) return false
    if (keyword.value && !l.msg.toLowerCase().includes(keyword.value.toLowerCase())) return false
    return true
  }),
)

const counts = computed(() => {
  const c = { info: 0, warn: 0, error: 0, debug: 0 }
  for (const l of lines.value) c[l.level]++
  return c
})

function scrollBottom() {
  nextTick(() => {
    if (boxRef.value) boxRef.value.scrollTop = boxRef.value.scrollHeight
  })
}

watch(shown, scrollBottom)

const pool: Line[] = [
  { time: '21:53:01', level: 'info', msg: 'sync: 剧集 增量轮次开始，事件 0' },
  { time: '21:53:06', level: 'debug', msg: '115: list /影视/剧集 cid=2849912 → 30 项' },
  { time: '21:53:11', level: 'info', msg: 'organize: 队列 +1 沙丘2 (2024)' },
  { time: '21:53:20', level: 'warn', msg: 'tmdb: 429 限流，退避 2s' },
  { time: '21:53:28', level: 'info', msg: 'scrape: 海报写入 poster.jpg 1.2MB' },
  { time: '21:53:35', level: 'error', msg: '115: 删除接口 429，5 分钟后重试' },
]

let timer: number | null = null
let idx = 0

function startLive() {
  stopLive()
  if (!live.value) return
  timer = window.setInterval(() => {
    if (document.hidden) return // 后台标签页别再堆 mock 日志
    const src = pool[idx % pool.length]
    idx++
    lines.value.push({ ...src, time: new Date().toTimeString().slice(0, 8) })
    if (lines.value.length > 400) lines.value = lines.value.slice(-300)
  }, 2600)
}

function stopLive() {
  if (timer) {
    clearInterval(timer)
    timer = null
  }
}

watch(live, (v) => (v ? startLive() : stopLive()))
startLive()
onBeforeUnmount(stopLive)

function clear() {
  lines.value = []
  toast.info('日志已清空')
}

function download() {
  toast.success('已导出 log 文件')
}
</script>

<template>
  <div class="logs">
    <HPanel flush>
      <div class="toolbar">
        <div class="row" style="gap: 6px">
          <button
            v-for="l in levels"
            :key="l.v"
            class="chip"
            :class="{ 'is-active': level === l.v }"
            @click="level = l.v"
          >
            {{ l.l }}
          </button>
        </div>
        <div class="search" style="max-width: 260px">
          <AppIcon name="search" :size="15" />
          <input v-model="keyword" class="input" placeholder="过滤关键字…" />
        </div>
        <div class="grow" />
        <label class="row" style="gap: 6px">
          <input v-model="live" type="checkbox" />
          <span class="small muted">实时跟随</span>
        </label>
        <button class="btn btn-sm btn-tertiary" @click="download">
          <AppIcon name="download" :size="14" />导出
        </button>
        <button class="btn btn-sm btn-ghost" style="color: var(--danger)" @click="clear">
          <AppIcon name="trash" :size="14" />清空
        </button>
      </div>

      <div ref="boxRef" class="log-box">
        <div v-for="(l, i) in shown" :key="i" class="log-line">
          <span class="log-time">{{ l.time }}</span>
          <span class="log-lv" :class="`lv-${l.level}`">{{ l.level }}</span>
          <span>{{ l.msg }}</span>
        </div>
        <p v-if="!shown.length" class="muted small" style="padding: 20px">没有匹配的日志</p>
      </div>
    </HPanel>

    <div class="col" style="gap: 14px">
      <HPanel title="统计" desc="当前缓冲">
        <div class="col" style="gap: 8px">
          <div class="row-between small">
            <span class="lv-info log-lv">INFO</span><span class="mono">{{ counts.info }}</span>
          </div>
          <div class="row-between small">
            <span class="lv-warn log-lv">WARN</span><span class="mono">{{ counts.warn }}</span>
          </div>
          <div class="row-between small">
            <span class="lv-error log-lv">ERROR</span><span class="mono">{{ counts.error }}</span>
          </div>
          <div class="row-between small">
            <span class="lv-debug log-lv">DEBUG</span><span class="mono">{{ counts.debug }}</span>
          </div>
        </div>
      </HPanel>

      <HPanel title="模块" desc="点一下只看这个模块">
        <div class="row wrap" style="gap: 6px">
          <button
            v-for="m in ['sync', 'organize', 'scrape', '115', 'emby', 'tmdb', 'webhook']"
            :key="m"
            class="chip"
            :class="{ 'is-active': keyword === m }"
            @click="keyword = keyword === m ? '' : m"
          >
            {{ m }}
          </button>
        </div>
      </HPanel>
    </div>
  </div>
</template>

<style scoped>
.logs {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 240px;
  gap: 16px;
  align-items: start;
}
.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--separator);
  flex-wrap: wrap;
}
.log-box {
  height: calc(100dvh - 260px);
  min-height: 320px;
  overflow: auto;
  padding: 8px 10px 16px;
}
@media (max-width: 900px) {
  .logs {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
