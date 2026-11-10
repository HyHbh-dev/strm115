<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import HPanel from '@/components/HPanel.vue'
import HTabs from '@/components/HTabs.vue'
import HEmpty from '@/components/HEmpty.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import TitleLink from '@/components/TitleLink.vue'

const router = useRouter()

/* ==================== 进行中 ==================== */

/**
 * 一行任务 = 内存里一个真的在跑的 Job。
 * 这一页以前是 mock：进度条随机、次数写死、取消只弹 toast。
 */
interface TaskRow {
  id: string
  kind: 'sync' | 'scrape' | 'subtitle'
  label: string
  library_id: number
  library: string
  phase: string
  done: number
  total: number
  percent: number
  message: string
  started_at: string
  detail?: Record<string, number>
  cancelable: boolean
}

interface TaskSummary {
  running: number
  offline_watching: number
  history_total: number
  failed_24h: number
  last_error?: string
}

interface LibraryOpt {
  id: number
  name: string
  collection_type?: string
  last_sync_at?: string
}

const rows = ref<TaskRow[]>([])
const summary = ref<TaskSummary>({ running: 0, offline_watching: 0, history_total: 0, failed_24h: 0 })
const libraries = ref<LibraryOpt[]>([])
const loading = ref(false)
const now = ref(Date.now())

const kindTone: Record<string, string> = {
  sync: 'badge-accent',
  scrape: 'badge-success',
  subtitle: 'badge-warning',
}

/** 各任务的阶段名不一样，合成一套人能看懂的说法。 */
const phaseText: Record<string, string> = {
  scan: '扫描目录',
  write: '写入文件',
  clean: '清理失效',
  emby: '通知 Emby',
  parse: '解析文件名',
  meta: '查询元数据',
  subs: '补字幕',
  run: '处理中',
  done: '已完成',
  error: '出错',
  canceled: '已取消',
}

const kindLabel = (k: string) => ({ sync: '同步', scrape: '刮削', subtitle: '补字幕' })[k] ?? k
const phaseLabel = (p: string) => phaseText[p] ?? p

/** 详情串：跟着任务类型取，不把同步的「目录数」显示在刮削上。 */
function detailText(t: TaskRow): string {
  const d = t.detail ?? {}
  if (t.kind === 'sync') {
    const parts = [`${d.files ?? 0} 个视频`, `新写 ${d.written ?? 0}`]
    if (d.removed) parts.push(`清理 ${d.removed}`)
    if (d.failed) parts.push(`失败 ${d.failed}`)
    return parts.join(' · ')
  }
  if (t.kind === 'scrape') {
    const parts = [`匹配 ${d.matched ?? 0}`, `未匹配 ${d.unmatched ?? 0}`]
    if (d.cached) parts.push(`缓存 ${d.cached}`)
    if (d.failed) parts.push(`失败 ${d.failed}`)
    return parts.join(' · ')
  }
  const parts = [`已存 ${d.saved ?? 0}`]
  if (d.missing) parts.push(`没找到 ${d.missing}`)
  if (d.failed) parts.push(`失败 ${d.failed}`)
  return parts.join(' · ')
}

/** 已跑时长。前端自己算，服务端只给开始时间，省一次刷新就变一次接口。 */
function elapsed(t: TaskRow): string {
  if (!t.started_at) return ''
  const start = Date.parse(t.started_at)
  if (Number.isNaN(start)) return ''
  const sec = Math.max(0, Math.floor((now.value - start) / 1000))
  if (sec < 60) return `${sec} 秒`
  const m = Math.floor(sec / 60)
  if (m < 60) return `${m} 分 ${sec % 60} 秒`
  return `${Math.floor(m / 60)} 小时 ${m % 60} 分`
}

async function loadTasks() {
  loading.value = true
  try {
    const r = await api.tasks.list()
    rows.value = r?.running ?? []
    summary.value = r?.summary ?? summary.value
    libraries.value = r?.libraries ?? []
  } catch (e: any) {
    toast.error('读取任务失败', e?.message)
  } finally {
    loading.value = false
    now.value = Date.now()
  }
}

