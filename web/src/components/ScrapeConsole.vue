<script setup lang="ts">
/**
 * 刮削控制台（「影视库」页里的一个标签，不是独立页面）。
 *
 * 以前是 /scrape 一整页，和 /local 的海报墙各自看同一批片子：
 * 一个页面负责「刮」，另一个页面负责「看」，来回跳很别扭。现在合成一页，
 * 这个组件只保留「干活」的部分：选库、开跑、看进度、按 ID 重刮、调参数。
 * 刮完的结果直接反映在旁边那个海报墙上。
 *
 * 三条硬规矩（和 backend/internal/scrape 的实现一一对应）：
 *
 *  1. **按 IMDb ID 查元数据**：文件名里写了 `tt…` 就直接查，不再搜片名；
 *     认错了或认不出时，可以手动填 ID 重刮单条。
 *  2. **产物落在 .strm 旁边**：movie.nfo / tvshow.nfo / poster.jpg / fanart.jpg
 *     与 `.zh.srt` 都写在同名视频边上，Emby 只读本地，不再联网刮。
 *  3. **避免风控**：只读同步台账、不遍历网盘；元数据与图片落库落盘，
 *     二轮刮削 0 请求；请求之间强制间隔，单条重刮只打 1 次 meta。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HEmpty from '@/components/HEmpty.vue'
import HField from '@/components/HField.vue'
import HModal from '@/components/HModal.vue'
import HPanel from '@/components/HPanel.vue'
import HSwitch from '@/components/HSwitch.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'

// 刮完不用自己找路：这个页面右边就是海报墙，直接切过去看效果。
const emit = defineEmits<{ 'go-wall': [] }>()

/* ---------------- 类型 ---------------- */
type Lib = { id: number; name: string; local_path?: string; collection_type?: string }
type Media = {
  id: number
  library_id: number
  rel_path: string
  kind: string
  title: string
  year: string
  season?: number
  episode?: number
  imdb_id: string
  meta_title: string
  meta_year: string
  rating: string
  runtime: string
  genres: string
  cast: string
  director: string
  overview: string
  poster_file: string
  backdrop_file: string
  nfo_file: string
  sub_file: string
  sub_lang: string
  sub_state: string
  /** zh = 下载的中文字幕；translated = 英文字幕 AI 翻译 */
  sub_origin?: string
  status: string
  message: string
  updated_at: string
}

type ScrapeJob = {
  phase: string
  total: number
  groups: number
  done: number
  matched: number
  unmatched: number
  failed: number
  fetched: number
  cached: number
  current: string
  running: boolean
  message: string
  err: string
}

type SubJob = {
  phase: string
  total: number
  done: number
  saved: number
  translated: number
  missing: number
  failed: number
  current: string
  running: boolean
  message: string
  err: string
}

type ScrapeCfg = {
  write_nfo: boolean
  fetch_images: boolean
  min_size_mb: number
  rate_ms: number
  auto: boolean
  language: string
  translate: boolean
  refresh_server: boolean
}

type SubCfg = {
  auto: boolean
  translate: boolean
  bilingual: boolean
  api_base: string
  model: string
  workers: number
  /** 字幕写盘后通知 Emby 重读；关掉的话覆盖 .srt 也不会换字幕 */
  refresh_server: boolean
}

/* ---------------- 库与列表 ---------------- */
const libs = ref<Lib[]>([])
const libId = ref<number>(0)
const loading = ref(false)
const items = ref<Media[]>([])
const total = ref(0)
const stats = ref<Record<string, number>>({})
const metaCached = ref(0)
const filter = ref<'all' | 'ok' | 'unmatched' | 'error'>('all')
const pageSize = 40
const offset = ref(0)

const counts = computed(() => {
  const s = stats.value || {}
  const ok = s.ok || 0
  const unmatched = s.unmatched || 0
  const error = s.error || 0
  return { ok, unmatched, error, total: ok + unmatched + error }
})

const page = computed(() => Math.floor(offset.value / pageSize) + 1)
const pages = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))

const filters = computed(() => [
  { key: 'all' as const, label: '全部', n: counts.value.total },
  { key: 'ok' as const, label: '已匹配', n: counts.value.ok },
  { key: 'unmatched' as const, label: '未匹配', n: counts.value.unmatched },
  { key: 'error' as const, label: '失败', n: counts.value.error },
])

const libName = computed(() => libs.value.find((l) => l.id === libId.value)?.name ?? '')

