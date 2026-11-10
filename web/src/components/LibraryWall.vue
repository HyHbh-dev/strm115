<script setup lang="ts">
/**
 * 我的海报墙。
 *
 * 和 WallBrowser（TMDB 榜单，用来「发现想看什么」）不同，这里只管
 * 「网盘里已经有、并且进了媒体库」的影视：
 *
 *  - 查：默认按本地台账的 TMDB ID 现查 TMDB 元数据（不依赖媒体服务器）；
 *    可切到 Emby（读媒体服务器里最终生效的 nfo）或纯本地台账兜底。
 *  - 增：从库挂的任一网盘目录（115 / 夸克）选片 → 生成 .strm → 登记台账 → 自动刮削 + 刷新 Emby。
 *  - 改：重写本地 nfo + 台账 + 元数据缓存；改 TMDB / IMDb ID 才按新 ID 重刮一次。
 *  - 删：默认只移除索引（.strm/元数据/台账/Emby 条目），可选连 115 源文件删。
 *
 * 所有写操作都围绕同一条链：网盘原文件 → 本地 .strm(+nfo/海报/字幕) → Emby 条目。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import TitleLink from '@/components/TitleLink.vue'
import AppIcon from './AppIcon.vue'
import DirPicker from './DirPicker.vue'
import HEmpty from './HEmpty.vue'
import HField from './HField.vue'
import HModal from './HModal.vue'
import HPanel from './HPanel.vue'
import HSwitch from './HSwitch.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'

/* ---------------- 类型 ---------------- */
/** 库的一个网盘来源（115 / 夸克）。v0.2.7 起一个库可以挂多个。 */
type LibSource = {
  account_id: number
  account_kind?: string
  kind?: string
  account_name?: string
  remote_cid?: string
  remote_path?: string
}

type Card = {
  key: string
  id?: string
  name: string
  original?: string
  type: 'movie' | 'series'
  year?: number
  rating?: number
  overview?: string
  genres?: string[]
  poster?: string
  backdrop?: string
  imdb_id?: string
  tmdb_id?: string
  library_id?: number
  library?: string
  rel_path?: string
  path?: string
  media_id?: number
  nfo?: boolean
  sub_state?: string
  /** zh = 下载的中文字幕；translated = 英文字幕 AI 翻译 */
  sub_origin?: string
  episodes?: number
  size?: number
  added?: string
  in_emby?: boolean
  in_local?: boolean
  people?: string[]
  cast?: CastMember[]
}

/**
 * 一位演员。image 是服务端代理过来的头像地址，空表示这个人没有图 ——
 * 这时显示姓名首字，而不是一个 404 的 <img>（破图比没图更难看）。
 */
type CastMember = {
  name: string
  role?: string
  image?: string
}

type Episode = {
  name: string
  season?: number
  episode?: number
  path?: string
  rel_path?: string
  sub_state?: string
  /** zh = 下载的中文字幕；translated = 英文字幕 AI 翻译 */
  sub_origin?: string
  nfo?: boolean
  media_id?: number
}

type LocalView = {
  exists?: boolean
  id?: number
  title?: string
  year?: string
  overview?: string
  genres?: string[]
  rating?: string
  imdb_id?: string
  tmdb_id?: string
  sub_state?: string
  /** zh = 下载的中文字幕；translated = 英文字幕 AI 翻译 */
  sub_origin?: string
  nfo_file?: string
  sub_file?: string
  rel_path?: string
  status?: string
}

type Detail = { item: Card; episodes: Episode[]; local: LocalView }
type Lib = { id: number; name: string; server_lib_id?: string }

const PAGE_SIZE = 30

// 墙上看不到想要的片，直接跳到旁边那个标签去刮，不用先退出页面再点侧栏。
const emit = defineEmits<{ 'go-scrape': [] }>()

/* ---------------- 列表状态 ---------------- */
const loading = ref(false)
const err = ref('')
const hint = ref('')
const items = ref<Card[]>([])
const total = ref(0)
const page = ref(1)
const hasMore = ref(false)
const libs = ref<Lib[]>([])
const genres = ref<string[]>([])
const libId = ref(0)
// 默认读 Emby：墙面要回答「媒体服务器里现在有什么」，而 Emby 读的是我们写在
// .strm 旁边的 nfo，是最终生效的那份。没接媒体服务器时后端会自动退回 TMDB。
const source = ref<'tmdb' | 'emby' | 'local'>('emby')
const activeSource = ref('emby')
const typ = ref<'all' | 'movie' | 'series'>('all')
const genre = ref('')
const search = ref('')
const sortKey = ref('recent')
const embyTotal = ref(0)
const localTotal = ref(0)
const embyOK = ref(false)
const embyChecked = ref(false)
const tmdbMissing = ref(0)
const broken = ref<Record<string, boolean>>({})

const sourceLabel = computed(() => {
  switch (activeSource.value) {
    case 'tmdb':
      return 'TMDB'
    case 'emby':
      return 'Emby'
    default:
      return '本地台账'
  }
})

const typeChips: { v: 'all' | 'movie' | 'series'; label: string }[] = [
  { v: 'all', label: '全部' },
  { v: 'movie', label: '电影' },
  { v: 'series', label: '剧集' },
]

let searchTimer: ReturnType<typeof setTimeout> | null = null

/** 让 Emby 重扫绑定的库，再拉一遍墙面：加了片 / 改了 nfo 之后用这个。 */
const syncing = ref(false)
async function syncEmby() {
  syncing.value = true
  try {
    await api.config.scanServerLibrary()
    toast.success('已让 Emby 重扫媒体库', '扫描完成后墙面会自动显示新条目')
    // Emby 的扫描是异步的，给几秒钟再取一次，免得用户以为没反应
    await new Promise((r) => setTimeout(r, 2500))
    await load()
  } catch (e: any) {
    toast.warning('同步失败', e?.message || '没连上媒体服务器')
  } finally {
    syncing.value = false
  }
}