async function cancelTask(t: TaskRow) {
  try {
    await api.tasks.cancel({ kind: t.kind, library_id: t.library_id })
    toast.success('已取消', `${t.library} · ${kindLabel(t.kind)}`)
    await loadTasks()
  } catch (e: any) {
    // 409 = 刚好在这几秒里跑完了。这不是错误，直接刷新一下。
    toast.warning('没有取消成功', e?.message)
    await loadTasks()
  }
}

/* ==================== 手动跑一轮 ==================== */

const pickLib = ref<number>(0)
const running = ref('')

async function start(kind: 'sync' | 'scrape', opts: { full?: boolean; refresh?: boolean } = {}) {
  const lib = pickLib.value || libraries.value[0]?.id
  if (!lib) {
    toast.warning('还没有媒体库', '先去「账号与媒体库」加一个')
    return
  }
  running.value = `${kind}:${lib}`
  try {
    await api.tasks.run({ kind, library_id: lib, ...opts })
    toast.success(kind === 'sync' ? '已开始同步' : '已开始刮削', '进度会出现在上面')
    await loadTasks()
    tab.value = 'jobs'
  } catch (e: any) {
    toast.error('启动失败', e?.message)
  } finally {
    running.value = ''
  }
}

/* ==================== 历史 ==================== */

interface Run {
  id: number
  library_id: number
  library_name: string
  trigger: string
  phase: string
  files: number
  written: number
  removed: number
  failed: number
  skipped: number
  message: string
  started_at: string
  finished_at: string
}

const runs = ref<Run[]>([])
const historyTotal = ref(0)
const historyLoading = ref(false)
const clearing = ref(false)

const triggerText: Record<string, string> = {
  full: '全量',
  incremental: '增量',
  auto: '自动',
  manual: '手动',
}

const runTone: Record<string, string> = {
  done: 'badge-success',
  error: 'badge-danger',
  canceled: 'badge-warning',
  running: 'badge-accent',
}
const runText: Record<string, string> = {
  done: '成功',
  error: '失败',
  canceled: '已取消',
  running: '进行中',
}

async function loadHistory() {
  historyLoading.value = true
  try {
    const r = await api.tasks.history({ limit: 100 })
    runs.value = r?.items ?? []
    historyTotal.value = r?.total ?? 0
  } catch (e: any) {
    toast.error('读取历史失败', e?.message)
  } finally {
    historyLoading.value = false
  }
}

function runDuration(r: Run): string {
  const a = Date.parse(r.started_at)
  const b = Date.parse(r.finished_at)
  if (Number.isNaN(a) || Number.isNaN(b) || b < a) return '—'
  const sec = Math.round((b - a) / 1000)
  if (sec < 60) return `${sec} 秒`
  return `${Math.floor(sec / 60)} 分 ${sec % 60} 秒`
}

/** 时间只显示到分钟：这一页看的是「什么时候跑的」，不是精确到秒的审计。 */
function shortTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** 重跑：用同一轮的方式再跑一次。全量轮次重跑仍然是全量。 */
async function rerun(r: Run) {
  try {
    await api.tasks.run({ kind: 'sync', library_id: r.library_id, full: r.trigger === 'full' })
    toast.success('已重新开始', `${r.library_name} · ${triggerText[r.trigger] ?? r.trigger}`)
    await loadTasks()
    tab.value = 'jobs'
  } catch (e: any) {
    toast.error('重跑失败', e?.message)
  }
}

async function clearHistory() {
  clearing.value = true
  try {
    const r = await api.tasks.clear()
    toast.success('历史已清空', `删除 ${r?.deleted ?? 0} 条（正在跑的那轮保留）`)
    await loadHistory()
    await loadTasks()
  } catch (e: any) {
    toast.error('清空失败', e?.message)
  } finally {
    clearing.value = false
  }
}

/* ==================== 整理记录 ==================== */

interface OrgRecord {
  id: number
  name: string
  title: string
  year: string
  confidence: number
  reason: string
  target: string
  message: string
  status: string
  updated_at: string
}
const orgRecords = ref<OrgRecord[]>([])
const orgTotal = ref(0)
const orgLoading = ref(false)