async function loadLibs() {
  const list = (await api.libraries.list()) as Lib[]
  libs.value = Array.isArray(list) ? list : []
  if (!libs.value.length) return
  if (!libId.value || !libs.value.some((l) => l.id === libId.value)) {
    libId.value = libs.value[0].id
  }
}

async function loadMedia() {
  if (!libId.value) return
  loading.value = true
  try {
    const d = await api.scrape.media(libId.value, {
      status: filter.value === 'all' ? '' : filter.value,
      limit: pageSize,
      offset: offset.value,
    })
    items.value = (d?.items ?? []) as Media[]
    total.value = d?.total ?? 0
    stats.value = (d?.stats ?? {}) as Record<string, number>
    metaCached.value = d?.meta_cached ?? 0
  } catch (e: any) {
    toast.warning('读取刮削结果失败', e?.message)
  } finally {
    loading.value = false
  }
}

function setFilter(f: typeof filter.value) {
  if (filter.value === f) return
  filter.value = f
  offset.value = 0
  void loadMedia()
}

function turnPage(dir: -1 | 1) {
  const next = offset.value + dir * pageSize
  if (next < 0 || next >= total.value) return
  offset.value = next
  void loadMedia()
}

/* ---------------- 任务进度 ---------------- */
const scrapeJob = ref<ScrapeJob | null>(null)
const subJob = ref<SubJob | null>(null)
// 刮削 / 补字幕以前各跑一个 1500ms setInterval，两个定时器可能同时活着，
// 后台标签页也照打接口。合并成一个 2000ms ticker：一轮里按标记分别拉，
// 都结束就自己停表，既少一个定时器也好统一做 document.hidden 保护。
let pollTimer: number | null = null
let scrapePolling = false
let subPolling = false

const phaseLabel: Record<string, string> = {
  idle: '待机',
  parse: '解析文件名',
  meta: '查询元数据',
  write: '写入文件',
  subs: '补字幕',
  run: '补字幕',
  done: '已完成',
  error: '失败',
  canceled: '已取消',
}

const scrapeRunning = computed(() => !!scrapeJob.value?.running)
const subRunning = computed(() => !!subJob.value?.running)
const busy = computed(() => scrapeRunning.value || subRunning.value)

const scrapePct = computed(() => {
  const j = scrapeJob.value
  if (!j) return 0
  if (j.phase === 'done') return 100
  // 查询阶段按「作品数」算，否则一季 24 集只涨 1 格，看着像卡死。
  const denom = j.phase === 'meta' ? j.groups || 1 : j.total || 1
  const base = j.phase === 'meta' ? j.matched + j.unmatched + j.failed : j.done || 0
  return Math.max(0, Math.min(100, Math.round((base * 100) / denom)))
})

const subPct = computed(() => {
  const j = subJob.value
  if (!j || !j.total) return j?.phase === 'done' ? 100 : 0
  return Math.max(0, Math.min(100, Math.round(((j.done || 0) * 100) / j.total)))
})

function stopPoll() {
  if (pollTimer !== null) {
    window.clearInterval(pollTimer)
    pollTimer = null
  }
  scrapePolling = false
  subPolling = false
}

/** 一轮 tick：两个任务各自按标记拉取，互不影响；都停了就停表。 */
async function tick() {
  if (document.hidden) return // 后台标签页别打接口，回来后下一轮自然续上
  if (!libId.value) return
  if (scrapePolling) {
    try {
      const j = (await api.scrape.job(libId.value)) as ScrapeJob
      scrapeJob.value = j
      if (!j?.running) {
        scrapePolling = false
        if (j?.phase && j.phase !== 'idle') {
          if (j.phase === 'done') toast.success('刮削完成', j.message)
          else if (j.phase === 'error') toast.warning('刮削失败', j.err || j.message)
          void loadMedia()
        }
      }
    } catch {
      scrapePolling = false
    }
  }
  if (subPolling) {
    try {
      const j = (await api.scrape.subtitleJob(libId.value)) as SubJob
      subJob.value = j
      if (!j?.running) {
        subPolling = false
        if (j?.phase && j.phase !== 'idle') {
          toast.success('补字幕结束', j.message)
          void loadMedia()
        }
      }
    } catch {
      subPolling = false
    }
  }
  if (!scrapePolling && !subPolling) stopPoll()
}