async function load() {
  loading.value = true
  err.value = ''
  try {
    const r: any = await api.catalog.list({
      library_id: libId.value || undefined,
      source: source.value,
      type: typ.value,
      genre: genre.value,
      q: search.value.trim(),
      sort: sortKey.value,
      page: page.value,
      limit: PAGE_SIZE,
    })
    items.value = (r?.items ?? []) as Card[]
    total.value = Number(r?.total ?? 0)
    hasMore.value = !!r?.has_more
    libs.value = (r?.libraries ?? []) as Lib[]
    genres.value = (r?.genres ?? []) as string[]
    activeSource.value = String(r?.source ?? source.value)
    embyTotal.value = Number(r?.emby_total ?? 0)
    localTotal.value = Number(r?.local_total ?? 0)
    embyOK.value = !!r?.emby_ok
    embyChecked.value = !!r?.emby_checked
    tmdbMissing.value = Number(r?.tmdb_missing ?? 0)
    hint.value = String(r?.hint ?? '')
    broken.value = {}
  } catch (e: any) {
    err.value = e?.message || '读取媒体目录失败'
    items.value = []
  } finally {
    loading.value = false
  }
}

function resetAndLoad() {
  page.value = 1
  void load()
}

function prevPage() {
  if (page.value > 1 && !loading.value) {
    page.value--
    void load()
  }
}
function nextPage() {
  if (hasMore.value && !loading.value) {
    page.value++
    void load()
  }
}

watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(resetAndLoad, 320)
})
watch([libId, source, typ, genre, sortKey], resetAndLoad)
onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
})

/* ---------------- 卡片 ---------------- */
function phStyle(c: Card) {
  let h = 0
  for (const ch of c.key + c.name) h = (h * 31 + ch.charCodeAt(0)) % 360
  return {
    background: `linear-gradient(150deg, hsl(${h} 34% 30%), hsl(${(h + 48) % 360} 38% 18%))`,
  }
}
function markBroken(key: string) {
  broken.value = { ...broken.value, [key]: true }
}
function subLabel(s?: string) {
  switch (s) {
    case 'ok':
    case 'saved':
      return '中文字幕已就绪'
    case 'missing':
      return '未找到中文字幕'
    case 'mismatch':
      return '字幕与片源不符（同名另一部片）'
    case 'failed':
    case 'error':
      return '字幕下载失败'
    case 'running':
      return '字幕处理中'
    default:
      return s ? s : '尚未补字幕'
  }
}
function subTone(s?: string) {
  if (s === 'ok' || s === 'saved') return 'badge-success'
  if (s === 'failed' || s === 'error' || s === 'missing' || s === 'mismatch') return 'badge-warning'
  return ''
}
/**
 * 头像占位用的首字。中文取第一个字（「梁」），西文取首字母（「TC」）。
 *
 * 不取整个姓名：演员格只有 72px 宽，两个字以上必然被截断，
 * 反而看不出是谁。
 */
function initialOf(name: string): string {
  const v = (name || '').trim()
  if (!v) return '?'
  if (/[\u4e00-\u9fa5]/.test(v[0])) return v[0]
  const parts = v.split(/\s+/).filter(Boolean)
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase()
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
}

function shortDate(s?: string) {
  return s ? String(s).slice(0, 10) : ''
}

/* ---------------- 详情抽屉 ---------------- */
const drawer = ref(false)
const detail = ref<Detail | null>(null)
const detailLoading = ref(false)
const showAllEp = ref(false)

async function openDetail(c: Card) {
  drawer.value = true
  detailLoading.value = true
  detail.value = null
  showAllEp.value = false
  try {
    detail.value = (await api.catalog.item(c.key, source.value)) as Detail
  } catch (e: any) {
    toast.warning('读取详情失败', e?.message)
    drawer.value = false
  } finally {
    detailLoading.value = false
  }
}

const visibleEpisodes = computed(() => {
  const all = detail.value?.episodes ?? []
  return showAllEp.value ? all : all.slice(0, 12)
})

const imdbLink = computed(() => {
  const id = detail.value?.item?.imdb_id
  return id ? `https://www.imdb.com/title/${id}/` : ''
})

const tmdbLink = computed(() => {
  const id = detail.value?.item?.tmdb_id
  const kind = detail.value?.item?.type === 'series' ? 'tv' : 'movie'
  return id ? `https://www.themoviedb.org/${kind}/${id}` : ''
})

/* ---------------- 补字幕 ---------------- */
const subBusy = ref(false)
async function fetchSubtitle(overwrite = false) {
  const id = detail.value?.local?.id ?? detail.value?.item?.media_id
  if (!id) {
    toast.warning('这条还没有本地刮削记录', '先到「影视刮削」跑一次，再补字幕')
    return
  }
  const key = detail.value?.item?.key
  if (!key) return
  subBusy.value = true
  try {
    const r: any = await api.scrape.subtitle(id, overwrite)
    if (r?.skipped) toast.info('已经有中文字幕', '要换一份就点「重新下载」')
    else toast.success(
      r?.overwritten ? '字幕已重新下载并覆盖' : '字幕已写入本地',
      r?.server_refresh ? `${r.saved} · 已通知 Emby 重读` : r?.saved,
    )
    detail.value = (await api.catalog.item(key, source.value)) as Detail
    void load()
  } catch (e: any) {
    toast.warning('补字幕失败', e?.message)
  } finally {
    subBusy.value = false
  }
}

/* ---------------- 改：编辑元数据 ---------------- */
const editOpen = ref(false)
const editSaving = ref(false)
const editOriginalID = ref('')
const editOriginalTMDbID = ref(0)
const editForm = ref({
  key: '',
  title: '',
  original: '',
  year: '',
  rating: '',
  tmdb_id: '',
  imdb_id: '',
  genres: '',
  overview: '',
})

function normID(v: string) {
  const m = /(tt\d{7,10})/i.exec(v.trim())
  return m ? m[1].toLowerCase() : ''
}

function normTMDbID(v: string) {
  const n = Number(String(v ?? '').trim())
  return Number.isInteger(n) && n > 0 ? n : 0
}

