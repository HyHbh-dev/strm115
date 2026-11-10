<script setup lang="ts">
/**
 * 我的追更（用户专有追更日历）。
 *
 * 和旧的全局 /api/calendar 周历不同：这里不问「本周有哪些在播剧」，只列
 * 「我已经订阅的剧集的每一集」，按 TMDB 单集 air_date 排。没播到是 pending，
 * 到期没搜到每天重试一次，提交离线就 done；TMDB 没给播出日的集单独一组。
 *
 * 数据由 /api/subscriptions/calendar?start=&days=28 一次给四周。前端按周一
 * 切成 7 天一屏，翻出已拉范围才重新请求，范围内的翻页只是本地重画。
 */
import { computed, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HEmpty from '@/components/HEmpty.vue'
import HPanel from '@/components/HPanel.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { fmtDay, pad2 } from '@/utils/format'

/** /api/subscriptions/calendar 返回的单集（字段名对齐 Go 的 json tag） */
interface SubEpisode {
  id: number
  subscription_id: number
  season: number
  episode: number
  /** 'YYYY-MM-DD' 或空；空的在 undated 里单独给 */
  air_date: string
  /** 单集标题 */
  name: string
  /** pending 待播 / waiting 待搜 / done 已入库 / skipped 起播线之前不搜（列表已过滤） */
  status: string
  try_count: number
  next_try_at: string
  found_at: string
  last_result: string
  /** 剧名 / 原名，从订阅带过来，用来按整部剧搜资源 */
  title: string
  original_title: string
  tmdb_id: number
  sub_state: string
}

interface CalendarPayload {
  start: string
  end: string
  days: number
  items: SubEpisode[]
  undated: SubEpisode[]
  total: number
}

interface Section {
  key: string
  weekday: string
  label: string
  isToday: boolean
  undated: boolean
  items: SubEpisode[]
}

/**
 * 父页面 MediaTransferPage 的 onCalendarFind 签名是 (q: string, en?: string)，
 * 是位置参数而不是对象 —— 这里按它本来的形状发射，否则父页面会拿到
 * 「[object Object]」去搜资源。
 */
const emit = defineEmits<{
  (e: 'find', q: string, en?: string): void
}>()

const loading = ref(false)
const refreshing = ref(false)
const errorMsg = ref('')
const data = ref<CalendarPayload | null>(null)

/** 本次请求覆盖的日期范围（含端点），以及当前显示的这一周的周一 */
const rangeStart = ref('')
const rangeEnd = ref('')
const weekStart = ref('')

/** 丢弃过期响应：翻页 / 刷新连点时，只认最后一次请求的结果 */
let seq = 0

/* ---------------- 日期小工具 ---------------- */
const WEEKDAYS = ['周一', '周二', '周三', '周四', '周五', '周六', '周日']

/** 'YYYY-MM-DD' → 本地时间的 Date（不用 new Date(str)，那是 UTC 会差一天） */
function parseDay(s: string): Date {
  const [y, m, d] = s.split('-').map(Number)
  return new Date(y || 1970, (m || 1) - 1, d || 1)
}

function addDays(d: Date, n: number): Date {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate())
  x.setDate(x.getDate() + n)
  return x
}

function mondayOf(d: Date): Date {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate())
  x.setDate(x.getDate() - ((x.getDay() + 6) % 7))
  return x
}

function thisMonday(): string {
  return fmtDay(mondayOf(new Date()))
}

/* ---------------- 拉取 ---------------- */
async function load(start: string): Promise<boolean> {
  const my = ++seq
  errorMsg.value = ''
  loading.value = true
  try {
    const r = (await api.subscriptions.calendar(start, 28)) as CalendarPayload
    if (my !== seq) return false
    data.value = r
    rangeStart.value = r.start || start
    rangeEnd.value =
      r.end || fmtDay(addDays(parseDay(rangeStart.value), (r.days || 28) - 1))
    return true
  } catch (e: any) {
    if (my !== seq) return false
    errorMsg.value = e?.message || '追更日历加载失败'
    toast.warning('追更日历加载失败', errorMsg.value)
    return false
  } finally {
    if (my === seq) loading.value = false
  }
}