/** 合并后只留一个 ticker：已经在跑就别再建，避免重复定时器。 */
function ensurePoll() {
  if (pollTimer !== null) return
  void tick()
  pollTimer = window.setInterval(tick, 2000)
}

function pollScrape() {
  scrapePolling = true
  ensurePoll()
}

function pollSub() {
  subPolling = true
  ensurePoll()
}

async function loadJobs() {
  if (!libId.value) return
  try {
    const j = (await api.scrape.job(libId.value)) as ScrapeJob
    scrapeJob.value = j
    if (j?.running) pollScrape()
  } catch {
    /* 没有任务不影响页面 */
  }
  try {
    const s = (await api.scrape.subtitleJob(libId.value)) as SubJob
    subJob.value = s
    if (s?.running) pollSub()
  } catch {
    /* 同上 */
  }
}

/* ---------------- 动作 ---------------- */
async function startScrape(overwrite: boolean) {
  if (!libId.value) {
    toast.warning('先建一个媒体库并同步一次')
    return
  }
  try {
    await api.scrape.start(libId.value, overwrite ? { overwrite: true, refresh_cache: true } : {})
    toast.info(overwrite ? '已开始重新刮削（覆盖已有文件）' : '已开始刮削')
    pollScrape()
  } catch (e: any) {
    toast.warning('启动失败', e?.message)
  }
}

async function cancelScrape() {
  try {
    await api.scrape.cancel(libId.value)
    toast.info('已请求停止')
  } catch (e: any) {
    toast.warning('取消失败', e?.message)
  }
}

async function startSubtitles(redo: boolean) {
  if (!libId.value) return
  try {
    const r = await api.scrape.subtitles(libId.value, redo)
    toast.info(
      redo ? `已开始重做全部字幕，共 ${r?.total ?? 0} 条` : `已开始补字幕，共 ${r?.total ?? 0} 条`,
      redo ? '会覆盖已有字幕，跑完统一通知 Emby 重读' : undefined,
    )
    pollSub()
  } catch (e: any) {
    toast.warning('启动失败', e?.message)
  }
}

async function fetchSub(m: Media, overwrite = false) {
  if (!m.imdb_id) {
    toast.warning('这条没有 IMDb ID', '先点「指定 IMDb ID」补一个')
    return
  }
  subBusy.value = m.id
  try {
    const r: any = await api.scrape.subtitle(m.id, overwrite)
    if (r?.skipped) toast.info('已经有中文字幕', '要换一份就点「重下」')
    else toast.success(r?.overwritten ? '字幕已重新下载并覆盖' : '字幕已写入',
      r?.server_refresh ? `${r.saved} · 已通知 Emby 重读` : r?.saved)
    await loadMedia()
  } catch (e: any) {
    toast.warning('补字幕失败', e?.message)
  } finally {
    subBusy.value = 0
  }
}
const subBusy = ref(0)

/* ---------------- 按 ID 重刮 ---------------- */
const modal = ref(false)
const editing = ref<Media | null>(null)
const saving = ref(false)
const form = ref({ imdbId: '', overwrite: true, subtitles: true })

function openRescrape(m: Media) {
  editing.value = m
  form.value = { imdbId: m.imdb_id || '', overwrite: true, subtitles: true }
  modal.value = true
}

async function submitRescrape() {
  const m = editing.value
  if (!m) return
  const id = form.value.imdbId.trim()
  if (!id) {
    toast.warning('请填 IMDb ID', '形如 tt15239678')
    return
  }
  saving.value = true
  try {
    const r = await api.scrape.rescrape(m.id, {
      imdb_id: id,
      overwrite: form.value.overwrite,
      subtitles: form.value.subtitles,
    })
    const sub = r?.subtitle
    if (sub && sub.ok === false) {
      toast.warning('元数据已更新，字幕失败', sub.error)
    } else if (sub?.saved) {
      toast.success('已重刮并写入字幕', sub.saved)
    } else {
      toast.success('已重刮', r?.media?.meta_title || m.rel_path)
    }
    modal.value = false
    await loadMedia()
  } catch (e: any) {
    toast.warning('重刮失败', e?.message)
  } finally {
    saving.value = false
  }
}

/* ---------------- 配置 ---------------- */
const cfg = ref<ScrapeCfg>({
  write_nfo: true,
  fetch_images: true,
  min_size_mb: 50,
  rate_ms: 1200,
  auto: true,
  language: 'zh',
  translate: true,
  refresh_server: true,
})
const sub = ref<SubCfg>({
  auto: true,
  translate: true,
  bilingual: false,
  api_base: '',
  model: '',
  workers: 2,
  refresh_server: true,
})
const savingCfg = ref(false)