function openEdit(c?: Card) {
  const d = c ?? detail.value?.item
  if (!d) return
  const lv = detail.value?.local
  editForm.value = {
    key: d.key,
    title: d.name ?? '',
    original: d.original ?? '',
    year: d.year ? String(d.year) : lv?.year ?? '',
    rating: d.rating ? String(d.rating) : lv?.rating ?? '',
    tmdb_id: d.tmdb_id ?? lv?.tmdb_id ?? '',
    imdb_id: d.imdb_id ?? lv?.imdb_id ?? '',
    genres: (d.genres?.length ? d.genres : lv?.genres ?? []).join(', '),
    overview: d.overview ?? lv?.overview ?? '',
  }
  editOriginalID.value = normID(editForm.value.imdb_id)
  editOriginalTMDbID.value = normTMDbID(editForm.value.tmdb_id)
  editOpen.value = true
}

const imdbChanged = computed(() => {
  const cur = normID(editForm.value.imdb_id)
  return cur !== '' && cur !== editOriginalID.value
})

const tmdbChanged = computed(() => {
  const cur = normTMDbID(editForm.value.tmdb_id)
  return cur > 0 && cur !== editOriginalTMDbID.value
})

const idChanged = computed(() => tmdbChanged.value || imdbChanged.value)

async function submitEdit() {
  const f = editForm.value
  if (!f.key) return
  editSaving.value = true
  try {
    // 换 ID = 换了一部片：只发 ID，让后端按新 ID 重刮全量元数据。
    // 若把旧字段一起提交，EditMeta 会把刚刮回来的新元数据又覆盖成旧值。
    const tmdbID = normTMDbID(f.tmdb_id)
    const body: Record<string, unknown> = idChanged.value
      ? tmdbChanged.value
        ? { key: f.key, tmdb_id: tmdbID }
        : { key: f.key, imdb_id: f.imdb_id }
      : {
          key: f.key,
          title: f.title,
          original: f.original,
          year: f.year,
          rating: f.rating,
          tmdb_id: tmdbID,
          imdb_id: f.imdb_id,
          overview: f.overview,
          genres: f.genres
            .split(/[,，、]/)
            .map((s) => s.trim())
            .filter(Boolean),
        }
    const r: any = await api.catalog.update(body)
    editOpen.value = false
    toast.success(
      tmdbChanged.value
        ? '已按新 TMDB ID 重刮'
        : imdbChanged.value
          ? '已按新 IMDb ID 重刮'
          : '元数据已更新',
      '本地 nfo 与台账已重写，Emby 会在刷新后生效',
    )
    if (detail.value && r?.item) {
      detail.value = {
        item: r.item as Card,
        episodes: detail.value.episodes,
        local: (r.local ?? detail.value.local) as LocalView,
      }
    }
    await load()
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    editSaving.value = false
  }
}

/* ---------------- 删：移除条目 ---------------- */
const delOpen = ref(false)
const delTarget = ref<Card | null>(null)
const purge = ref(false)
const deleting = ref(false)

function openDelete(c?: Card) {
  const d = c ?? detail.value?.item
  if (!d) return
  delTarget.value = d
  purge.value = false
  delOpen.value = true
}

async function submitDelete() {
  const c = delTarget.value
  if (!c) return
  deleting.value = true
  try {
    const r: any = await api.catalog.remove(c.key, purge.value)
    toast.success('已从海报墙移除', r?.note)
    delOpen.value = false
    drawer.value = false
    delTarget.value = null
    await load()
  } catch (e: any) {
    toast.warning('删除失败', e?.message)
  } finally {
    deleting.value = false
  }
}

/* ---------------- 增：从 115 目录选片 ---------------- */
const addOpen = ref(false)
const pickerOpen = ref(false)
const addLibId = ref(0)
const addCid = ref('')
const addRemotePath = ref('')
const addDirLabel = ref('')
const addRelDir = ref('')
const scanResult = ref<any>(null)
const scanning = ref(false)
const adding = ref(false)
// 一个库可能挂了多个网盘（115 + 夸克）：这里存每个库的来源清单，
// 弹窗里可以选择「从哪个网盘加」，否则永远只能从主账号（多半是 115）加。
const libSources = ref<Record<number, LibSource[]>>({})
const addAccountId = ref(0)
const addAccountKind = computed(() => {
  const src = (libSources.value[addLibId.value] || []).find((x) => x.account_id === addAccountId.value)
  return src?.account_kind || src?.kind || '115'
})

function openAdd() {
  addLibId.value = libId.value || libs.value[0]?.id || 0
  addAccountId.value = (libSources.value[addLibId.value] || [])[0]?.account_id || 0
  addCid.value = ''
  addRemotePath.value = ''
  addDirLabel.value = ''
  addRelDir.value = ''
  scanResult.value = null
  addOpen.value = true
}

// 换库/换网盘：之前选的目录属于另一个网盘，留着会算错落盘路径。
watch(addLibId, (id) => {
  addAccountId.value = (libSources.value[id] || [])[0]?.account_id || 0
  addCid.value = ''
  addRemotePath.value = ''
  addDirLabel.value = ''
  scanResult.value = null
})
watch(addAccountId, () => {
  addCid.value = ''
  addRemotePath.value = ''
  addDirLabel.value = ''
  scanResult.value = null
})

function onPick(v: { cid: string; path: string; name: string }) {
  addCid.value = v.cid
  addRemotePath.value = v.path
  addDirLabel.value = v.name
  addRelDir.value = '' // 留空 → 后端按库的 remote_path 自动换算，与同步路径一致
  scanResult.value = null
}

function pickBody() {
  return {
    library_id: addLibId.value,
    // 带上来源账号：夸克目录的 cid 只有夸克账号认得，
    // 后端按它决定用谁的凭证读目录、写 q/ 前缀的播放地址。
    account_id: addAccountId.value || undefined,
    cid: addCid.value,
    rel_dir: addRelDir.value,
    remote_path: addRemotePath.value,
  }
}

async function scanDir() {
  if (!addLibId.value || !addCid.value) {
    toast.warning('先选媒体库和网盘目录')
    return
  }
  scanning.value = true
  try {
    const r: any = await api.catalog.scan(pickBody())
    scanResult.value = r
    if (typeof r?.rel_dir === 'string') addRelDir.value = r.rel_dir
  } catch (e: any) {
    toast.warning('预览失败', e?.message)
  } finally {
    scanning.value = false
  }
}

