<script setup lang="ts">
/**
 * 总览面板。
 *
 * 这一页原来是纯 mock：数字写死、按钮 setTimeout 假装在跑、海报是随机渐变色块。
 * 现在每个数字都能对上一个真实来源：
 *  - 库 / 台账 / 刮削统计 / 最新入库 → GET /api/dashboard（SQLite）
 *  - 正在跑的任务 → 同一个接口里的 jobs（进程内存里的 sync/scrape/subtitle Job）
 *  - Emby 连通性 → 并行调 GET /api/settings/server-status（会真的发一次请求）
 *  - 「立即全量同步」→ 逐库 POST /api/libraries/{id}/sync {force:true}
 *
 * Emby 单独一条请求是有意的：探活要走外部 HTTP，最坏情况几秒才回，
 * 塞进总览接口会拖慢首屏；分开后两边谁慢都不影响另一边先渲染。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import TitleLink from '@/components/TitleLink.vue'
import AppIcon from '@/components/AppIcon.vue'
import HEmpty from '@/components/HEmpty.vue'
import HPanel from '@/components/HPanel.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'

interface Service {
  key: string
  label: string
  state: string
  detail: string
  hint?: string
}
interface Job {
  kind: string
  library_id: number
  library: string
  phase: string
  done: number
  total: number
  percent: number
  message: string
  started_at: string
}
interface LibRow {
  id: number
  name: string
  account_name: string
  remote_path: string
  local_root: string
  collection_type: string
  files: number
  scraped: number
  unmatched: number
  subtitled: number
  last_sync_at: string
  last_sync_files: number
  status: string
  job?: Job
}
interface AccountQuota {
  id: number
  kind: string
  name: string
  used: number
  total: number
  ok: boolean
  note?: string
}
/** 只用到网盘容量（accounts）：系统资源卡片已移除，其余字段仍由后端返回，前端不再声明。 */
interface SystemStatus {
  accounts: AccountQuota[]
}
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
interface Recent {
  id: number
  library_id: number
  library: string
  name: string
  year: string
  type: string
  rating: string
  poster: string
  added: string
  rel_path: string
  episodes?: number
}
interface Dash {
  server: { version: string; uptime_sec: number; user: string; media_root: string; play_domain: string }
  services: Service[]
  emby: { kind: string; url: string; configured: boolean }
  totals: {
    libraries: number
    files: number
    bytes: number
    scraped: number
    unmatched: number
    failed: number
    subtitled: number
    running: number
  }
  libraries: LibRow[]
  recent: Recent[]
  jobs: Job[]
  runs: Run[]
  last_sync_at: string
  last_full_sync: string
  generated_at: string
}
interface EmbyStatus {
  kind: string
  url: string
  configured: boolean
  connected: boolean
  libraries: number
  version?: string
  server_name?: string
  error?: string
}

const data = ref<Dash | null>(null)
const emby = ref<EmbyStatus | null>(null)
const loading = ref(true)
const loadErr = ref('')
const refreshing = ref(false)
const syncing = ref(false)

const t = computed(
  () =>
    data.value?.totals ?? {
      libraries: 0,
      files: 0,
      bytes: 0,
      scraped: 0,
      unmatched: 0,
      failed: 0,
      subtitled: 0,
      running: 0,
    },
)
const recent = computed(() => data.value?.recent ?? [])
const jobs = computed(() => data.value?.jobs ?? [])
const runs = computed(() => data.value?.runs ?? [])

let loadInFlight: Promise<void> | null = null

function load(silent = false): Promise<void> {
  if (loadInFlight) return loadInFlight
  if (!silent) loading.value = true
  const task = (async () => {
    // 两个请求并行：Emby 探活慢的时候，库数据先到就先画出来。
    const [d, e] = await Promise.allSettled([api.dashboard.overview(), api.config.serverStatus()])
    if (d.status === 'fulfilled') {
      data.value = d.value as Dash
      loadErr.value = ''
    } else {
      loadErr.value = d.reason?.message || '读取总览数据失败'
    }
    emby.value = e.status === 'fulfilled' ? (e.value as EmbyStatus) : null
    loading.value = false
  })()
  loadInFlight = task
  void task.then(() => { if (loadInFlight === task) loadInFlight = null })
  return task
}

/* ---------------- 系统资源 / 网盘容量 ----------------
 * 单独一条请求：要问 115 / 夸克，慢的时候不能拖住概览首屏。 */