async function loadConfig() {
  try {
    const d = await api.scrape.config()
    if (d?.scrape) cfg.value = { ...cfg.value, ...d.scrape }
    if (d?.subtitle) sub.value = { ...sub.value, ...d.subtitle }
  } catch (e: any) {
    toast.warning('读取配置失败', e?.message)
  }
}

async function saveScrapeCfg() {
  savingCfg.value = true
  try {
    await api.scrape.saveConfig({ scrape_config: JSON.stringify(cfg.value) })
    toast.success('刮削配置已保存')
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    savingCfg.value = false
  }
}

async function saveSubCfg() {
  savingCfg.value = true
  try {
    await api.scrape.saveConfig({ subtitle_config: JSON.stringify(sub.value) })
    toast.success('字幕配置已保存')
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    savingCfg.value = false
  }
}

/* ---------------- 展示辅助 ---------------- */
function imdbURL(id: string) {
  return id ? `https://www.imdb.com/title/${id}/` : ''
}

function displayTitle(m: Media) {
  return m.meta_title || m.title || '(未识别)'
}

function displayYear(m: Media) {
  return m.meta_year || m.year || ''
}

function seasonEpisode(m: Media) {
  if (m.kind !== 'series' || (!m.season && !m.episode)) return ''
  const pad = (n?: number) => String(n || 0).padStart(2, '0')
  return `S${pad(m.season)}E${pad(m.episode)}`
}

function statusBadge(m: Media) {
  if (m.status === 'ok') return { cls: 'badge-success', text: '已匹配' }
  if (m.status === 'error') return { cls: 'badge-danger', text: '失败' }
  return { cls: 'badge-warning', text: '未匹配' }
}

function subBadge(m: Media) {
  // 「有中字」要区分来源：机翻和下载来的官方中文在播放器里长得一样，
  // 只有这里能看出来 —— 奥德赛那次就是满屏机翻才发现不对。
  if (m.sub_file) {
    if (m.sub_origin === 'translated') return { cls: 'badge-warning', text: '机翻中字' }
    return { cls: 'badge-success', text: '有中字' }
  }
  if (m.sub_state === 'missing') return { cls: 'badge-warning', text: '无中字' }
  if (m.sub_state === 'mismatch') return { cls: 'badge-warning', text: '字幕不符' }
  if (m.sub_state === 'failed') return { cls: 'badge-danger', text: '字幕失败' }
  return { cls: '', text: '未补字幕' }
}

function onLibChange() {
  offset.value = 0
  stopPoll()
  scrapeJob.value = null
  subJob.value = null
  void loadMedia()
  void loadJobs()
}

/* ---------------- 生命周期 ---------------- */
onMounted(async () => {
  await Promise.all([loadLibs(), loadConfig()])
  if (libId.value) {
    await Promise.all([loadMedia(), loadJobs()])
  }
})

onBeforeUnmount(stopPoll)
</script>