async function submitAdd() {
  if (!addLibId.value || !addCid.value) {
    toast.warning('先选媒体库和网盘目录')
    return
  }
  adding.value = true
  try {
    const r: any = await api.catalog.add(pickBody())
    toast.success(`已添加 ${r?.added ?? 0} 个视频`, r?.note)
    addOpen.value = false
    await load()
    if (!r?.scrape_started) toast.info('已有刮削任务在跑，稍后刷新即可看到海报')
  } catch (e: any) {
    toast.warning('添加失败', e?.message)
  } finally {
    adding.value = false
  }
}

onMounted(async () => {
  await load()
  try {
    const list: any = await api.libraries.list()
    const arr: any[] = Array.isArray(list) ? list : (list?.libraries ?? [])
    const map: Record<number, LibSource[]> = {}
    for (const l of arr) {
      if (!l?.id) continue
      const srcs = (l.sources ?? []) as LibSource[]
      map[Number(l.id)] = srcs.length
        ? srcs
        : [{ account_id: Number(l.account_id), account_kind: '115', remote_path: l.remote_path, remote_cid: l.remote_cid }]
    }
    libSources.value = map
  } catch {
    /* 多账号时 DirPicker 自己会退化成「第一个 115 账号」 */
  }
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <!-- 控制台：数据源 / 筛选 / 添加 -->
    <HPanel title="我的媒体库" desc="网盘里已有的影视，默认直接读 Emby；增删改都同步落回本地与媒体服务器">
      <template #actions>
        <div class="segmented" role="group" aria-label="数据源">
          <button :class="{ 'is-active': source === 'tmdb' }" @click="source = 'tmdb'">TMDB</button>
          <button :class="{ 'is-active': source === 'emby' }" @click="source = 'emby'">Emby</button>
          <button :class="{ 'is-active': source === 'local' }" @click="source = 'local'">本地台账</button>
        </div>
        <button class="btn btn-primary btn-sm" @click="openAdd">
          <AppIcon name="plus" :size="14" />从 115 添加
        </button>
        <button class="btn btn-tertiary btn-sm" title="去刮削与字幕" @click="emit('go-scrape')">
          <AppIcon name="scrape" :size="14" />刮削
        </button>
        <button
          class="btn btn-tertiary btn-sm"
          title="让 Emby 重新扫描媒体库，再从 Emby 同步一次"
          :disabled="syncing || loading"
          @click="syncEmby"
        >
          <AppIcon name="server" :size="14" />{{ syncing ? '同步中…' : '从 Emby 同步' }}
        </button>
        <button class="btn btn-icon btn-sm" title="刷新" :disabled="loading" @click="load">
          <AppIcon name="refresh" :size="15" />
        </button>
      </template>

      <div class="row wrap filter-row" style="gap: 8px">
        <select v-model.number="libId" class="select-native" style="min-width: 150px">
          <option :value="0">全部媒体库</option>
          <option v-for="l in libs" :key="l.id" :value="l.id">{{ l.name }}</option>
        </select>
        <label class="search" style="min-width: 210px; flex: 1">
          <AppIcon name="search" :size="15" />
          <input v-model="search" class="input" placeholder="搜片名或路径…" />
        </label>
        <div class="segmented">
          <button
            v-for="c in typeChips"
            :key="c.v"
            :class="{ 'is-active': typ === c.v }"
            @click="typ = c.v"
          >
            {{ c.label }}
          </button>
        </div>
        <select v-model="genre" class="select-native" style="min-width: 120px">
          <option value="">全部类型</option>
          <option v-for="g in genres" :key="g" :value="g">{{ g }}</option>
        </select>
        <select v-model="sortKey" class="select-native" style="min-width: 120px">
          <option value="recent">最近入库</option>
          <option value="title">片名 A→Z</option>
          <option value="rating">评分最高</option>
          <option value="year">年份最新</option>
        </select>
      </div>

      <p v-if="hint" class="notice-line">
        <AppIcon name="info" :size="14" />{{ hint }}
      </p>
      <p v-if="err" class="notice-line danger-line">
        <AppIcon name="alert" :size="14" />{{ err }}
      </p>
      <p v-if="activeSource === 'tmdb' && !hint" class="muted small" style="margin-top: 10px">
        当前按台账的 TMDB ID 现查元数据，不依赖媒体服务器；想看服务器里最终生效的 nfo 就切 Emby。
      </p>
      <p v-if="activeSource === 'local' && !hint" class="muted small" style="margin-top: 10px">
        当前显示本地刮削台账：包含网盘里已有、但 Emby 还没扫到的片目。
      </p>
    </HPanel>

    <!-- 统计 -->
    <div class="grid-4">
      <div class="stat">
        <span class="stat-label"><AppIcon name="grid" :size="14" />当前结果</span>
        <span class="stat-value">{{ total.toLocaleString() }}<small>部</small></span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="server" :size="14" />Emby 条目</span>
        <span class="stat-value">{{ embyTotal.toLocaleString() }}</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="scrape" :size="14" />本地台账</span>
        <span class="stat-value">{{ localTotal.toLocaleString() }}</span>
      </div>
      <div class="stat">
        <span class="stat-label"><AppIcon name="shield" :size="14" />数据源</span>
        <span class="stat-value" style="font-size: 19px; padding-top: 5px">
          <span
            class="badge"
            :class="activeSource === 'tmdb' ? 'badge-success' : embyOK ? 'badge-success' : 'badge-warning'"
          >
            {{ sourceLabel }}
          </span>
        </span>
      </div>
    </div>

    <!-- 海报墙 -->
    <HPanel title="海报墙" desc="点卡片看详情，悬停可直接编辑或删除">
      <template #actions>
        <span class="badge badge-accent">{{ libs.length || 0 }} 个媒体库</span>
      </template>

      <div v-if="items.length" class="poster-grid wall-grid">
        <div
          v-for="it in items"
          :key="it.key"
          class="poster wall-card"
          role="button"
          tabindex="0"
          @click="openDetail(it)"
          @keydown.enter="openDetail(it)"
        >
          <div class="poster-art">
            <img
              v-if="it.poster && !broken[it.key]"
              :src="it.poster"
              alt=""
              loading="lazy"
              decoding="async"
              @error="markBroken(it.key)"
            />
            <div v-else class="ph" :style="phStyle(it)">{{ it.name }}</div>
            <span v-if="it.rating" class="rate">★ {{ it.rating.toFixed(1) }}</span>
            <span class="kind-tag">{{ it.type === 'series' ? '剧集' : '电影' }}</span>
            <div class="card-actions">
              <button class="ca" title="编辑元数据" @click.stop="openEdit(it)">
                <AppIcon name="edit" :size="14" />
              </button>
              <button class="ca danger" title="从墙面移除" @click.stop="openDelete(it)">
                <AppIcon name="trash" :size="14" />
              </button>
            </div>
          </div>
          <span class="poster-title">
            <TitleLink :type="it.type" :tmdb-id="it.tmdb_id" :media-id="it.media_id" :title="it.name" :year="it.year">{{ it.name }}</TitleLink>
          </span>
          <span class="poster-sub">
            {{ it.year || '—' }} · {{ it.library || '未分类' }}
            <template v-if="it.episodes && it.type === 'series'"> · {{ it.episodes }} 集</template>
          </span>
        </div>
      </div>

      <div v-else-if="loading" class="poster-grid wall-grid">
        <div v-for="i in 12" :key="i" class="poster">
          <div class="skeleton" style="aspect-ratio: 2 / 3; border-radius: var(--r-md)" />
          <div class="skeleton" style="height: 12px; width: 80%" />
          <div class="skeleton" style="height: 10px; width: 50%" />
        </div>
      </div>

      <HEmpty
        v-else
        title="这里还没有片目"
        :desc="err || (search ? '换个关键词或筛选条件试试' : '先从网盘目录添加一批，或跑一次同步让 Emby 扫到已有的 .strm')"
        icon="local"
      >
        <button class="btn btn-sm btn-primary" @click="openAdd">
          <AppIcon name="plus" :size="13" />从 115 添加
        </button>
      </HEmpty>

      <template #footer>
        <div class="row-between">
          <span class="muted small">共 {{ total.toLocaleString() }} 部 · 第 {{ page }} 页 · 每页 {{ PAGE_SIZE }} 部</span>
          <div class="row" style="gap: 6px">
            <button class="btn btn-sm btn-tertiary" :disabled="page <= 1 || loading" @click="prevPage">
              <AppIcon name="chevronLeft" :size="13" />上一页
            </button>
            <button class="btn btn-sm btn-tertiary" :disabled="!hasMore || loading" @click="nextPage">
              下一页<AppIcon name="chevronRight" :size="13" />
            </button>
          </div>
        </div>
      </template>
    </HPanel>

    <!-- 详情抽屉 -->
    <Teleport to="body">
      <Transition name="fade">
        <div v-if="drawer" class="drawer-overlay" @click.self="drawer = false">
          <aside class="drawer">
            <header class="drawer-head">
              <div class="row-between">
                <strong>条目详情</strong>
                <button class="btn btn-icon btn-sm" aria-label="关闭" @click="drawer = false">
                  <AppIcon name="close" :size="16" />
                </button>
              </div>
            </header>

            <div v-if="detailLoading" class="drawer-body">
              <div class="skeleton" style="height: 160px; border-radius: var(--r-md)" />
              <div class="skeleton" style="height: 20px; width: 60%; margin-top: 16px" />
              <div class="skeleton" style="height: 12px; width: 90%; margin-top: 12px" />
            </div>

            <div v-else-if="detail" class="drawer-body">
              <div class="hero">
                <img v-if="detail.item.backdrop" :src="detail.item.backdrop" alt="" />
                <div class="hero-fade" />
                <div class="hero-text">
                  <h2>
                    <TitleLink :type="detail.item.type" :tmdb-id="detail.item.tmdb_id" :media-id="detail.item.media_id" :title="detail.item.name" :year="detail.item.year">{{ detail.item.name }}</TitleLink>
                  </h2>
                  <p v-if="detail.item.original && detail.item.original !== detail.item.name" class="muted small">
                    {{ detail.item.original }}
                  </p>
                </div>
              </div>

              <div class="row wrap" style="gap: 6px; margin-top: 12px">
                <span class="badge badge-accent">{{ detail.item.type === 'series' ? '剧集' : '电影' }}</span>
                <span v-if="detail.item.year" class="badge">{{ detail.item.year }}</span>
                <span v-if="detail.item.rating" class="badge badge-warning">★ {{ detail.item.rating.toFixed(1) }}</span>
                <span v-if="detail.item.episodes" class="badge">{{ detail.item.episodes }} 集</span>
                <span v-if="detail.item.library" class="badge">{{ detail.item.library }}</span>
              </div>

              <div class="kv-grid">
                <div class="kv"><span>元数据</span><b>{{ detail.item.nfo ? '本地 NFO 已写' : '尚未写 NFO' }}</b></div>
                <div class="kv">
                  <span>字幕</span>
                  <b>
                    {{ subLabel(detail.item.sub_state) }}
                    <span v-if="detail.item.sub_origin === 'translated'" class="muted small">（AI 翻译）</span>
                  </b>
                </div>
                <div class="kv"><span>媒体库</span><b>{{ detail.item.library || '—' }}</b></div>
                <div class="kv"><span>入库时间</span><b>{{ shortDate(detail.item.added) || '—' }}</b></div>
              </div>

              <div class="row wrap" style="gap: 6px; margin-top: 10px">
                <span v-if="embyChecked" class="badge" :class="detail.item.in_emby ? 'badge-success' : ''">
                  {{ detail.item.in_emby ? 'Emby 已收录' : 'Emby 未收录' }}
                </span>
                <span class="badge" :class="subTone(detail.item.sub_state)">
                  {{ detail.local?.exists ? '有本地台账' : '无本地台账' }}
                </span>
                <a v-if="tmdbLink" class="badge badge-accent link-badge" :href="tmdbLink" target="_blank" rel="noopener">
                  <AppIcon name="external" :size="12" />TMDB {{ detail.item.tmdb_id }}
                </a>
                <a v-if="imdbLink" class="badge badge-info link-badge" :href="imdbLink" target="_blank" rel="noopener">
                  <AppIcon name="external" :size="12" />IMDb {{ detail.item.imdb_id }}
                </a>
              </div>

              <template v-if="detail.item.overview">
                <h3 class="section-title">简介</h3>
                <p class="overview">{{ detail.item.overview }}</p>
              </template>

              <template v-if="detail.item.genres?.length">
                <h3 class="section-title">类型</h3>
                <div class="row wrap" style="gap: 6px">
                  <span v-for="g in detail.item.genres" :key="g" class="chip">{{ g }}</span>
                </div>
              </template>

              <template v-if="detail.item.cast?.length">
                <h3 class="section-title">演员</h3>
                <div class="cast-row">
                  <div
                    v-for="c in detail.item.cast.slice(0, 12)"
                    :key="c.name"
                    class="cast-item"
                    :title="c.role ? c.name + ' 饰 ' + c.role : c.name"
                  >
                    <img v-if="c.image" :src="c.image" class="cast-avatar" alt="" loading="lazy" />
                    <span v-else class="cast-avatar cast-fallback">{{ initialOf(c.name) }}</span>
                    <span class="cast-name">{{ c.name }}</span>
                    <span v-if="c.role" class="cast-role">{{ c.role }}</span>
                  </div>
                </div>
                <p class="muted small" style="margin-top: 8px">
                  头像来自 TMDB，写在本地 nfo 的 &lt;thumb&gt; 里；改完要重刮一次才会有。
                </p>
              </template>

              <!-- 老条目（重刮之前）只有一串名字，仍然显示出来，别让它凭空消失 -->
              <template v-else-if="detail.item.people?.length">
                <h3 class="section-title">演员</h3>
                <p class="muted small">{{ detail.item.people.slice(0, 12).join(' · ') }}</p>
              </template>

              <h3 class="section-title">本地路径</h3>
              <p class="mono path-line">{{ detail.item.rel_path || detail.item.path || '—' }}</p>
              <p v-if="detail.local?.nfo_file" class="mono path-line muted small">
                nfo：{{ detail.local.nfo_file }}
              </p>
              <p v-if="detail.local?.sub_file" class="mono path-line muted small">
                字幕：{{ detail.local.sub_file }}
              </p>

              <template v-if="detail.episodes.length">
                <h3 class="section-title">分集（{{ detail.episodes.length }}）</h3>
                <div class="ep-list">
                  <div v-for="(ep, i) in visibleEpisodes" :key="`${ep.rel_path}-${i}`" class="ep-row">
                    <span class="mono ep-no">
                      {{ ep.season ? `S${String(ep.season).padStart(2, '0')}` : '' }}{{ ep.episode ? `E${String(ep.episode).padStart(2, '0')}` : '' }}
                    </span>
                    <span class="truncate grow">{{ ep.name }}</span>
                    <span v-if="ep.nfo" class="mini-tag">nfo</span>
                    <span v-if="ep.sub_state === 'ok'" class="mini-tag ok">字幕</span>
                  </div>
                </div>
                <button
                  v-if="detail.episodes.length > 12"
                  class="btn btn-sm btn-ghost"
                  style="margin-top: 8px"
                  @click="showAllEp = !showAllEp"
                >
                  {{ showAllEp ? '收起' : `展开全部 ${detail.episodes.length} 集` }}
                </button>
              </template>
            </div>

            <footer v-if="detail" class="drawer-foot">
              <button class="btn btn-tertiary" :disabled="subBusy" @click="fetchSubtitle()">
                <AppIcon name="file" :size="14" />{{ subBusy ? '处理中…' : '补中文字幕' }}
              </button>
              <button
                class="btn btn-ghost"
                :disabled="subBusy"
                title="重新下载并覆盖已有字幕；会通知 Emby 重读"
                @click="fetchSubtitle(true)"
              >
                <AppIcon name="refresh" :size="14" />重新下载
              </button>
              <button class="btn btn-primary" @click="openEdit()">
                <AppIcon name="edit" :size="14" />编辑
              </button>
              <button class="btn btn-danger" @click="openDelete()">
                <AppIcon name="trash" :size="14" />删除
              </button>
            </footer>
          </aside>
        </div>
      </Transition>
    </Teleport>

    <!-- 编辑元数据 -->
    <HModal v-model="editOpen" title="编辑元数据" size="lg">
      <template #desc>改写本地 nfo 与台账，不联网重刮；Emby 刷新后生效</template>
      <div class="col" style="gap: 12px">
        <div class="grid-2">
          <HField label="片名"><input v-model="editForm.title" class="input" /></HField>
          <HField label="原名"><input v-model="editForm.original" class="input" /></HField>
          <HField label="年份"><input v-model="editForm.year" class="input" placeholder="2026" /></HField>
          <HField label="评分"><input v-model="editForm.rating" class="input" placeholder="7.5" /></HField>
        </div>
        <div class="grid-2">
          <HField label="TMDB ID" hint="海报墙按它直查 TMDB；改了会按新 ID 重刮全量元数据">
            <input v-model="editForm.tmdb_id" class="input input-mono" placeholder="693134" inputmode="numeric" />
          </HField>
          <HField label="IMDb ID" hint="可粘整条 imdb.com 链接；改了会走 TMDB /find 反查并重刮">
            <input v-model="editForm.imdb_id" class="input input-mono" placeholder="tt15239678" />
          </HField>
        </div>
        <HField label="类型" hint="用逗号分隔"><input v-model="editForm.genres" class="input" placeholder="动作, 科幻" /></HField>
        <HField label="简介"><textarea v-model="editForm.overview" class="textarea" rows="4" /></HField>
        <div v-if="idChanged" class="notice-line">
          <AppIcon name="info" :size="14" />检测到{{ tmdbChanged ? ' TMDB ID ' : ' IMDb ID ' }}变化：将按新 ID 重刮全量元数据，其它输入本次不生效。
        </div>
        <p class="muted small">留空的字段保持原值。修改会同步写入元数据缓存，下次刮削不会把你改的内容覆盖回去。</p>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="editOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="editSaving" @click="submitEdit">
          {{ editSaving ? '保存中…' : '保存' }}
        </button>
      </template>
    </HModal>

    <!-- 删除确认 -->
    <HModal v-model="delOpen" title="从海报墙移除">
      <template #desc>这一步会同时清理本地 .strm、nfo、海报、字幕、台账和 Emby 条目</template>
      <div class="col" style="gap: 12px">
        <div class="warnbox">
          <AppIcon name="alert" :size="16" />
          <div>
            <strong>{{ delTarget?.name }}</strong>
            <p class="mono small" style="margin-top: 4px">{{ delTarget?.rel_path || delTarget?.path || '—' }}</p>
          </div>
        </div>
        <div class="row-between switch-row">
          <div>
            <strong class="small">同时删除 115 网盘里的源文件</strong>
            <p class="muted small">源文件会进 115 回收站，可恢复；不勾选则网盘文件保留，但下次同步会重新生成。</p>
          </div>
          <HSwitch v-model="purge" label="同时删除 115 源文件" />
        </div>
        <p v-if="purge" class="notice-line danger-line">
          <AppIcon name="alert" :size="14" />将调用 115 删除接口，本地所有相关文件也会一并移除。
        </p>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="delOpen = false">取消</button>
        <button class="btn btn-danger" :disabled="deleting" @click="submitDelete">
          {{ deleting ? '删除中…' : purge ? '删除条目与源文件' : '只移除索引' }}
        </button>
      </template>
    </HModal>

    <!-- 从网盘添加：115 / 夸克TV 都走同一个目录选择器 -->
    <HModal v-model="addOpen" title="从网盘目录添加影视" size="lg">
      <template #desc>选中一个网盘目录，把里面的视频生成 .strm 并自动刮削入库</template>
      <div class="col" style="gap: 12px">
        <div class="grid-2">
          <HField label="目标媒体库" required>
            <select v-model.number="addLibId" class="select-native">
              <option v-if="!libs.length" :value="0">还没有媒体库</option>
              <option v-for="l in libs" :key="l.id" :value="l.id">{{ l.name }}</option>
            </select>
          </HField>
          <HField
            v-if="(libSources[addLibId] || []).length > 1"
            label="从哪个网盘加"
            tip="这个库挂了多个网盘目录；选夸克就从夸克目录里挑片，.strm 会写夸克的播放地址"
          >
            <select v-model.number="addAccountId" class="select-native">
              <option
                v-for="src in libSources[addLibId] || []"
                :key="src.account_id"
                :value="src.account_id"
              >
                {{ (src.account_kind || src.kind) === 'quarktv' ? '夸克' : '115' }} ·
                {{ src.remote_path || src.remote_cid }}
              </option>
            </select>
          </HField>
          <HField label="网盘目录" required>
            <button class="btn btn-tertiary btn-block" @click="pickerOpen = true">
              <AppIcon name="folder" :size="14" />
              <span class="truncate">{{ addDirLabel || '选择目录…' }}</span>
            </button>
          </HField>
        </div>
        <HField label="库内子目录" hint="留空由系统按库的网盘根路径自动换算，保证与同步生成的 .strm 不重复；预览后可修改">
          <input v-model="addRelDir" class="input input-mono" placeholder="留空自动换算，如 2026/空枪" />
        </HField>

        <div class="row wrap" style="gap: 8px">
          <button class="btn btn-tertiary" :disabled="!addLibId || !addCid || scanning" @click="scanDir">
            <AppIcon name="search" :size="14" />{{ scanning ? '预览中…' : '预览可添加的片' }}
          </button>
          <span class="grow" />
          <span class="muted small">最多递归 4 层 / 24 个目录 / 300 个视频，避免风控</span>
        </div>

        <div v-if="scanResult" class="scan-box">
          <div class="row wrap" style="gap: 8px">
            <span class="badge badge-success">可添加 {{ scanResult.will_add }}</span>
            <span class="badge">已入库跳过 {{ scanResult.will_skip }}</span>
            <span class="badge">{{ scanResult.videos }} 个视频</span>
            <span class="muted small mono">→ {{ scanResult.rel_dir || '库根目录' }}</span>
          </div>
          <div v-if="scanResult.items?.length" class="scan-list">
            <div v-for="(v, i) in scanResult.items" :key="`${v.file_id}-${i}`" class="scan-row">
              <AppIcon name="file" :size="13" />
              <span class="truncate grow">{{ v.name }}</span>
              <span class="muted small mono">{{ v.rel_dir || '/' }}</span>
            </div>
          </div>
          <p v-if="!scanResult.will_add" class="muted small" style="margin-top: 8px">
            这个目录里的视频都已经入库了，换个目录看看。
          </p>
        </div>

        <div class="notice-line">
          <AppIcon name="info" :size="14" />添加后会写入 .strm 并自动刮削元数据、补字幕，Emby 刷新后海报墙出现新卡片。
        </div>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="addOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="!addLibId || !addCid || adding" @click="submitAdd">
          <AppIcon name="plus" :size="14" />{{ adding ? '添加中…' : '添加到媒体库' }}
        </button>
      </template>
    </HModal>

    <DirPicker
      v-model="pickerOpen"
      :account-id="addAccountId || undefined"
      title="选择网盘目录"
      desc="这个目录（含子目录）里的视频会被生成 .strm"
      @pick="onPick"
    />
  </div>
</template>

<style scoped>
/* 演员表：横排头像 + 名字 + 角色。窄屏自动换行，不横向滚动 ——
   抽屉本身就是一个滚动容器，里面再套一条横向滚动条很难用。 */
.cast-row {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
}
.cast-item {
  width: 82px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 5px;
  text-align: center;
}
.cast-avatar {
  width: 56px;
  height: 56px;
  border-radius: 50%;
  object-fit: cover;
  background: var(--surface-tertiary);
  border: 1px solid var(--border-secondary);
}
.cast-fallback {
  display: grid;
  place-items: center;
  font-size: 15px;
  font-weight: 600;
  color: var(--muted);
}
.cast-name {
  font-size: 11.5px;
  line-height: 1.25;
  width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cast-role {
  font-size: 11px;
  line-height: 1.2;
  width: 100%;
  color: var(--muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.wall-grid {
  grid-template-columns: repeat(auto-fill, minmax(138px, 1fr));
}
/* 表单控件全局默认 width:100%，放进 flex 行里会各自撑满一整行 */
.filter-row > .select-native {
  width: auto;
  flex: none;
}
.filter-row > .segmented {
  flex: none;
}
.filter-row > .search {
  width: auto;
  min-width: 200px;
}
.wall-card {
  cursor: pointer;
  outline: none;
}
.wall-card:focus-visible .poster-art {
  box-shadow: 0 0 0 2px var(--accent);
}
.kind-tag {
  position: absolute;
  left: 7px;
  top: 7px;
  padding: 1px 7px;
  border-radius: var(--r-full);
  background: rgb(10 8 6 / 0.7);
  color: #fff;
  font-size: 10.5px;
  font-weight: 600;
}
.card-actions {
  position: absolute;
  right: 6px;
  bottom: 6px;
  display: flex;
  gap: 5px;
  opacity: 0;
  transform: translateY(4px);
  transition: opacity 0.18s, transform 0.18s;
}
.wall-card:hover .card-actions,
.wall-card:focus-within .card-actions {
  opacity: 1;
  transform: translateY(0);
}
.ca {
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  border: 1px solid rgb(255 255 255 / 0.22);
  border-radius: 8px;
  background: rgb(16 14 12 / 0.82);
  color: #fff;
}
.ca:hover {
  background: var(--accent);
  border-color: var(--accent);
}
.ca.danger:hover {
  background: var(--danger);
  border-color: var(--danger);
}
.notice-line {
  display: flex;
  align-items: flex-start;
  gap: 7px;
  margin-top: 10px;
  padding: 9px 11px;
  border-radius: var(--r-sm);
  background: var(--accent-soft);
  color: var(--foreground);
  font-size: 12.5px;
  line-height: 1.5;
}
.notice-line svg {
  flex: none;
  margin-top: 1px;
}
.danger-line {
  background: color-mix(in oklab, var(--danger) 12%, var(--surface));
  color: var(--danger);
}

/* ---------- 抽屉 ---------- */
.drawer-overlay {
  position: fixed;
  inset: 0;
  z-index: calc(var(--z-overlay) - 1);
  background: rgb(6 6 8 / 0.46);
  backdrop-filter: blur(2px);
}
.drawer-head {
  padding: 14px 18px;
  border-bottom: 1px solid var(--separator);
  font-size: 14px;
}
.drawer-foot {
  display: flex;
  gap: 8px;
  padding: 12px 18px;
  border-top: 1px solid var(--separator);
}
.hero {
  position: relative;
  height: 172px;
  border-radius: var(--r-md);
  overflow: hidden;
  background: linear-gradient(140deg, var(--surface-secondary), var(--accent-soft));
}
.hero img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.hero-fade {
  position: absolute;
  inset: 0;
  background: linear-gradient(180deg, transparent 30%, rgb(0 0 0 / 0.78));
}
.hero-text {
  position: absolute;
  left: 14px;
  right: 14px;
  bottom: 12px;
  color: #fff;
}
.hero-text h2 {
  font-size: 19px;
  line-height: 1.3;
  margin: 0;
  text-shadow: 0 1px 10px rgb(0 0 0 / 0.5);
}
.hero-text p {
  color: rgb(255 255 255 / 0.76);
  margin: 3px 0 0;
}
.kv-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px 14px;
  margin-top: 14px;
  padding: 12px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.kv {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
  font-size: 12px;
}
.kv span {
  color: var(--muted);
}
.kv b {
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.section-title {
  margin: 18px 0 7px;
  font-size: 12.5px;
  font-weight: 650;
  color: var(--muted);
  letter-spacing: 0.02em;
}
.overview {
  font-size: 13px;
  line-height: 1.7;
  color: var(--foreground);
  white-space: pre-wrap;
}
.link-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  text-decoration: none;
}
.path-line {
  font-size: 11.5px;
  line-height: 1.6;
  overflow-wrap: anywhere;
  padding: 7px 9px;
  border-radius: 6px;
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.ep-list {
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  overflow: hidden;
}
.ep-row {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 7px 10px;
  border-bottom: 1px solid var(--separator);
  font-size: 12.5px;
}
.ep-row:last-child {
  border-bottom: 0;
}
.ep-no {
  flex: none;
  width: 52px;
  color: var(--muted);
  font-size: 11px;
}
.mini-tag {
  flex: none;
  padding: 1px 6px;
  border-radius: 5px;
  background: var(--surface-secondary);
  color: var(--muted);
  font-size: 10.5px;
}
.mini-tag.ok {
  background: color-mix(in oklab, var(--success) 16%, var(--surface));
  color: var(--success);
}

/* ---------- 弹窗内 ---------- */
.warnbox {
  display: flex;
  gap: 10px;
  padding: 12px;
  border-radius: var(--r-md);
  background: color-mix(in oklab, var(--danger) 9%, var(--surface));
  border: 1px solid color-mix(in oklab, var(--danger) 24%, var(--border));
  color: var(--danger);
}
.warnbox svg {
  flex: none;
  margin-top: 2px;
}
.warnbox strong {
  color: var(--foreground);
  font-size: 14px;
}
.switch-row {
  gap: 14px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
}
.scan-box {
  padding: 11px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.scan-list {
  margin-top: 9px;
  max-height: 180px;
  overflow: auto;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
}
.scan-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 9px;
  border-bottom: 1px solid var(--separator);
  font-size: 12px;
}
.scan-row:last-child {
  border-bottom: 0;
}
@media (max-width: 720px) {
  .kv-grid {
    grid-template-columns: 1fr;
  }
}
</style>