const sys = ref<SystemStatus | null>(null)

let sysInFlight: Promise<void> | null = null

function loadSys(): Promise<void> {
  // 系统状态要问 115 / 夸克容量，隧道上可能好几秒。30 秒定时器
  // 撞车时别叠请求，否则整个页面的交互都会被挂起的连接拖慢。
  if (sysInFlight) return sysInFlight
  const task = (async () => {
    try {
      sys.value = (await api.system.status()) as SystemStatus
    } catch {
      // 读不到容量就保留上一次结果，卡片停在「正在读取…」或旧值，30 秒后重试
    }
  })()
  sysInFlight = task
  void task.then(() => { if (sysInFlight === task) sysInFlight = null })
  return task
}

function pct(used: number, total: number): number {
  return total > 0 ? Math.min(100, Math.max(0, Math.round((used / total) * 100))) : 0
}
function tone(p: number): string {
  return p >= 90 ? 'danger' : p >= 75 ? 'warning' : 'ok'
}
function panLabel(kind: string): string {
  return kind === 'quarktv' ? '夸克TV' : kind === '115' ? '115' : kind
}

async function refresh() {
  refreshing.value = true
  await load(true)
  refreshing.value = false
  if (loadErr.value) toast.error('刷新失败', loadErr.value)
  else toast.info('概览已刷新')
}

let timer: number | undefined
let sysTimer: number | undefined
onMounted(async () => {
  await load()
  void loadSys()
  // 有任务在跑就 4 秒刷一次（进度条要动），闲着的时候完全不轮询。
  timer = window.setInterval(() => {
    if (document.hidden) return
    if (jobs.value.length) void load(true)
  }, 4000)
  // 系统资源 30 秒一刷，和后端缓存同频。
  sysTimer = window.setInterval(() => {
    if (!document.hidden) void loadSys()
  }, 30_000)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
  if (sysTimer) window.clearInterval(sysTimer)
})

/* ==================== 状态徽章 ==================== */
const badges = computed(() => {
  const out: { key: string; label: string; state: string; detail: string; hint?: string }[] = []
  const e = emby.value
  if (e) {
    out.push({
      key: 'emby',
      label: e.kind === 'jellyfin' ? 'Jellyfin' : 'Emby',
      state: e.connected ? 'ok' : e.configured ? 'warn' : 'off',
      detail: e.connected
        ? `${e.libraries} 个库${e.version ? ' · ' + e.version : ''}`
        : e.configured
          ? '已配置但连不上'
          : '未配置',
      hint: e.error || '',
    })
  }
  for (const s of data.value?.services ?? []) {
    out.push({ key: s.key, label: s.label, state: s.state, detail: s.detail, hint: s.hint })
  }
  const full = data.value?.last_full_sync
  out.push({
    key: 'full',
    label: '上次全量',
    state: full ? 'ok' : 'off',
    detail: fmtAgo(full),
    hint: full ? fmtTime(full) : '还没有跑过全量同步',
  })
  return out
})

const badgeClass = (state: string) =>
  state === 'ok' ? 'badge-success' : state === 'warn' ? 'badge-warning' : ''

/* ==================== 待办 ==================== */
const todo = computed(() => {
  const out: { tone: string; text: string; action: string; to: any }[] = []
  const d = data.value
  if (!d) return out
  if (!d.server.play_domain) {
    out.push({
      tone: 'badge-warning',
      text: '还没配播放地址，新同步出来的 STRM 可能播放器打不开',
      action: '去配置',
      to: { path: '/sync', query: { tab: 'config' } },
    })
  }
  if (t.value.unmatched > 0) {
    out.push({
      tone: 'badge-warning',
      text: `${t.value.unmatched} 个条目没匹配到影视信息（不会写 NFO）`,
      action: '去处理',
      to: { path: '/library', query: { tab: 'scrape' } },
    })
  }
  if (t.value.failed > 0) {
    out.push({
      tone: 'badge-danger',
      text: `${t.value.failed} 条刮削失败，可以单条重刮`,
      action: '去处理',
      to: { path: '/library', query: { tab: 'scrape' } },
    })
  }
  const never = d.libraries.filter((l) => !l.last_sync_at).length
  if (never > 0) {
    out.push({
      tone: 'badge-warning',
      text: `${never} 个库还没同步过，本地没有 STRM`,
      action: '去同步',
      to: { path: '/sync' },
    })
  }
  return out
})