const orgStateText: Record<string, string> = {
  moved: '已整理',
  failed: '失败',
  skipped: '跳过',
  ignored: '已忽略',
}
const orgStateTone: Record<string, string> = {
  moved: 'badge-success',
  failed: 'badge-danger',
  skipped: 'badge-warning',
  ignored: '',
}

async function loadOrgRecords() {
  orgLoading.value = true
  try {
    const r = await api.organize.records({ limit: 20 })
    orgRecords.value = r?.items ?? []
    orgTotal.value = r?.total ?? 0
  } catch (e: any) {
    toast.error('读取整理记录失败', e?.message)
  } finally {
    orgLoading.value = false
  }
}

/* ==================== 页签与轮询 ==================== */

const tab = ref('jobs')
const tabs = computed(() => [
  { value: 'jobs', label: '进行中', count: rows.value.length },
  { value: 'history', label: '历史', count: historyTotal.value },
  { value: 'records', label: '整理记录', count: orgTotal.value, countTone: 'warning' as const },
])

/**
 * 只在「进行中」页签轮询。
 *
 * 3 秒一次：任务本身动辄几分钟，再快只是白刷；但太慢会让「取消」看起来
 * 没生效。切到别的页签立刻停，别让一个后台页面一直打接口。
 */
let timer: number | undefined
function syncPolling() {
  if (timer) {
    window.clearInterval(timer)
    timer = undefined
  }
  if (tab.value === 'jobs') {
    timer = window.setInterval(() => {
      if (document.hidden) return // 后台标签页别刷，回来后自然续上
      now.value = Date.now()
      loadTasks()
    }, 3000)
  }
}

onMounted(async () => {
  await loadTasks()
  if (!libraries.value.length) await loadTasks()
  syncPolling()
})

onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
})