/** 目标周是否完整落在已拉的四周里（YYYY-MM-DD 可直接按字典序比） */
function inRange(monday: string): boolean {
  const end = fmtDay(addDays(parseDay(monday), 6))
  return !!rangeStart.value && monday >= rangeStart.value && end <= rangeEnd.value
}

async function gotoWeek(monday: string) {
  if (!data.value || !inRange(monday)) {
    if (!(await load(monday))) return
  }
  weekStart.value = monday
}

function shiftWeek(n: number) {
  void gotoWeek(fmtDay(addDays(parseDay(weekStart.value), n * 7)))
}

function goThisWeek() {
  void gotoWeek(thisMonday())
}

/** 「刷新」：保持当前范围起点重拉一次日历数据，不改动正在看的这一周 */
function reloadCalendar() {
  void load(rangeStart.value || weekStart.value)
}

onMounted(async () => {
  weekStart.value = thisMonday()
  // 起点往前放一周：这样「上一周」第一次翻页就还在已拉范围内，不联网。
  await load(fmtDay(addDays(parseDay(weekStart.value), -7)))
})

/* ---------------- 分组与统计 ---------------- */
const undated = computed<SubEpisode[]>(() => data.value?.undated ?? [])
const allEpisodes = computed<SubEpisode[]>(() => [
  ...(data.value?.items ?? []),
  ...undated.value,
])
const total = computed(() => data.value?.total ?? allEpisodes.value.length)
const doneCount = computed(() => allEpisodes.value.filter((e) => e.status === 'done').length)
const waitingCount = computed(() => allEpisodes.value.filter((e) => e.status === 'waiting').length)
const isEmpty = computed(
  () => !loading.value && !errorMsg.value && allEpisodes.value.length === 0,
)
const rangeText = computed(() => {
  if (!weekStart.value) return '载入中…'
  return `${weekStart.value} ~ ${fmtDay(addDays(parseDay(weekStart.value), 6))}`
})

const sections = computed<Section[]>(() => {
  if (!weekStart.value) return []
  const base = parseDay(weekStart.value)
  const byDate = new Map<string, SubEpisode[]>()
  for (const ep of data.value?.items ?? []) {
    if (!ep.air_date) continue
    const list = byDate.get(ep.air_date) ?? []
    list.push(ep)
    byDate.set(ep.air_date, list)
  }
  const today = fmtDay(new Date())
  const list: Section[] = Array.from({ length: 7 }, (_, i) => {
    const date = fmtDay(addDays(base, i))
    const d = parseDay(date)
    return {
      key: date,
      weekday: WEEKDAYS[i],
      label: `${d.getMonth() + 1}月${d.getDate()}日`,
      isToday: date === today,
      undated: false,
      items: byDate.get(date) ?? [],
    }
  })
  if (undated.value.length) {
    list.push({
      key: 'undated',
      weekday: 'TMDB 没给播出日',
      label: '',
      isToday: false,
      undated: true,
      items: undated.value,
    })
  }
  return list
})

/* ---------------- 展示 ---------------- */
const STATUS_BADGE: Record<string, { text: string; cls: string }> = {
  pending: { text: '待播', cls: 'badge-info' },
  waiting: { text: '待搜', cls: 'badge-warning' },
  done: { text: '已入库', cls: 'badge-success' },
  skipped: { text: '未追', cls: 'badge' },
}
const UNDATED_BADGE = { text: '未定档', cls: 'badge' }

function badgeOf(ep: SubEpisode, isUndated: boolean) {
  if (isUndated) return UNDATED_BADGE
  return STATUS_BADGE[ep.status] ?? { text: '未定档', cls: 'badge' }
}

function codeOf(ep: SubEpisode): string {
  return `S${pad2(ep.season || 0)}E${pad2(ep.episode || 0)}`
}