<template>
  <div class="col" style="gap: 16px">
    <!-- 顶部：库选择 + 动作 + 风控说明 -->
    <HPanel title="刮削控制台" desc="选库开跑，产物落在 .strm 旁边，Emby 只读本地">
      <template #actions>
        <select v-model.number="libId" class="select-native" style="min-width: 190px" @change="onLibChange">
          <option v-if="!libs.length" :value="0">还没有媒体库</option>
          <option v-for="l in libs" :key="l.id" :value="l.id">{{ l.name }}</option>
        </select>
      </template>

      <div class="row wrap" style="gap: 8px">
        <button class="btn btn-primary" :disabled="!libId || busy" @click="startScrape(false)">
          <AppIcon name="play" :size="14" />{{ scrapeRunning ? '刮削中…' : '开始刮削' }}
        </button>
        <button class="btn btn-tertiary" :disabled="!libId || busy" @click="startScrape(true)">
          <AppIcon name="refresh" :size="14" />重新刮削（覆盖）
        </button>
        <button v-if="scrapeRunning" class="btn btn-ghost" @click="cancelScrape">
          <AppIcon name="close" :size="14" />停止
        </button>
        <span class="grow" />
        <button class="btn btn-tertiary" :disabled="!libId || busy" @click="startSubtitles(false)">
          <AppIcon name="file" :size="14" />补中文字幕
        </button>
        <button class="btn btn-ghost" :disabled="!libId || busy" @click="startSubtitles(true)">
          重做全部字幕
        </button>
        <span class="grow" />
        <button class="btn btn-ghost" title="切到海报墙看刮削效果" @click="emit('go-wall')">
          <AppIcon name="local" :size="14" />看海报墙
        </button>
      </div>

      <div class="grid-4 safe-grid">
        <div class="tipbox">
          <strong class="small">① 只读台账</strong>
          <p class="muted small">刮削读同步下来的文件名，不遍历网盘，不碰 115 接口。</p>
        </div>
        <div class="tipbox">
          <strong class="small">② 按 IMDb ID 直查</strong>
          <p class="muted small">名字里有 <code class="mono">tt…</code> 就跳过搜索，单次 meta 拿全量。</p>
        </div>
        <div class="tipbox">
          <strong class="small">③ 本地缓存</strong>
          <p class="muted small">元数据、图片、字幕都落盘落库，二次刮削 0 请求。</p>
        </div>
        <div class="tipbox">
          <strong class="small">④ 落在 STRM 旁</strong>
          <p class="muted small">nfo / poster.jpg / fanart.jpg / .zh.srt 与视频同名。</p>
        </div>
      </div>

      <p v-if="!libs.length" class="muted small" style="margin-top: 12px">
        还没有媒体库。先去「账号与媒体库」建一个库并同步一次，刮削才有清单可读。
      </p>
    </HPanel>

    <!-- 统计 -->
    <div class="grid-4">
      <div class="stat">
        <span class="stat-label"><AppIcon name="scrape" :size="14" />片目</span>
        <span class="stat-value">{{ counts.total.toLocaleString() }}</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="check" :size="14" />已匹配</span>
        <span class="stat-value" style="color: var(--success)">{{ counts.ok.toLocaleString() }}</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="alert" :size="14" />未匹配</span>
        <span class="stat-value">{{ counts.unmatched.toLocaleString() }}</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="alert" :size="14" />失败</span>
        <span class="stat-value" :style="counts.error ? 'color: var(--danger)' : ''">
          {{ counts.error.toLocaleString() }}
        </span>
      </div>
    </div>

    <!-- 进度 -->
    <div v-if="scrapeJob && scrapeJob.phase && scrapeJob.phase !== 'idle'" class="grid-2">
      <HPanel :title="`刮削进度 · ${phaseLabel[scrapeJob.phase] || scrapeJob.phase}`">
        <template #actions>
          <span class="badge" :class="scrapeRunning ? 'badge-warning' : scrapeJob.phase === 'error' ? 'badge-danger' : 'badge-success'">
            {{ scrapeRunning ? '进行中' : phaseLabel[scrapeJob.phase] || scrapeJob.phase }}
          </span>
        </template>
        <div class="col" style="gap: 10px">
          <div class="row-between small">
            <span class="truncate">{{ scrapeJob.message }}</span>
            <span class="mono">{{ scrapePct }}%</span>
          </div>
          <div class="bar"><i :style="{ width: scrapePct + '%' }" /></div>
          <p v-if="scrapeJob.current" class="muted small mono truncate">{{ scrapeJob.current }}</p>
          <div class="row wrap small muted" style="gap: 10px">
            <span>视频 {{ scrapeJob.total }}</span>
            <span>作品 {{ scrapeJob.groups }}</span>
            <span>已写 {{ scrapeJob.done }}</span>
            <span>匹配 {{ scrapeJob.matched }}</span>
            <span>未匹配 {{ scrapeJob.unmatched }}</span>
            <span>失败 {{ scrapeJob.failed }}</span>
            <span class="badge badge-info">IMDb 请求 {{ scrapeJob.fetched }}</span>
            <span class="badge">命中缓存 {{ scrapeJob.cached }}</span>
          </div>
          <p v-if="scrapeJob.err" class="small" style="color: var(--danger)">{{ scrapeJob.err }}</p>
        </div>
      </HPanel>

      <HPanel v-if="subJob && subJob.phase && subJob.phase !== 'idle'" title="字幕进度">
        <template #actions>
          <span class="badge" :class="subRunning ? 'badge-warning' : 'badge-success'">
            {{ subRunning ? '进行中' : phaseLabel[subJob.phase] || subJob.phase }}
          </span>
        </template>
        <div class="col" style="gap: 10px">
          <div class="row-between small">
            <span class="truncate">{{ subJob.message }}</span>
            <span class="mono">{{ subPct }}%</span>
          </div>
          <div class="bar"><i :style="{ width: subPct + '%' }" /></div>
          <p v-if="subJob.current" class="muted small mono truncate">{{ subJob.current }}</p>
          <div class="row wrap small muted" style="gap: 10px">
            <span>共 {{ subJob.total }}</span>
            <span>完成 {{ subJob.done }}</span>
            <span>写入 {{ subJob.saved }}</span>
            <span>翻译 {{ subJob.translated }}</span>
            <span>无字幕源 {{ subJob.missing }}</span>
            <span>失败 {{ subJob.failed }}</span>
          </div>
        </div>
      </HPanel>
    </div>

    <!-- 结果 -->
    <HPanel
      title="刮削结果"
      :desc="`共 ${counts.total} 条 · 本地 IMDb 缓存 ${metaCached} 部 · 数据落在各自 .strm 同目录`"
      flush
    >
      <template #actions>
        <button class="btn btn-sm btn-ghost" :disabled="loading" @click="loadMedia">
          <AppIcon name="refresh" :size="14" />刷新
        </button>
      </template>

      <div class="table-toolbar">
        <div class="segmented">
          <button
            v-for="f in filters"
            :key="f.key"
            :class="{ 'is-active': filter === f.key }"
            @click="setFilter(f.key)"
          >
            {{ f.label }}<span v-if="f.n" class="mono"> {{ f.n }}</span>
          </button>
        </div>
        <span class="grow" />
        <span class="muted small">{{ loading ? '读取中…' : `${page} / ${pages} 页` }}</span>
      </div>

      <div v-if="items.length" class="table-wrap">
        <table class="table media-table">
          <thead>
            <tr>
              <th style="width: 58px"></th>
              <th>片目</th>
              <th style="width: 112px">IMDb</th>
              <th style="width: 150px">信息</th>
              <th style="width: 92px">元数据</th>
              <th style="width: 92px">字幕</th>
              <th style="width: 158px">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in items" :key="m.id">
              <td>
                <div class="poster">
                  <img
                    v-if="m.poster_file"
                    :src="`/api/media/${m.id}/image`"
                    alt=""
                    loading="lazy"
                    decoding="async"
                  />
                  <span v-else class="muted">无图</span>
                </div>
              </td>
              <td>
                <div class="row" style="gap: 7px; flex-wrap: wrap">
                  <strong class="small">{{ displayTitle(m) }}</strong>
                  <span v-if="displayYear(m)" class="muted small">{{ displayYear(m) }}</span>
                  <span v-if="seasonEpisode(m)" class="badge mono">{{ seasonEpisode(m) }}</span>
                </div>
                <p class="mono small muted path truncate" :title="m.rel_path">{{ m.rel_path }}</p>
                <p v-if="m.message" class="small" style="color: var(--warning); margin-top: 2px">
                  {{ m.message }}
                </p>
              </td>
              <td>
                <a
                  v-if="m.imdb_id"
                  class="mono small imdb"
                  :href="imdbURL(m.imdb_id)"
                  target="_blank"
                  rel="noreferrer"
                  :title="m.imdb_id"
                >
                  {{ m.imdb_id }}<AppIcon name="external" :size="11" />
                </a>
                <span v-else class="muted small">—</span>
              </td>
              <td class="small muted">
                <div v-if="m.rating" class="mono">★ {{ m.rating }}</div>
                <div v-if="m.runtime">{{ m.runtime }}</div>
                <div v-if="m.genres" class="truncate" :title="m.genres">{{ m.genres }}</div>
              </td>
              <td>
                <span class="badge" :class="statusBadge(m).cls">{{ statusBadge(m).text }}</span>
              </td>
              <td>
                <span class="badge" :class="subBadge(m).cls">{{ subBadge(m).text }}</span>
              </td>
              <td>
                <div class="row" style="gap: 4px; flex-wrap: wrap">
                  <button class="btn btn-sm btn-tertiary" @click="openRescrape(m)">
                    <AppIcon name="edit" :size="13" />{{ m.imdb_id ? '重刮' : '指定 ID' }}
                  </button>
                  <button
                    class="btn btn-sm btn-ghost"
                    :disabled="!m.imdb_id || subBusy === m.id"
                    :title="m.imdb_id ? '补中文字幕' : '先指定 IMDb ID'"
                    @click="fetchSub(m)"
                  >
                    <AppIcon name="file" :size="13" />{{ subBusy === m.id ? '…' : '字幕' }}
                  </button>
                  <button
                    class="btn btn-sm btn-ghost"
                    :disabled="!m.imdb_id || subBusy === m.id"
                    title="重新下载并覆盖已有字幕；会通知 Emby 重读"
                    @click="fetchSub(m, true)"
                  >
                    <AppIcon name="refresh" :size="13" />重下
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <HEmpty
        v-else
        :title="libId ? '还没有刮削记录' : '还没有媒体库'"
        :desc="
          libId
            ? '点上面的「开始刮削」，会读同步下来的文件名（含 tt… 的直接按 ID 查）。'
            : '先去「账号与媒体库」建库并同步一次。'
        "
        icon="scrape"
      />

      <template v-if="total > pageSize" #footer>
        <button class="btn btn-sm btn-tertiary" :disabled="offset === 0" @click="turnPage(-1)">上一页</button>
        <span class="muted small">第 {{ page }} / {{ pages }} 页 · 共 {{ total }} 条</span>
        <button class="btn btn-sm btn-tertiary" :disabled="page >= pages" @click="turnPage(1)">下一页</button>
      </template>
    </HPanel>

    <!-- 配置 -->
    <div class="grid-2">
      <HPanel title="刮削配置" desc="写什么、走多快；改完立即生效">
        <HField label="写入 NFO" hint="Emby 优先读本地 nfo，读到就不会再联网刮">
          <label class="checkline"><HSwitch v-model="cfg.write_nfo" />生成 movie.nfo / tvshow.nfo / episodedetails</label>
        </HField>
        <HField label="图片" hint="海报与背景图下载到视频同目录，刷新多少次都不回源">
          <label class="checkline"><HSwitch v-model="cfg.fetch_images" />下载 poster.jpg / fanart.jpg</label>
        </HField>
        <HField label="同步后自动刮削" hint="关掉就得每次手动点「开始刮削」">
          <label class="checkline"><HSwitch v-model="cfg.auto" />同步完成即刮削并补字幕</label>
        </HField>
        <HField label="中文优先" hint="标题取文件名里的中文名，英文简介走翻译">
          <div class="col" style="gap: 8px">
            <label class="checkline"><HSwitch v-model="cfg.translate" />英文简介/标题自动翻成中文</label>
            <select v-model="cfg.language" class="select-native" style="max-width: 200px">
              <option value="zh">简体中文优先</option>
              <option value="original">保留 IMDb 原文</option>
            </select>
          </div>
        </HField>
        <HField label="最小体积" tip="小于这个体积的视频（预告片 / 样片）不刮">
          <input v-model.number="cfg.min_size_mb" class="input" type="number" min="0" style="max-width: 130px" />
          <span class="muted small" style="margin-left: 8px">MB</span>
        </HField>
        <HField label="请求间隔" tip="两次 IMDb 请求之间的最小间隔；调小会让风控更早找上门">
          <input v-model.number="cfg.rate_ms" class="input" type="number" min="200" step="100" style="max-width: 130px" />
          <span class="muted small" style="margin-left: 8px">毫秒（建议 ≥ 1000）</span>
        </HField>
        <HField label="让 Emby 重读" hint="刮完触发一次目录刷新，否则界面上还是旧信息">
          <label class="checkline"><HSwitch v-model="cfg.refresh_server" />刮削完成后刷新媒体库</label>
        </HField>
        <template #footer>
          <button class="btn btn-primary" :disabled="savingCfg" @click="saveScrapeCfg">
            {{ savingCfg ? '保存中…' : '保存刮削配置' }}
          </button>
        </template>
      </HPanel>

      <HPanel title="字幕配置" desc="按 IMDb ID 查字幕；有中文直接用，没有就翻译">
        <HField label="自动补字幕" hint="整库刮削结束后自动跑一轮，只补还没有中字的条目">
          <label class="checkline"><HSwitch v-model="sub.auto" />刮完自动补中文字幕</label>
        </HField>
        <HField label="没有中文时" hint="字幕源只有英文时，用翻译接口转成中文再落盘">
          <label class="checkline"><HSwitch v-model="sub.translate" />把英文字幕翻译成中文</label>
        </HField>
        <HField label="双语" hint="译文在上、原文在下，适合对照检查翻译质量">
          <label class="checkline"><HSwitch v-model="sub.bilingual" />输出中英双语字幕</label>
        </HField>
        <HField
          label="写完通知 Emby"
          hint="字幕落盘后让媒体服务器重读；关掉的话覆盖 .srt 也不会换掉播放器里的旧字幕"
        >
          <label class="checkline"><HSwitch v-model="sub.refresh_server" />通知 Emby 重读字幕</label>
        </HField>
        <HField label="翻译接口" tip="OpenAI 兼容的 /v1 地址；留空用内置的公共接口">
          <input v-model="sub.api_base" class="input mono" placeholder="https://…/v1" />
        </HField>
        <HField label="模型">
          <input v-model="sub.model" class="input mono" placeholder="Index-Translate-35B-A3B" />
        </HField>
        <HField label="并发" tip="公共接口并发别开太大，2 比较稳">
          <input v-model.number="sub.workers" class="input" type="number" min="1" max="8" style="max-width: 110px" />
        </HField>
        <div class="tipbox" style="margin-top: 4px">
          <strong class="small">落盘位置 / Emby 刷新</strong>
          <p class="muted small">
            字幕写成「视频同名 + <code class="mono">.zh.srt</code>」，和 .strm 放在一起。
            Emby 只认与视频同名的字幕，<code class="mono">.strm.zh.srt</code> 会被当成孤儿文件。
          </p>
          <p class="muted small" style="margin-top: 6px">
            Emby 只在首次扫描时登记外挂字幕，光覆盖 .srt 它不会自己重探。
            「重做全部字幕」和单条「重下」现在会真的覆盖文件、把旧的
            <code class="mono">.chs/.cht</code> 字幕改名留底、刷新 .strm 时间戳，
            并通知 Emby 重读（上面那个开关）。普通「补中文字幕」仍然跳过已有的字幕。
          </p>
        </div>
        <template #footer>
          <button class="btn btn-primary" :disabled="savingCfg" @click="saveSubCfg">
            {{ savingCfg ? '保存中…' : '保存字幕配置' }}
          </button>
        </template>
      </HPanel>
    </div>

    <!-- 单条按 ID 重刮 -->
    <HModal v-model="modal" title="按 IMDb ID 重刮" size="md">
      <template #desc>
        后端只发 1 次 meta 请求（命中缓存 0 次），就地改写 .strm 旁边的 nfo / 海报 / 字幕。
      </template>
      <div class="col" style="gap: 14px">
        <div v-if="editing" class="tipbox">
          <strong class="small">{{ displayTitle(editing) }} {{ displayYear(editing) }}</strong>
          <p class="mono small muted" style="margin-top: 4px">{{ editing.rel_path }}</p>
        </div>
        <HField
          label="IMDb ID"
          required
          tip="可以粘整条链接，后端自己会取出 tt 号"
          hint="形如 tt15239678，或 https://www.imdb.com/title/tt15239678/"
        >
          <input
            v-model="form.imdbId"
            class="input mono"
            placeholder="tt15239678"
            @keyup.enter="submitRescrape"
          />
        </HField>
        <div class="col" style="gap: 8px">
          <label class="checkline"><HSwitch v-model="form.overwrite" />覆盖已有的 nfo / 海报（推荐）</label>
          <label class="checkline"><HSwitch v-model="form.subtitles" />同时补一条中文字幕</label>
        </div>
      </div>
      <template #footer>
        <button class="btn btn-ghost" @click="modal = false">取消</button>
        <button class="btn btn-primary" :disabled="saving || !form.imdbId.trim()" @click="submitRescrape">
          {{ saving ? '刮削中…' : '开始刮削' }}
        </button>
      </template>
    </HModal>
  </div>
</template>

<style scoped>
.checkline {
  display: flex;
  align-items: center;
  gap: 9px;
  font-size: 13px;
  line-height: 1.5;
}
.tipbox {
  padding: 12px 13px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.tipbox p {
  margin-top: 4px;
  line-height: 1.6;
}
.safe-grid {
  margin-top: 14px;
}
.table-toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--border);
  flex-wrap: wrap;
}
.media-table td {
  vertical-align: top;
}
.poster {
  width: 42px;
  height: 60px;
  border-radius: 6px;
  overflow: hidden;
  background: var(--surface-secondary);
  border: 1px solid var(--border);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 10px;
}
.poster img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}
.path {
  margin-top: 2px;
  max-width: 380px;
}
.imdb {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  color: var(--accent);
}
.imdb:hover {
  text-decoration: underline;
}
</style>