watch(tab, (v) => {
  if (v === 'jobs') loadTasks()
  if (v === 'history') loadHistory()
  if (v === 'records') loadOrgRecords()
  syncPolling()
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <!-- 顶部汇总：三个数字回答「现在忙不忙、有没有一直在坏」 -->
    <div class="sum">
      <div class="sum-item">
        <span class="sum-num">{{ summary.running }}</span>
        <span class="muted small">正在跑</span>
      </div>
      <div class="sum-item">
        <span class="sum-num">{{ summary.offline_watching }}</span>
        <span class="muted small">离线任务在等</span>
      </div>
      <div class="sum-item">
        <span class="sum-num" :class="{ 'warn': summary.failed_24h > 0 }">{{ summary.failed_24h }}</span>
        <span class="muted small">24 小时内失败</span>
      </div>
      <div class="grow" />
      <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="loadTasks">
        <AppIcon name="refresh" :size="14" />{{ loading ? '刷新中…' : '刷新' }}
      </button>
    </div>

    <p v-if="summary.last_error" class="hint-warn small">
      <AppIcon name="alert" :size="14" />最近一次失败：{{ summary.last_error }}
    </p>

    <HTabs v-model="tab" :items="tabs" />

    <!-- ==================== 进行中 ==================== -->
    <template v-if="tab === 'jobs'">
      <HPanel
        title="正在跑的任务"
        desc="进度来自任务本身的内存状态，3 秒刷新一次"
        flush
      >
        <div v-if="rows.length" class="job-list">
          <div v-for="t in rows" :key="t.id" class="job">
            <div class="row-between wrap" style="gap: 8px">
              <div class="row wrap" style="gap: 8px">
                <span class="badge" :class="kindTone[t.kind]">{{ t.label }}</span>
                <strong class="small">{{ t.library }}</strong>
                <span class="muted small">· {{ phaseLabel(t.phase) }}</span>
              </div>
              <div class="row" style="gap: 8px">
                <span class="muted small mono">{{ elapsed(t) }}</span>
                <button
                  v-if="t.cancelable"
                  class="btn btn-sm btn-ghost"
                  style="color: var(--danger)"
                  @click="cancelTask(t)"
                >
                  <AppIcon name="close" :size="14" />取消
                </button>
              </div>
            </div>

            <div class="bar" :class="{ indeterminate: t.percent < 0 }" style="margin-top: 8px">
              <i v-if="t.percent >= 0" :style="{ width: Math.max(t.percent, 2) + '%' }" />
              <i v-else />
            </div>

            <div class="row-between small muted" style="margin-top: 6px">
              <span class="truncate">{{ t.message || phaseLabel(t.phase) }}</span>
              <span class="mono">
                <template v-if="t.percent >= 0">{{ t.done }} / {{ t.total }} · {{ t.percent }}%</template>
                <template v-else>{{ detailText(t) }}</template>
              </span>
            </div>
            <p v-if="t.percent >= 0" class="muted small" style="margin: 2px 0 0">{{ detailText(t) }}</p>
          </div>
        </div>

        <div v-else class="empty" style="padding: 30px 0">
          <p class="empty-title">{{ loading ? '读取中…' : '没有在跑的任务' }}</p>
          <p class="empty-desc">
            同步、刮削、补字幕都在这里排队显示。下面可以直接起一轮。
          </p>
        </div>
      </HPanel>

      <HPanel title="手动跑一轮" desc="平时靠定时任务，想立刻见效就在这里点">
        <div class="row wrap" style="gap: 10px; align-items: flex-end">
          <label class="col" style="gap: 6px">
            <span class="muted small">媒体库</span>
            <!-- 下拉固定宽度：input 默认 width:100%，放进 flex 行会把按钮挤到第二行 -->
            <select v-model.number="pickLib" class="input" style="width: 220px">
              <option v-for="l in libraries" :key="l.id" :value="l.id">{{ l.name }}</option>
            </select>
          </label>
          <button
            class="btn btn-primary"
            :disabled="!!running || !libraries.length"
            @click="start('sync')"
          >
            <AppIcon name="refresh" :size="14" />开始同步
          </button>
          <button
            class="btn btn-tertiary"
            :disabled="!!running || !libraries.length"
            @click="start('sync', { full: true })"
          >
            全量同步
          </button>
          <button
            class="btn btn-tertiary"
            :disabled="!!running || !libraries.length"
            @click="start('scrape')"
          >
            <AppIcon name="image" :size="14" />开始刮削
          </button>
          <button
            class="btn btn-tertiary"
            :disabled="!!running || !libraries.length"
            @click="start('scrape', { refresh: true })"
          >
            刮削（忽略缓存）
          </button>
        </div>
        <p class="muted small" style="margin-top: 12px">
          「全量」会重写每个 .strm 的修改时间，让 Emby 整库重扫 ——
          换播放域名、改命名规则之后要的就是这个效果，平时用增量就够。
          「忽略缓存」用于换了数据源（比如刚配好 TMDB 想补演员头像）。
        </p>
        <p v-if="!libraries.length" class="muted small">
          还没有媒体库，先去
          <RouterLink class="link" to="/libraries">账号与媒体库</RouterLink>加一个。
        </p>
      </HPanel>
    </template>

    <!-- ==================== 历史 ==================== -->
    <template v-else-if="tab === 'history'">
      <HPanel
        title="同步历史"
        desc="每跑一轮记一行，重启不丢；刮削与补字幕跑完只留结果，没有流水账"
        flush
      >
        <template #actions>
          <button class="btn btn-sm btn-tertiary" :disabled="historyLoading" @click="loadHistory">
            <AppIcon name="refresh" :size="14" />刷新
          </button>
          <button
            class="btn btn-sm btn-ghost"
            style="color: var(--danger)"
            :disabled="clearing || !runs.length"
            @click="clearHistory"
          >
            <AppIcon name="trash" :size="14" />清空
          </button>
        </template>

        <div v-if="!runs.length" class="empty" style="padding: 30px 0">
          <p class="empty-title">{{ historyLoading ? '读取中…' : '还没有跑过同步' }}</p>
          <p class="empty-desc">在「进行中」里起一轮，或者等定时任务触发</p>
        </div>

        <div v-else class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>媒体库</th>
                <th style="width: 80px">方式</th>
                <th style="width: 130px">开始</th>
                <th style="width: 100px">用时</th>
                <th>结果</th>
                <th style="width: 150px">统计</th>
                <th style="width: 90px">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in runs" :key="r.id">
                <td>
                  <strong class="small">{{ r.library_name || '库 #' + r.library_id }}</strong>
                  <div class="muted small mono">#{{ r.id }}</div>
                </td>
                <td>
                  <span class="badge">{{ triggerText[r.trigger] ?? r.trigger }}</span>
                </td>
                <td class="mono small">{{ shortTime(r.started_at) }}</td>
                <td class="mono small">{{ runDuration(r) }}</td>
                <td>
                  <span class="badge" :class="runTone[r.phase]">{{ runText[r.phase] ?? r.phase }}</span>
                  <div v-if="r.message" class="muted small" style="margin-top: 3px">{{ r.message }}</div>
                </td>
                <td class="mono small">
                  {{ r.written }} 写<template v-if="r.removed"> · {{ r.removed }} 删</template>
                  <div v-if="r.failed" class="small" style="color: var(--danger)">
                    {{ r.failed }} 失败
                  </div>
                </td>
                <td>
                  <button class="btn btn-sm btn-ghost" title="用同样的方式再跑一轮" @click="rerun(r)">
                    <AppIcon name="refresh" :size="14" />重跑
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </HPanel>
    </template>

    <!-- ==================== 整理记录 ==================== -->
    <template v-else-if="tab === 'records'">
      <HPanel title="整理记录" desc="最近 20 条；忽略 / 重试在「自动整理」页处理" flush>
        <template #actions>
          <button class="btn btn-sm btn-tertiary" @click="router.push('/organize')">
            <AppIcon name="organize" :size="14" />去跑一轮整理
          </button>
        </template>

        <div v-if="!orgRecords.length" class="empty" style="padding: 28px 0">
          <p class="empty-title">{{ orgLoading ? '读取中…' : '还没有记录' }}</p>
          <p class="empty-desc">在「自动整理」里跑一轮之后，这里会列出每一部的识别依据</p>
        </div>

        <div v-else class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>原始名称</th>
                <th>识别为</th>
                <th class="num">置信度</th>
                <th>依据</th>
                <th>状态</th>
                <th>目标 / 说明</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in orgRecords" :key="r.id">
                <td class="mono small" style="max-width: 240px">
                  <span class="truncate" style="display: block">{{ r.name }}</span>
                </td>
                <td class="small">
                  <TitleLink :title="r.title" :year="r.year">
                    {{ r.title || '—' }}
                  </TitleLink><template v-if="r.year"> ({{ r.year }})</template>
                </td>
                <td class="num mono small">{{ r.confidence }}%</td>
                <td class="muted small">{{ r.reason }}</td>
                <td>
                  <span class="badge" :class="orgStateTone[r.status]">
                    {{ orgStateText[r.status] ?? r.status }}
                  </span>
                </td>
                <td class="small muted">
                  <span class="truncate" style="display: block; max-width: 300px">
                    {{ r.target || r.message }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </HPanel>
    </template>
  </div>
</template>

<style scoped>
.sum {
  display: flex;
  align-items: center;
  gap: 26px;
  padding: 14px 16px;
  border-radius: var(--r-md);
  border: 1px solid var(--border-secondary);
  background: var(--surface-secondary);
}
.sum-item {
  display: flex;
  align-items: baseline;
  gap: 6px;
}
.sum-num {
  font-size: 22px;
  font-weight: 650;
  letter-spacing: -0.02em;
}
.sum-num.warn {
  color: var(--warning);
}
/* 总量未知时的流动条；具体百分比仍然由 .bar > i 的宽度决定 */
.bar.indeterminate > i {
  width: 38%;
  animation: slide 1.5s ease-in-out infinite;
}
@keyframes slide {
  0% {
    margin-left: -38%;
  }
  100% {
    margin-left: 100%;
  }
}
.job-list {
  display: flex;
  flex-direction: column;
}
.job {
  padding: 14px 16px;
  border-bottom: 1px solid var(--border-secondary);
}
.job:last-child {
  border-bottom: 0;
}
.hint-warn {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--warning);
  margin: 0;
}
.link {
  color: var(--accent);
  text-decoration: none;
}
.link:hover {
  text-decoration: underline;
}
</style>