/** 按整部剧的名字搜（带 SxxExx 反而常搜不到），原名一并交给资源搜索 */
function findEpisode(ep: SubEpisode) {
  const q = ep.title || ep.original_title || ep.name || ''
  if (!q) return
  emit('find', q, ep.original_title || '')
}

/**
 * 「刷新集数」：对每条订阅去重后各调一次 refreshCalendar 重新拉 TMDB 集数集合。
 * 只锁这一个按钮，不阻塞翻页等交互；跑完后台重拉一次日历。
 */
async function refreshEpisodes() {
  if (refreshing.value) return
  const ids = [
    ...new Set(
      allEpisodes.value
        .map((e) => Number(e.subscription_id))
        .filter((n) => Number.isFinite(n) && n > 0),
    ),
  ]
  if (!ids.length) {
    toast.info('还没有可刷新的订阅')
    return
  }
  refreshing.value = true
  try {
    const rs = await Promise.allSettled(ids.map((id) => api.subscriptions.refreshCalendar(id)))
    const ok = rs.filter((r) => r.status === 'fulfilled').length
    toast.success(`已刷新 ${ok}/${ids.length} 条订阅的集数`)
    void load(rangeStart.value || weekStart.value)
  } finally {
    refreshing.value = false
  }
}
</script>

<template>
  <div class="col mycal" style="gap: 14px">
    <HPanel
      title="我的追更"
      desc="只列已订阅剧集的每一集，按 TMDB 单集播出日排；到期没搜到每天重试一次，提交离线即入库"
    >
      <template #actions>
        <div class="row wrap" style="gap: 6px">
          <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="shiftWeek(-1)">
            <AppIcon name="chevronLeft" :size="14" />上一周
          </button>
          <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="goThisWeek">本周</button>
          <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="shiftWeek(1)">
            下一周<AppIcon name="chevronRight" :size="14" />
          </button>
          <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="reloadCalendar">
            <AppIcon name="refresh" :size="14" />{{ loading ? '载入中…' : '刷新' }}
          </button>
        </div>
      </template>

      <!-- 统计：共 N 集 · 已入库 X · 待搜 Y -->
      <div class="mycal-stats row wrap" style="gap: 10px">
        <span class="badge badge-accent"><AppIcon name="calendar" :size="13" />{{ rangeText }}</span>
        <p class="mycal-summary">
          共 <b>{{ total }}</b> 集 · 已入库 <b>{{ doneCount }}</b> · 待搜 <b>{{ waitingCount }}</b>
        </p>
        <button
          class="btn btn-sm btn-tertiary mycal-refresh-btn"
          :disabled="refreshing"
          @click="refreshEpisodes"
        >
          <AppIcon name="refresh" :size="14" />{{ refreshing ? '刷新中…' : '刷新集数' }}
        </button>
      </div>

      <div v-if="loading && !data" class="mycal-loading">
        <div v-for="i in 4" :key="i" class="skeleton" style="height: 64px" />
      </div>
      <div v-else-if="errorMsg" class="mycal-error">
        <AppIcon name="alert" :size="15" />{{ errorMsg }}
        <button class="btn btn-sm btn-tertiary" @click="reloadCalendar">重试</button>
      </div>
      <HEmpty
        v-else-if="isEmpty"
        icon="calendar"
        title="还没有追更的剧集"
        desc="去「订阅」标签加一部剧，或在「资源搜索」里认片订阅后，这里会按 TMDB 播出日列出每一集"
      />

      <div v-else class="mycal-grid">
        <section
          v-for="s in sections"
          :key="s.key"
          class="mycal-day"
          :class="{ today: s.isToday, 'is-undated': s.undated }"
        >
          <div class="mycal-day-head">
            <template v-if="s.undated">
              <AppIcon name="calendar" :size="14" />
              <strong>{{ s.weekday }}</strong>
            </template>
            <template v-else>
              <strong>{{ s.weekday }}</strong>
              <span class="muted small">{{ s.label }}</span>
            </template>
            <span class="mycal-day-count">{{ s.items.length }}</span>
          </div>

          <div v-if="!s.items.length" class="mycal-none">—</div>
          <article
            v-for="ep in s.items"
            :key="ep.id || `${ep.subscription_id}-${ep.season}-${ep.episode}`"
            class="mycal-card"
            :title="ep.last_result || ''"
          >
            <div class="mycal-card-top">
              <span class="mycal-code">{{ codeOf(ep) }}</span>
              <span class="badge" :class="badgeOf(ep, s.undated).cls">
                {{ badgeOf(ep, s.undated).text }}
              </span>
            </div>
            <p class="mycal-title" :title="ep.title || ep.original_title">
              {{ ep.title || ep.original_title || '未命名剧集' }}
            </p>
            <p v-if="ep.name" class="muted small mycal-epname" :title="ep.name">{{ ep.name }}</p>
            <p v-if="ep.original_title" class="muted small mycal-orig" :title="ep.original_title">
              {{ ep.original_title }}
            </p>
            <div class="mycal-meta row wrap" style="gap: 6px">
              <span
                v-if="ep.try_count > 0"
                class="badge badge-danger"
                :title="ep.last_result || ''"
              >已试 {{ ep.try_count }} 次</span>
              <span v-if="ep.last_result" class="muted small mycal-result" :title="ep.last_result">
                {{ ep.last_result }}
              </span>
            </div>
            <button class="btn btn-sm btn-tertiary mycal-find" @click="findEpisode(ep)">
              <AppIcon name="search" :size="13" />搜这一集
            </button>
          </article>
        </section>
      </div>
    </HPanel>
  </div>