/* ==================== 动作 ==================== */
async function syncAll() {
  const d = data.value
  if (!d?.libraries.length) {
    toast.warning('还没有媒体库', '先到「账号与媒体库」选一个 115 / 夸克TV 目录建库')
    return
  }
  if (!d.server.play_domain) {
    toast.warning('还没配播放地址', '先到 Strm 管理的「STRM 配置」填一个播放器能访问的地址')
    return
  }
  syncing.value = true
  let ok = 0
  try {
    for (const l of d.libraries) {
      try {
        await api.sync.start(l.id, { force: true, delete_stale: true })
        ok++
      } catch (e: any) {
        toast.error(`「${l.name}」启动失败`, e?.message)
      }
    }
    if (ok) {
      toast.success(`已提交 ${ok} 个库的全量同步`, '进度会显示在下面的任务概览里')
      await load(true)
    }
  } finally {
    syncing.value = false
  }
}

/* ==================== 格式化 ==================== */
function fmtTime(s?: string) {
  if (!s) return '—'
  const d = new Date(s)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
function fmtAgo(s?: string) {
  if (!s) return '从未'
  const d = new Date(s)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '从未'
  const ms = Date.now() - d.getTime()
  if (ms < 60_000) return '刚刚'
  const m = Math.floor(ms / 60_000)
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  const day = Math.floor(h / 24)
  return day < 30 ? `${day} 天前` : fmtTime(s).slice(0, 10)
}
function fmtBytes(n: number) {
  if (!n || n < 0) return '0'
  const unit = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let v = n
  let i = 0
  while (v >= 1024 && i < unit.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || i < 2 ? Math.round(v) : v.toFixed(1)} ${unit[i]}`
}
const kindLabel = (k: string) => ({ sync: '同步', scrape: '刮削', subtitle: '字幕' })[k] ?? k
const PHASES: Record<string, string> = {
  scan: '扫描目录',
  parse: '解析片名',
  meta: '查元数据',
  write: '写文件',
  clean: '清理失效',
  emby: '刷新 Emby',
  subs: '补字幕',
  run: '处理中',
  done: '完成',
  error: '失败',
  canceled: '已取消',
  idle: '空闲',
}
// 同步的进度条没有总量，右侧显示阶段中文名；有百分比时显示百分比更有用。
const jobPhase = (j: Job) =>
  j.percent >= 0 ? `${j.percent}%` : (PHASES[j.phase] ?? j.phase)
const phaseLabel = (p: string) =>
  ({ running: '进行中', done: '完成', error: '失败', canceled: '已取消' })[p] ?? p
const triggerLabel = (x: string) =>
  ({ full: '全量', incremental: '增量', auto: '自动', manual: '手动' })[x] ?? x
const libStatus = (s: string) =>
  ({
    ok: { text: '正常', cls: 'badge-success' },
    syncing: { text: '同步中', cls: 'badge-accent' },
    pending: { text: '未同步', cls: 'badge-warning' },
    error: { text: '上次失败', cls: 'badge-danger' },
  })[s] ?? { text: s, cls: '' }

const greeting = computed(() => {
  const h = new Date().getHours()
  const hi = h < 5 ? '凌晨好' : h < 9 ? '早上好' : h < 12 ? '上午好' : h < 18 ? '下午好' : '晚上好'
  const user = data.value?.server.user
  const who = user ? `，${user}` : ''
  if (!data.value) return `${hi}${who}`
  if (!t.value.libraries) return `${hi}${who}，先建一个媒体库吧`
  if (jobs.value.length) return `${hi}${who}，有 ${jobs.value.length} 个任务在跑`
  return `${hi}${who}，库里 ${t.value.files.toLocaleString()} 个片目都在位`
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <p v-if="loadErr" class="err small">{{ loadErr }}</p>

    <!-- 顶部：问候 + 服务状态 + 快捷动作 -->
    <div class="hero panel">
      <div class="col" style="gap: 8px; min-width: 0">
        <div class="row wrap" style="gap: 8px">
          <span
            v-for="b in badges"
            :key="b.key"
            class="badge"
            :class="badgeClass(b.state)"
            :title="b.hint || b.detail"
          >
            <i class="dot" />{{ b.label }}<template v-if="b.detail"> · {{ b.detail }}</template>
          </span>
          <span v-if="!data && loading" class="badge"><i class="dot" />正在读取状态…</span>
        </div>
        <h2 style="font-size: 21px">{{ greeting }}</h2>
        <p class="muted small">
          {{ t.libraries }} 个媒体库 · {{ t.files.toLocaleString() }} 个片目 ·
          {{ t.scraped.toLocaleString() }} 个已刮削
          <template v-if="data?.server.play_domain"> · 播放地址 {{ data.server.play_domain }}</template>
          <template v-else> · 还没配播放地址</template>
        </p>
      </div>
      <div class="row" style="flex: none">
        <button class="btn btn-tertiary" :disabled="refreshing" @click="refresh">
          <AppIcon name="refresh" :size="15" :class="{ spin: refreshing }" />刷新
        </button>
        <button class="btn btn-primary" :disabled="syncing || !t.libraries" @click="syncAll">
          <AppIcon name="sync" :size="15" />{{ syncing ? '提交中…' : '全量同步' }}
        </button>
      </div>
    </div>

    <!-- 统计 -->
    <div class="grid-4">
      <div class="stat">
        <span class="stat-label"><AppIcon name="local" :size="14" />媒体库</span>
        <span class="stat-value">{{ t.libraries }}<small>个</small></span>
        <span class="muted small">上次同步 {{ fmtAgo(data?.last_sync_at) }}</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="file" :size="14" />片目</span>
        <span class="stat-value">{{ t.files.toLocaleString() }}</span>
        <span class="muted small">台账里的 STRM 数</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="scrape" :size="14" />已刮削</span>
        <span class="stat-value">{{ t.scraped.toLocaleString() }}</span>
        <span class="muted small" :class="{ warn: t.unmatched || t.failed }">
          未匹配 {{ t.unmatched }} · 失败 {{ t.failed }}
        </span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="server" :size="14" />已入库体积</span>
        <span class="stat-value" style="font-size: 22px">{{ fmtBytes(t.bytes) }}</span>
        <span class="muted small">台账内视频文件合计</span>
      </div>
    </div>

    <!-- 网盘容量：独立接口，慢的时候不拖累上面的概览 -->
    <div>
      <HPanel title="网盘容量" desc="115 走 space_info，夸克走网页 /member（需配 Cookie）">
        <div v-if="!sys" class="muted small">正在读取…</div>
        <HEmpty v-else-if="!sys.accounts.length" title="还没有网盘账号" desc="去「账号」页登录 115 或夸克TV" icon="accounts" />
        <div v-else class="sys-list">
          <div v-for="q in sys.accounts" :key="q.id" class="sys-row">
            <div class="row-between small">
              <span>
                <span class="badge">{{ panLabel(q.kind) }}</span> {{ q.name }}
              </span>
              <b class="mono" :title="q.note || ''">
                <template v-if="q.ok">{{ fmtBytes(q.used) }} / {{ fmtBytes(q.total) }} · {{ pct(q.used, q.total) }}%</template>
                <template v-else class="muted">—</template>
              </b>
            </div>
            <div v-if="q.ok" class="sys-track">
              <i :class="tone(pct(q.used, q.total))" :style="{ width: pct(q.used, q.total) + '%' }" />
            </div>
            <div v-else class="muted small">{{ q.note || '暂时读不到容量' }}</div>
          </div>
        </div>
      </HPanel>
    </div>

    <!-- 建议处理：全部由上面的真实数字推出来，没有就不显示 -->
    <HPanel v-if="todo.length" title="建议处理" desc="按当前数据算出来的待办，不是固定文案">
      <ul class="todo">
        <li v-for="(x, i) in todo" :key="i">
          <span class="badge" :class="x.tone"><i class="dot" />待办</span>
          <span class="grow small">{{ x.text }}</span>
          <RouterLink class="btn btn-sm btn-ghost" :to="x.to">
            {{ x.action }}<AppIcon name="chevronRight" :size="14" />
          </RouterLink>
        </li>
      </ul>
    </HPanel>

    <div class="cols">
      <!-- 最新入库 -->
      <HPanel title="最新入库" desc="按刮削写入时间倒序；海报是已下载到本地的那一张" flush>
        <template #actions>
          <RouterLink class="btn btn-sm btn-ghost" :to="{ path: '/library' }">
            全部<AppIcon name="chevronRight" :size="14" />
          </RouterLink>
        </template>
        <div v-if="recent.length" class="poster-row">
          <RouterLink
            v-for="p in recent"
            :key="p.id"
            class="poster"
            :to="{ path: '/library' }"
            :title="`${p.name}（${p.year || '—'}）· ${p.library} · ${fmtTime(p.added)}`"
          >
            <div class="poster-art">
              <img v-if="p.poster" :src="p.poster" :alt="p.name" class="poster-img" loading="lazy" />
              <div v-else class="ph">{{ p.name }}</div>
              <span v-if="p.rating" class="rate">{{ Number(p.rating).toFixed(1) }}</span>
              <span v-if="p.type === 'series' && (p.episodes ?? 0) > 1" class="eps">
                {{ p.episodes }} 集
              </span>
            </div>
            <span class="poster-title">
              <TitleLink :type="p.type" :media-id="p.id" :title="p.name" :year="p.year">{{ p.name }}</TitleLink>
            </span>
            <span class="poster-sub">{{ p.year || '—' }} · {{ p.library }}</span>
          </RouterLink>
        </div>
        <div v-else-if="loading" class="pad muted small">正在读取…</div>
        <HEmpty
          v-else
          title="还没有刮削结果"
          desc="到影视库跑一次刮削，NFO、海报、简介会自动写到 .strm 旁边"
          icon="scrape"
        >
          <RouterLink class="btn btn-sm btn-primary" :to="{ path: '/library', query: { tab: 'scrape' } }">
            去刮削
          </RouterLink>
        </HEmpty>
      </HPanel>

      <!-- 任务概览 -->
      <HPanel title="任务概览" desc="在跑的来自内存里的任务，历史来自同步流水账" flush>
        <template #actions>
          <RouterLink class="btn btn-sm btn-ghost" to="/sync">Strm 管理</RouterLink>
        </template>
        <div class="job-wrap">
          <div v-if="jobs.length" class="col" style="gap: 12px">
            <div v-for="j in jobs" :key="j.kind + '-' + j.library_id">
              <div class="row-between small">
                <strong class="truncate">{{ j.library }} · {{ kindLabel(j.kind) }}</strong>
                <span class="mono muted">{{ jobPhase(j) }}</span>
              </div>
              <!-- 同步要边走边发现目录，总量事先不知道：用流动条表示「在动」，
                   而不是随便画一个百分比骗人。 -->
              <div class="bar" :class="{ indeterminate: j.percent < 0 }" style="margin-top: 5px">
                <i v-if="j.percent >= 0" :style="{ width: Math.max(j.percent, 2) + '%' }" />
                <i v-else />
              </div>
              <span class="muted small">
                {{ j.message || jobPhase(j) }}
                <template v-if="j.total"> · {{ j.done }}/{{ j.total }}</template>
                <template v-else-if="j.done"> · 已写 {{ j.done }}</template>
              </span>
            </div>
          </div>
          <p v-else class="muted small">当前没有正在跑的任务。</p>

          <p class="section-title" style="margin-top: 14px">最近同步轮次</p>
          <ul class="task-list">
            <li v-for="r in runs.slice(0, 5)" :key="r.id" class="task-item">
              <div class="grow">
                <div class="row-between">
                  <strong class="small truncate">{{ r.library_name }}</strong>
                  <span
                    class="badge"
                    :class="
                      r.phase === 'done'
                        ? 'badge-success'
                        : r.phase === 'error'
                          ? 'badge-danger'
                          : r.phase === 'running'
                            ? 'badge-accent'
                            : ''
                    "
                  >
                    {{ phaseLabel(r.phase) }}
                  </span>
                </div>
                <div class="row small muted" style="gap: 6px; margin-top: 3px">
                  <span class="mono">{{ triggerLabel(r.trigger) }}</span>
                  <span>·</span>
                  <span class="mono">+{{ r.written }} / -{{ r.removed }}</span>
                  <template v-if="r.failed">
                    <span>·</span>
                    <span class="warn">失败 {{ r.failed }}</span>
                  </template>
                  <span>·</span>
                  <span :title="fmtTime(r.started_at)">{{ fmtAgo(r.started_at) }}</span>
                </div>
              </div>
            </li>
            <li v-if="!runs.length" class="muted small">还没有同步记录，跑一次增量就有了。</li>
          </ul>
        </div>
      </HPanel>
    </div>

    <!-- 库状态 -->
    <HPanel title="媒体库状态" desc="台账条数 = 应有的 STRM 数；已刮削来自刮削台账" flush>
      <template #actions>
        <RouterLink class="btn btn-sm btn-tertiary" to="/accounts">管理媒体库</RouterLink>
      </template>
      <div v-if="data?.libraries.length" class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>库</th>
              <th>账号</th>
              <th>网盘目录</th>
              <th class="num">片目</th>
              <th class="num">已刮削</th>
              <th class="num">有字幕</th>
              <th>上次同步</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="l in data.libraries" :key="l.id">
              <td>
                <strong>{{ l.name }}</strong>
                <div class="mono muted" style="font-size: 11px">{{ l.local_root }}</div>
              </td>
              <td class="muted">{{ l.account_name || '—' }}</td>
              <td class="mono muted small">{{ l.remote_path || '—' }}</td>
              <td class="num">{{ l.files.toLocaleString() }}</td>
              <td class="num">
                {{ l.scraped.toLocaleString() }}
                <span v-if="l.unmatched" class="warn" style="font-size: 11px"> / 缺 {{ l.unmatched }}</span>
              </td>
              <td class="num">{{ l.subtitled.toLocaleString() }}</td>
              <td class="muted small" :title="fmtTime(l.last_sync_at)">{{ fmtAgo(l.last_sync_at) }}</td>
              <td>
                <span class="badge" :class="libStatus(l.status).cls">{{ libStatus(l.status).text }}</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <HEmpty
        v-else-if="!loading"
        title="还没有媒体库"
        desc="先登录 115 或夸克TV，再选一个网盘目录建库，之后就能同步 STRM 和刮削了"
        icon="local"
      >
        <RouterLink class="btn btn-sm btn-primary" to="/accounts">去建库</RouterLink>
      </HEmpty>
      <div v-else class="pad muted small">正在读取…</div>
    </HPanel>
  </div>
</template>

<style scoped>
.hero {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
  padding: 20px 22px;
  background:
    radial-gradient(90% 140% at 100% 0%, var(--accent-soft), transparent 60%),
    var(--surface);
}
.cols {
  display: grid;
  grid-template-columns: minmax(0, 1.55fr) minmax(300px, 1fr);
  gap: 16px;
  align-items: start;
}
/* 栅格子项默认 min-width:auto：里面一张长表就能把整列撑宽，
   于是下面的卡片看起来「错位」——给个 0 让内容老实待在格子里。 */
.cols > * {
  min-width: 0;
}
.poster-row {
  display: grid;
  grid-auto-flow: column;
  grid-auto-columns: minmax(118px, 1fr);
  gap: 14px;
  overflow-x: auto;
  padding: 14px 16px 16px;
}
.poster-img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.poster-art .eps {
  position: absolute;
  left: 7px;
  bottom: 7px;
  padding: 1px 6px;
  border-radius: var(--r-full);
  background: rgb(10 8 6 / 0.72);
  color: #fff;
  font-size: 11px;
  font-weight: 600;
}
.pad {
  padding: 14px 16px;
}
.job-wrap {
  padding: 14px 16px 16px;
}
.task-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin: 0;
  padding: 0;
  list-style: none;
}
.task-item {
  display: flex;
  gap: 12px;
  padding: 9px 0;
  border-bottom: 1px solid var(--separator);
}
.task-item:last-child {
  border-bottom: 0;
}
.todo {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}
.todo li {
  display: flex;
  align-items: center;
  gap: 10px;
}
.warn {
  color: var(--warning);
}
.sys-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 4px 0;
}
.sys-row {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.sys-track {
  height: 6px;
  border-radius: 999px;
  background: var(--border);
  overflow: hidden;
}
.sys-track > i {
  display: block;
  height: 100%;
  border-radius: 999px;
  background: var(--accent);
  transition: width 0.4s ease;
}
.sys-track > i.warning {
  background: var(--warning);
}
.sys-track > i.danger {
  background: var(--danger);
}
/* 总量未知时的流动条：只表达「还在跑」，不假装进度 */
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
.spin {
  animation: sp 0.9s linear infinite;
}
@keyframes sp {
  to {
    transform: rotate(360deg);
  }
}
@media (max-width: 1080px) {
  .cols {
    grid-template-columns: minmax(0, 1fr);
  }
}
@media (max-width: 720px) {
  .hero {
    flex-direction: column;
    align-items: flex-start;
    gap: 14px;
  }
}
</style>