</template>

<style scoped>
.mycal {
  width: 100%;
  min-width: 0;
}

/* 统计条：范围徽章 + 计数 + 刷新集数，窄屏自然换行 */
.mycal-stats {
  align-items: center;
  padding-bottom: 12px;
  margin-bottom: 14px;
  border-bottom: 1px solid var(--separator);
}
.mycal-summary {
  color: var(--muted);
  font-size: 12.5px;
}
.mycal-summary b {
  color: var(--foreground);
  font-weight: 650;
}
.mycal-stats .mycal-refresh-btn {
  margin-left: auto;
}

.mycal-loading {
  display: grid;
  gap: 8px;
}
.mycal-error {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  color: var(--danger);
  font-size: 12.5px;
}

/* 一周七列；undated 跨整行，单独成块 */
.mycal-grid {
  display: grid;
  grid-template-columns: repeat(7, minmax(0, 1fr));
  gap: 8px;
  min-width: 0;
}
.mycal-day {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 8px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.mycal-day.today {
  border-color: var(--accent);
  background: var(--accent-soft);
}
.mycal-day.is-undated {
  grid-column: 1 / -1;
}
.mycal-day-head {
  display: flex;
  align-items: baseline;
  gap: 6px;
  flex-wrap: wrap;
  min-width: 0;
  padding-bottom: 8px;
  border-bottom: 1px solid var(--border);
  color: var(--muted);
  font-size: 11.5px;
}
.mycal-day-head strong {
  color: var(--foreground);
  font-size: 12.5px;
}
.mycal-day-count {
  margin-left: auto;
  font-size: 11px;
  color: var(--muted);
}
.mycal-none {
  text-align: center;
  padding: 12px 0;
  font-size: 12px;
  color: var(--muted);
}

.mycal-card {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 9px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
}
.mycal-card-top {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  min-width: 0;
}
.mycal-code {
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 700;
  color: var(--accent);
}
.mycal-title {
  font-size: 12.5px;
  font-weight: 600;
  line-height: 1.4;
  overflow-wrap: anywhere;
}
.mycal-epname,
.mycal-orig {
  overflow-wrap: anywhere;
}
.mycal-meta {
  min-width: 0;
}
.mycal-result {
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mycal-find {
  align-self: flex-start;
  margin-top: 2px;
}

@media (max-width: 900px) {
  .mycal-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .mycal-stats .mycal-refresh-btn {
    margin-left: 0;
  }
  .mycal-error .btn {
    margin-left: 0;
  }
}
@media (max-width: 560px) {
  .mycal-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
