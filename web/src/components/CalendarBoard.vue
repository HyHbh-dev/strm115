<script lang="ts">
/**
 * 模块级共享缓存：故意放在普通 <script>（每个模块只执行一次），而不是
 * <script setup>（顶层代码每个组件实例都重跑一次）。父页面用 v-else-if 切
 * tab 会销毁 / 重建 CalendarBoard，缓存只有放这里才能跨挂载存活 —— 切回
 * 「播出日历」直接复用，不重复打 /api/calendar 与 /api/calendar/douban。
 *
 * 值用 unknown：具体类型（CalWeek / DoubanPayload）定义在 <script setup> 内、
 * 对本块不可见；load() 读取时再 as 成对应类型，运行期结构由后端 JSON 决定。
 * seq 用对象包一层：避免模块作用域可变的 let 被编译器以 getter/setter 特殊处理。
 */
const weekCache = new Map<string, unknown>()
const doubanCache = new Map<string, unknown>()
const shared = { seq: 0 }
</script>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HEmpty from '@/components/HEmpty.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { forgetSub, loadSubsIndex, rememberSub, subIdOf, subRowOf } from '@/stores/subscriptions'
import type { SubRow } from '@/stores/subscriptions'
import { openDetail } from '@/stores/detail'
import TitleLink from '@/components/TitleLink.vue'
import SubLineSlider from '@/components/SubLineSlider.vue'
import { fmtSize, KIND_FILTERS, SIZE_FILTERS, useResSearch } from '@/composables/useResSearch'
import type { ResItem } from '@/composables/useResSearch'
import { use115Share } from '@/composables/use115Share'
import { pad2, fmtDay as fmtDate, copyText } from '@/utils/format'

/**
 * 播出日历面板（照 Trakt Calendar 的版式）：一周七列、每天若干海报卡，
 * 卡下是 SxxExx + 集标题 + 播出时刻 / 电视网，左侧是 MY / 剧集 / 首播 /
 * 新剧 / 电影的筛选。
 *
 * 数据全部来自 /api/calendar：TMDB 出剧单、分集与电影，TVDB 补时刻和电视网。
 * 筛选只在前端本地过滤，不重新打接口 —— 后端整周缓存本来就一次给全。
 *
 * 从 CalendarPage 抽出来做成组件，是因为「发现与订阅」和「播出日历」已经
 * 合成一个页面：日历是那张页面的一个标签。单独留一个页面壳子会让两边各自
 * 维护一份同样的代码，订阅状态、资源搜索这些共享逻辑就会慢慢跑偏。
 */

interface CalEntry {
  key: string
  kind: 'episode' | 'movie'
  date: string
  tmdb_id?: number
  tvdb_id?: number
  title: string
  original?: string
  poster?: string
  backdrop?: string
  season?: number
  episode?: number
  code?: string
  episode_title?: string
  air_time?: string
  network?: string
  runtime?: number
  rating?: number
  overview?: string
  subscribed?: boolean
  premiere?: boolean
  new_show?: boolean
  source?: string
  page_url?: string
  /** 只在 source=douban 时有：TMDB 上可能没有这部片，不能拿 tmdb_id 去开详情 */
  douban_id?: number
  /** 豆瓣原话「更新至12集」 */
  episodes_info?: string
}

/** 豆瓣榜单里的一条（国产剧那一栏用）。字段名与后端 douban.Item 对齐。 */
interface DoubanItem {
  id: number
  kind: 'movie' | 'tv'
  title: string
  original?: string
  year?: number
  poster?: string
  rating?: number
  episodes?: number
  episodes_info?: string
  release_date?: string
  release_text?: string
  card_subtitle?: string
  url?: string
}

interface CalDay {
  date: string
  weekday: string
  label: string
  items: CalEntry[]
}

type CalDayView = CalDay & { hidden: number }

interface CalWeek {
  start: string
  end: string
  days: CalDay[]
  total: number
  cached: boolean
  cached_at?: string
  sources: {
    tmdb: boolean
    source?: 'tvmaze' | 'tvdb' | 'off'
    tvmaze?: boolean
    tvmaze_error?: string
    tvdb: boolean
    tvdb_configured: boolean
    tvdb_error?: string
    attribution?: string
    attribution_url?: string
    shows: number
  }
  notice?: string
  /**
   * 豆瓣那一半：movies 由 /api/calendar/douban 异步拿到后按 date 并进 days
   * （有明确上映日），剧集只能放这里 —— 豆瓣不给国产剧的逐集播出日期，
   * 只给「更新至第 N 集」。主响应里这块只是 enabled=false 的占位。
   */
  douban?: {
    /** 这一轮到底有没有去问豆瓣；设置页关掉时为 false */
    enabled?: boolean
    ok: boolean
    error?: string
    tv?: DoubanItem[]
  }
}

/**
 * /api/calendar/douban 的返回：国产剧 tv + 本周上映的国内电影 movies。
 * movies 是日历 Entry（字段与 CalEntry 一致），前端按 date 并进周历格子。
 */
interface DoubanPayload {
  enabled: boolean
  ok: boolean
  error?: string
  tv?: DoubanItem[]
  movies?: CalEntry[]
}

const emit = defineEmits<{
  /** 点豆瓣剧卡上的「找资源」：交给父页面切到资源搜索标签并直接开搜 */
  (e: 'find', q: string, en?: string): void
}>()

// 模块级缓存 / seq 见文件顶部的普通 <script> 块：必须放在 <script setup>
// 之外，切 tab 销毁 / 重建组件后才能继续存活、不重复请求。
const week = ref<CalWeek | null>(null)
const start = ref('')
const loading = ref(false)
// 国产剧栏独立 loading：豆瓣是异步补的，转圈只在这一栏，不挡住周历主体。
const dbLoading = ref(false)
const errorMsg = ref('')
const filter = ref('all')
// 一天可能有 20 集（日更脱口秀 + 周更剧都算），默认每列只铺 8 张，
// 剩下的用「展开全部」一次放出来 —— 否则首屏要滚十几屏才能到周日。
const perDay = 8
const expanded = ref(false)
const sel = ref<CalEntry | null>(null)
const broken = ref<Record<string, boolean>>({})
const subscribing = ref(false)

/* 抽屉里的资源搜索：和电影墙一样复用 useResSearch / use115Share，
   点「搜索资源」结果就地展开，不跳到「发现与订阅」页把日历上下文丢掉。
   打开别的卡片时整块重置，避免上一部片的结果挂在新标题下面。 */
const res = useResSearch()
const {
  busyKey: busy115,
  loadTarget: loadShareTarget,
  isBusy: isBusy115,
  save: saveTo115,
  offline: offlineTo115,
  offlineURI,
} = use115Share()
const searchOpen = ref(false)
const searchQ = ref('')
const searchEn = ref('')
const resultsRef = ref<HTMLElement | null>(null)

// 每个筛选带一句说明（按钮 title + 空态提示），免得「新剧」和「电影」
// 都空着 / 长得像时被误认为同一个功能（Bug 2）。
const filters = [
  { value: 'all', label: '全部', desc: '本周所有剧集分集与电影' },
  { value: 'mine', label: '我的订阅', desc: '只看已加入订阅的条目' },
  { value: 'tv', label: '剧集', desc: '所有剧集分集（含首播、新剧）' },
  { value: 'premieres', label: '首播', desc: '本季第一集（SxxE01）' },
  { value: 'new', label: '新剧', desc: '第一季第一集、本周新开播的剧（不含电影）' },
  { value: 'movies', label: '电影', desc: '本周上映 / 发行的电影（含 TMDB 与豆瓣）' },
]

/**
 * 这条日历条目订过没有。
 *
 * 后端 /api/calendar 的条目里根本没有 subscribed 字段（它只回播出信息），
 * 所以「我的订阅」筛选以前只看 e.subscribed 时永远是空的 —— 用户明明订了剧，
 * 切到这个筛选却什么都没有。真正的答案在共享索引里，字段只作为「刚订完还没
 * 刷新索引」的本地标记。
 */
function isSubscribed(e: CalEntry): boolean {
  return !!e.subscribed || !!subIdOf(e.kind === 'movie' ? 'movie' : 'tv', e.tmdb_id)
}

/* ---------------- 日历本集详情里的订阅进度 ---------------- */
/**
 * 起播线（原水位线）从共享订阅索引取整行，不再为每张卡片多打一次 /api/title。
 * 详情里改完起播线会 loadSubsIndex(true)，这边下次打开抽屉就是新值。
 */
const subHere = computed<SubRow | null>(() => {
  const e = sel.value
  if (!e?.tmdb_id) return null
  return subRowOf(e.kind === 'movie' ? 'movie' : 'tv', e.tmdb_id)
})

function waterText(s: SubRow) {
  if (!s.watermark_episode) return '还没开始'
  return `S${pad2(s.watermark_season)}E${pad2(s.watermark_episode)}`
}

function matches(e: CalEntry, kind = filter.value): boolean {
  switch (kind) {
    case 'mine':
      return isSubscribed(e)
    case 'tv':
      return e.kind === 'episode'
    case 'premieres':
      return !!e.premiere
    case 'new':
      // Bug 2 加固：明确排除电影与豆瓣条目。以前只判 new_show，TMDB 电影曾被
      // 误标成新剧，导致「新剧」筛选里全是电影、和「电影」筛选看起来一模一样。
      // 后端电影已不标 NewShow，这里再加一道 kind === 'episode' 兜底。
      return !!e.new_show && e.kind === 'episode'
    case 'movies':
      return e.kind === 'movie'
    default:
      return true
  }
}

const allEntries = computed<CalEntry[]>(() =>
  (week.value?.days ?? []).reduce<CalEntry[]>((acc, d) => acc.concat(d.items), []),
)
const visibleDays = computed<CalDayView[]>(() =>
  (week.value?.days ?? []).map((d) => {
    const items = d.items.filter((e) => matches(e))
    return {
      ...d,
      items: expanded.value ? items : items.slice(0, perDay),
      hidden: expanded.value ? 0 : Math.max(0, items.length - perDay),
    }
  }),
)
const visibleTotal = computed(() => visibleDays.value.reduce((n, d) => n + d.items.length, 0))
const hiddenTotal = computed(() => visibleDays.value.reduce((n, d) => n + d.hidden, 0))
const countFor = (kind: string) => allEntries.value.filter((e) => matches(e, kind)).length
const rangeText = computed(() =>
  week.value ? `${week.value.start} ~ ${week.value.end}` : '载入中…',
)

/* ---------------- 豆瓣那一半：国产剧进度 + 国内档期 ---------------- */
const DOUBAN_URL = 'https://movie.douban.com'
/** 国产剧一栏默认铺 8 部，再多要手动展开——它不参与按天排，太长会把周历挤下去。 */
const dbExpanded = ref(false)
const DB_MAX = 8
const dbTV = computed<DoubanItem[]>(() => week.value?.douban?.tv ?? [])
const dbShown = computed(() => (dbExpanded.value ? dbTV.value : dbTV.value.slice(0, DB_MAX)))
/** 只有真的问了豆瓣、而且真的失败时才报错：关掉豆瓣不算失败。 */
const dbError = computed(() => {
  const d = week.value?.douban
  if (!d?.enabled || d.ok) return ''
  return d.error || ''
})
/**
 * 国产剧这一栏只在「全部 / 剧集 / 首播 / 新剧」下露脸：
 * 切到「电影」时它不是电影，切到「我的订阅」时它没有 tmdb_id 也订不了，
 * 摆在那儿只会让人以为筛选失效了。
 */
const showDbTV = computed(
  () => dbTV.value.length > 0 && filter.value !== 'movies' && filter.value !== 'mine',
)
const dbMovieCount = computed(
  () =>
    allEntries.value.filter((e) => e.source === 'douban' && e.kind === 'movie').length,
)

/** TMDB 站内搜索：豆瓣条目在 TMDB 上没有 id 时，用片名带用户过去找一次。 */
function tmdbSearch(title: string) {
  return `https://www.themoviedb.org/search?query=${encodeURIComponent(title)}`
}

function dbLine(it: DoubanItem) {
  const parts: string[] = []
  if (it.episodes_info) parts.push(it.episodes_info)
  if (it.rating) parts.push(`★ ${it.rating.toFixed(1)}`)
  return parts.join(' · ')
}

/** 卡上那枚「来源」角标的文案：TVDB 补的时刻、豆瓣的国内条目要如实标出来。 */
function sourceLabel(e: CalEntry) {
  if (e.source === 'douban') return '豆瓣'
  if (e.source === 'tvdb') return 'TVDB'
  return 'TMDB'
}

function shiftWeek(days: number) {
  const base = week.value?.start || fmtDate(new Date())
  const d = new Date(`${base}T00:00:00`)
  d.setDate(d.getDate() + days)
  start.value = fmtDate(d)
  void load()
}

function goThisWeek() {
  start.value = ''
  void load()
}

function cacheKey() {
  // '' 代表「本周」，后端 ParseStart('') 会算本周一；缓存里统一记成 'cur'。
  return start.value || 'cur'
}

/**
 * 用「未合并豆瓣」的原始周历 base + 豆瓣响应 d，组出这一刻要显示的 week。
 *
 * 每次都返回新对象（days / items 也复制一份）：一是让 Vue 侦测到变化重新
 * 渲染，二是缓存里的 base 永远保持干净的 TMDB 主体，重复 render 不会越并越多。
 * movies 按 date 并进对应那天，并以 key 去重 —— 既避开与 TMDB 电影 / 旧缓存
 * 重复，也不破坏模板里 :key="e.key" 的唯一性。
 */
function render(base: CalWeek, d: DoubanPayload | null): CalWeek {
  const days: CalDay[] = base.days.map((day) => ({ ...day, items: [...day.items] }))
  if (d?.movies?.length) {
    for (const m of d.movies) {
      const day = days.find((x) => x.date === m.date)
      if (day && !day.items.some((e) => e.key === m.key)) {
        day.items.push(m)
      }
    }
  }
  // 占位（豆瓣还在异步加载）：只让这块有形状，别假装已经问过、也别报假错误。
  const douban: CalWeek['douban'] = d
    ? {
        enabled: d.enabled,
        ok: d.ok,
        tv: d.tv ?? [],
        ...(d.error ? { error: d.error } : {}),
      }
    : { enabled: true, ok: false, tv: [] }
  return { ...base, days, douban }
}

/**
 * 加载一周：主体周历先渲染，豆瓣另起请求异步补。
 *
 * 为什么拆开：豆瓣 4 个榜单串行 + 限流 + 最长 12s，同步等会把整张日历拖到
 * 十几秒。现在 /api/calendar 只回 TMDB 主体，拿到就立刻摆上桌；豆瓣由
 * /api/calendar/douban 单独拉，成功就把 movies 并进格子、tv 写进一栏，
 * 失败/超时只写这一栏的 error，页面绝不为它转圈。
 */
async function load(refresh = false) {
  const key = cacheKey()
  const seq = ++shared.seq
  errorMsg.value = ''

  // ---- 1) 主体周历：命中缓存就复用（切 tab 回来不重复请求），否则拉一次 ----
  let base = (refresh ? undefined : weekCache.get(key)) as CalWeek | undefined
  if (!base) {
    loading.value = true
    try {
      const r = await api.calendar.week(start.value || undefined, refresh)
      if (seq !== shared.seq) return // 期间翻周 / 切走，丢弃这次结果
      base = r as CalWeek
      weekCache.set(key, base) // 存的是「未合并豆瓣」的原始周历
      broken.value = {}
    } catch (e: any) {
      if (seq !== shared.seq) return
      errorMsg.value = e?.message || '播出日历加载失败'
      toast.warning('日历加载失败', errorMsg.value)
      loading.value = false
      dbLoading.value = false
      return
    }
    loading.value = false
  } else {
    broken.value = {}
  }
  // 到这里 base 一定非空：要么命中缓存，要么刚拉回来（失败已 return）。
  const pristine = base as CalWeek

  // 先把 TMDB 主体摆上去（豆瓣已缓存就顺带合并），页面立刻有内容，不等豆瓣。
  const cachedDb = (refresh ? undefined : doubanCache.get(key)) as DoubanPayload | undefined
  week.value = render(pristine, cachedDb ?? null)

  // ---- 2) 豆瓣：已缓存就收工，否则独立异步补；失败只影响这一栏 ----
  if (cachedDb) {
    dbLoading.value = false
    return
  }
  dbLoading.value = true
  try {
    const d = (await api.calendar.douban(start.value || undefined)) as DoubanPayload
    if (seq !== shared.seq) return // 过期响应别盖掉新一周的数据
    doubanCache.set(key, d)
    week.value = render(pristine, d)
  } catch (e: any) {
    if (seq !== shared.seq) return
    // 失败/超时：days 里 TMDB 数据一点不动，只把这一栏标成 ok=false + error。
    week.value = render(pristine, {
      enabled: true,
      ok: false,
      error: e?.message || '豆瓣数据加载失败或超时',
      tv: [],
      movies: [],
    })
  } finally {
    if (seq === shared.seq) dbLoading.value = false
  }
}

onMounted(() => {
  void load()
  void loadShareTarget()
  // 订阅索引：详情里的按钮要知道这部剧是不是已经订过（订过的那个按钮以前是 disabled 的，
  // 用户点了没反应，以为取消不了）。
  // 强制刷新：可能刚从「订阅」页签订完过来，那边改的是它自己的内存列表。
  void loadSubsIndex(true)
})

/**
 * 点片名看影视详情（不是日历详情）：这条只关心剧本身，不关心哪一天播。
 *
 * 豆瓣条目在 TMDB 上可能根本没有 id（国产剧、国内小片很常见），这时给片名，
 * 让详情抽屉自己按名字问 TMDB —— 也就是「在 TMDB 查一下」这个动作。
 */
function showDetail(e: { kind?: string; tmdb_id?: number; title: string } | null) {
  if (!e?.title) return
  openDetail({
    type: e.kind === 'movie' ? 'movie' : 'tv',
    tmdbId: e.tmdb_id || undefined,
    query: e.tmdb_id ? undefined : e.title,
    title: e.title,
  })
}

function markBroken(key: string) {
  broken.value[key] = true
}

function phStyle(e: { key: string; title: string }) {
  let h = 0
  for (const ch of e.key + e.title) h = (h * 31 + ch.charCodeAt(0)) % 360
  return {
    background: `linear-gradient(150deg, hsl(${h} 34% 32%), hsl(${(h + 48) % 360} 38% 20%))`,
  }
}

function dayNum(date: string) {
  return String(Number(date.slice(8, 10)))
}
function monthText(date: string) {
  return `${Number(date.slice(5, 7))}月`
}

function open(e: CalEntry) {
  sel.value = e
  searchOpen.value = false
  searchQ.value = e.title
  searchEn.value = e.original || ''
  res.reset()
}

function close() {
  sel.value = null
  searchOpen.value = false
  res.reset()
}

/** 结果里的第一条可用链接：磁力 / torrent 直链 / 原帖 / 帖子里的其它链接 */
function firstLink(it: ResItem) {
  return it.magnet || it.torrent || it.url || it.links?.[0] || ''
}

// 模板 @click 直接绑 copy，保留名字；剪贴板与提示统一走 copyText。
async function copy(text?: string) {
  if (!text) {
    toast.warning('这条没有可直接复制的链接')
    return
  }
  await copyText(text, '链接已复制')
}

/** 当前抽屉里这部剧是否已订阅：后端标注 + 本地索引，二者之一为真就算订过 */
const subscribedHere = computed(() => {
  const e = sel.value
  if (!e) return false
  return isSubscribed(e)
})

async function subscribeCurrent() {
  const e = sel.value
  if (!e || !e.tmdb_id || subscribing.value) return
  const type = e.kind === 'movie' ? 'movie' : 'tv'
  const existing = subIdOf(type, e.tmdb_id)
  subscribing.value = true
  try {
    // 已经订过就取消 —— 以前这里是 disabled 的死按钮，用户点了完全没反应
    if (existing) {
      await api.subscriptions.remove(existing)
      forgetSub(type, e.tmdb_id)
      e.subscribed = false
      toast.success('已取消订阅', e.title)
      return
    }
    const r: any = await api.subscriptions.create({
      tmdb_id: e.tmdb_id,
      media_type: type,
      title: e.title,
      original_title: e.original || '',
    })
    rememberSub(type, e.tmdb_id, Number(r?.id) || 0)
    toast.success('已加入订阅', e.title)
    e.subscribed = true
  } catch (err: any) {
    if (err?.status === 409) {
      toast.info('已经在订阅里了')
      e.subscribed = true
      void loadSubsIndex(true)
    } else {
      toast.warning('操作失败', err?.message || '稍后再试')
    }
  } finally {
    subscribing.value = false
    // 订阅行整份缓存也要刷新：抽屉里的起播线不能停在旧值。
    void loadSubsIndex(true)
  }
}

/**
 * 搜资源：第一次点展开结果区并按剧名搜一轮，之后点同一个按钮就是「重新搜索」
 * （refresh=1 穿透缓存）。来源 / 体积切换只在本地过滤，不重新联网。
 * 日历里点的是某一集，但资源按整部剧的名字找 —— 带 SxxExx 反而常搜不到，
 * 需要限定集数时可以在结果区的输入框里自己补。
 */
function runSearch(refresh = false) {
  const e = sel.value
  if (!e) return
  const q = searchQ.value.trim() || e.title
  // 改了关键词就别再带上这部剧的原名，否则后端会拿旧英文名去换名
  const en = q === e.title ? searchEn.value : ''
  searchOpen.value = true
  void res.run({ q, en, refresh })
}

function goSearch() {
  const first = !searchOpen.value
  runSearch(!first)
  if (first) {
    void nextTick(() => resultsRef.value?.scrollIntoView({ behavior: 'smooth', block: 'nearest' }))
  }
}
</script>

<template>
  <div class="col cal-page" style="gap: 14px">
    <header class="cal-head">
      <div>
        <h2 style="font-size: 16px">播出日历</h2>
        <p class="muted small">
          {{ rangeText }} · 共 {{ week?.total ?? 0 }} 条 ·
          <span v-if="week?.sources?.shows">扫了 {{ week.sources.shows }} 部在播剧</span>
          <span v-if="week?.cached" class="badge">缓存</span>
        </p>
      </div>
      <div class="row wrap" style="gap: 6px">
        <button class="btn btn-tertiary" :disabled="loading" @click="shiftWeek(-7)">
          <AppIcon name="chevronLeft" :size="14" />上一周
        </button>
        <button class="btn btn-tertiary" :disabled="loading" @click="goThisWeek">本周</button>
        <button class="btn btn-tertiary" :disabled="loading" @click="shiftWeek(7)">
          下一周<AppIcon name="chevronRight" :size="14" />
        </button>
        <button v-if="hiddenTotal > 0 || expanded" class="btn btn-tertiary" @click="expanded = !expanded">
          {{ expanded ? '收起' : `展开全部（+${hiddenTotal}）` }}
        </button>
        <button class="btn btn-tertiary" :disabled="loading" @click="load(true)">
          <AppIcon name="refresh" :size="14" />{{ loading ? '载入中…' : '重抓' }}
        </button>
      </div>
    </header>

    <div class="row wrap" style="gap: 6px">
      <span class="badge">TMDB · 剧单 / 分集 / 海报</span>
      <span
        class="badge"
        :class="{
          'badge-off':
            week?.sources?.source === 'off' || (!week?.sources?.tvmaze && !week?.sources?.tvdb),
        }"
      >
        {{
          week?.sources?.source === 'tvmaze'
            ? 'TVMaze · 时刻 / 电视网'
            : week?.sources?.source === 'tvdb'
              ? 'TVDB · 时刻 / 电视网'
              : '未启用时刻增强'
        }}
      </span>
      <span class="badge">按播出地日期</span>
      <span
        v-if="week?.douban"
        class="badge"
        :class="{ 'badge-off': !week.douban.enabled || !week.douban.ok }"
      >
        <template v-if="!week.douban.enabled">豆瓣 · 未启用</template>
        <template v-else>
          豆瓣 · 国内档期 / 国产剧<template v-if="dbMovieCount">
            · {{ dbMovieCount }} 部本周上映</template
          >
        </template>
      </span>
      <router-link v-if="week && week.douban && !week.douban.enabled" to="/settings" class="cal-link">
        开启豆瓣数据 →
      </router-link>
      <a
        v-if="week?.sources?.attribution_url"
        :href="week.sources.attribution_url"
        target="_blank"
        rel="noopener noreferrer"
        class="cal-link"
        >{{ week.sources.attribution }} ↗</a
      >
      <router-link
        v-if="
          week?.sources?.source === 'off' || (!week?.sources?.tvmaze && !week?.sources?.tvdb)
        "
        to="/settings"
        class="cal-link"
      >
        去系统配置切换数据源 →
      </router-link>
    </div>

    <div v-if="week?.notice" class="cal-notice">
      <AppIcon name="info" :size="15" />{{ week.notice }}
    </div>

    <div class="cal-layout">
      <aside class="cal-side">
        <div class="cal-side-title">筛选</div>
        <button
          v-for="f in filters"
          :key="f.value"
          class="cal-filter"
          :class="{ on: filter === f.value }"
          :title="f.desc"
          @click="filter = f.value"
        >
          <span>{{ f.label }}</span>
          <span class="cal-count">{{ countFor(f.value) }}</span>
        </button>
        <p class="muted small cal-side-note">
          切换筛选只在本地重画，不会重新联网。悬停按钮可看每个筛选的含义。
        </p>
      </aside>

      <div class="col" style="gap: 10px; min-width: 0">
        <div v-if="errorMsg" class="cal-error">
          <AppIcon name="alert" :size="15" />{{ errorMsg }}
          <button class="btn btn-tertiary" @click="load(true)">重试</button>
        </div>

        <div v-if="loading && !week" class="cal-grid">
          <div v-for="i in 7" :key="i" class="cal-day">
            <div class="cal-day-head"><span class="skeleton" style="width: 70px; height: 26px" /></div>
            <div class="skeleton cal-skeleton" />
          </div>
        </div>
        <div v-else-if="week" class="cal-grid">
          <section v-for="d in visibleDays" :key="d.date" class="cal-day">
            <div class="cal-day-head">
              <strong>{{ dayNum(d.date) }}</strong>
              <span>{{ monthText(d.date) }} {{ d.weekday }}</span>
              <span class="cal-day-count">{{ d.items.length }}</span>
            </div>
            <div v-if="!d.items.length" class="cal-none">—</div>
            <article
              v-for="e in d.items"
              :key="e.key"
              class="cal-card"
              :title="e.title"
              @click="open(e)"
            >
              <div class="poster-art">
                <img
                  v-if="e.poster && !broken[e.key]"
                  :src="e.poster"
                  alt=""
                  loading="lazy"
                  decoding="async"
                  @error="markBroken(e.key)"
                />
                <div v-else class="ph" :style="phStyle(e)">{{ e.title }}</div>
                <!-- 角标区分类型：new_show→新剧、kind=movie→电影，别让电影卡
                     没有类型标识而被和「新剧」混淆（Bug 2）。 -->
                <span v-if="isSubscribed(e)" class="cal-flag flag-sub">★ 已订阅</span>
                <span v-else-if="e.new_show" class="cal-flag flag-new">新剧</span>
                <span v-else-if="e.premiere" class="cal-flag flag-pre">首播</span>
                <span v-else-if="e.kind === 'movie'" class="cal-flag flag-movie">电影</span>
                <span v-if="e.rating" class="rate">★ {{ e.rating.toFixed(1) }}</span>
              </div>
              <div
                class="cal-show is-clickable"
                :title="
                  e.tmdb_id
                    ? `${e.title} · 点片名看详情`
                    : `${e.title} · 豆瓣条目，点片名去 TMDB 碰一次`
                "
                @click.stop="showDetail(e)"
              >
                {{ e.title }}
              </div>
              <div class="cal-line" :title="e.episode_title || e.title">
                <span class="cal-code">{{ e.code || '电影' }}</span>
                <span class="cal-ep">{{ e.episode_title || e.original || '—' }}</span>
              </div>
              <div class="cal-sub">
                <span v-if="e.source === 'douban'" class="cal-db-tag">豆瓣</span>
                <span>{{ e.air_time || e.date.slice(5) }}</span>
                <span v-if="e.network"> on {{ e.network }}</span>
              </div>
            </article>
            <div v-if="d.hidden" class="cal-more">另有 {{ d.hidden }} 条 · 顶部可展开</div>
          </section>
        </div>

        <p v-if="week && !visibleTotal" class="muted small">
          <template v-if="filter === 'new'">
            本周没有新开播的剧；电影在「电影」筛选里，全部剧集在「剧集」筛选里。
          </template>
          <template v-else>这个筛选下本周没有条目，换一个看看。</template>
        </p>

        <!-- 国产剧栏独立 loading：豆瓣异步补，转圈只在这一栏，不挡周历主体 -->
        <div
          v-if="dbLoading && !dbTV.length && filter !== 'movies' && filter !== 'mine'"
          class="cal-notice"
          style="margin-top: 4px"
        >
          <AppIcon name="info" :size="15" />正在加载国产剧 / 国内档期（豆瓣）…周历主体已就绪
        </div>
        <!-- 国产剧：豆瓣不按天给日期，只给「更新至第 N 集」，所以单独一栏 -->
        <div v-if="dbError" class="cal-notice" style="margin-top: 4px">
          <AppIcon name="alert" :size="15" />豆瓣没取到：{{ dbError }}（TMDB 那半边不受影响，
          可在「系统配置 → 豆瓣」检查开关与反代地址）
        </div>
        <section v-if="showDbTV" class="cal-db">
          <div class="row-between" style="align-items: baseline">
            <h3 class="cal-db-head">国产剧 · 更新进度</h3>
            <a :href="DOUBAN_URL" target="_blank" rel="noopener noreferrer" class="cal-link">
              数据来自豆瓣 ↗
            </a>
          </div>
          <p class="muted small" style="margin: 2px 0 8px">
            豆瓣只给「更新至第几集」，没有逐集播出日期，所以这些剧不按天排进上面的周历。
          </p>
          <div class="cal-db-grid">
            <article v-for="it in dbShown" :key="'db' + it.id" class="cal-db-card">
              <div class="cal-db-poster">
                <img
                  v-if="it.poster && !broken['db' + it.id]"
                  :src="it.poster"
                  alt=""
                  loading="lazy"
                  decoding="async"
                  @error="markBroken('db' + it.id)"
                />
                <div v-else class="ph" :style="phStyle({ key: 'db' + it.id, title: it.title })">
                  {{ it.title }}
                </div>
              </div>
              <div class="cal-db-body">
                <p class="cal-db-title" :title="it.card_subtitle || it.title">{{ it.title }}</p>
                <p class="muted small cal-db-line">{{ dbLine(it) || '暂无进度' }}</p>
                <div class="row wrap" style="gap: 8px">
                  <a
                    v-if="it.url"
                    :href="it.url"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="cal-link"
                    >豆瓣 ↗</a
                  >
                  <a
                    :href="tmdbSearch(it.title)"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="cal-link"
                    >TMDB 搜 ↗</a
                  >
                  <button class="chip cal-db-find" @click="emit('find', it.title)">
                    找资源
                  </button>
                </div>
              </div>
            </article>
          </div>
          <button
            v-if="dbTV.length > DB_MAX"
            class="btn btn-tertiary btn-sm"
            style="margin-top: 8px"
            @click="dbExpanded = !dbExpanded"
          >
            {{ dbExpanded ? '收起' : `展开全部 ${dbTV.length} 部` }}
          </button>
        </section>
      </div>
    </div>

    <Teleport to="body">
      <Transition name="fade">
        <div v-if="sel" class="cal-mask" @click="close()">
          <aside class="cal-drawer" :class="{ wide: searchOpen }" @click.stop>
            <div class="row" style="justify-content: space-between; align-items: flex-start">
              <div>
                <h2 style="margin: 0">
                  <TitleLink :type="sel.kind" :tmdb-id="sel.tmdb_id" :title="sel.title">
                    {{ sel.title }}
                  </TitleLink>
                </h2>
                <p v-if="sel.original" class="muted small" style="margin: 4px 0 0">
                  {{ sel.original }}
                </p>
              </div>
              <button class="btn btn-tertiary" @click="close()">
                <AppIcon name="close" :size="14" />
              </button>
            </div>

            <div class="cal-detail-art">
              <img v-if="sel.backdrop && !broken['detail' + sel.key]" :src="sel.backdrop" alt=""
                   @error="markBroken('detail' + sel.key)" />
              <img v-else-if="sel.poster" :src="sel.poster" alt="" />
              <div v-else class="ph" :style="phStyle(sel)">{{ sel.title }}</div>
            </div>

            <div class="row wrap" style="gap: 6px; margin-top: 12px">
              <span v-if="sel.code" class="badge">{{ sel.code }}</span>
              <span class="badge">{{ sel.date }}</span>
              <span v-if="sel.premiere" class="badge">季首播</span>
              <span v-if="sel.new_show" class="badge">新剧 / 新片</span>
              <span v-if="subscribedHere" class="badge">已订阅</span>
              <span v-if="sel.episodes_info" class="badge">{{ sel.episodes_info }}</span>
              <span class="badge">来源 {{ sourceLabel(sel) }}</span>
            </div>

            <h3 v-if="sel.episode_title" style="margin: 16px 0 6px">{{ sel.episode_title }}</h3>
            <div class="cal-facts">
              <div><span>播出</span><b>{{ sel.air_time || '未提供时刻' }}</b></div>
              <div><span>电视网</span><b>{{ sel.network || '—' }}</b></div>
              <div><span>时长</span><b>{{ sel.runtime ? sel.runtime + ' 分钟' : '—' }}</b></div>
              <div><span>评分</span><b>{{ sel.rating ? sel.rating.toFixed(1) : '—' }}</b></div>
            </div>

            <p
              v-if="sel.source === 'douban' && !sel.tmdb_id"
              class="muted small"
              style="margin-top: 10px; line-height: 1.6"
            >
              这条来自豆瓣，TMDB 上暂时没有对应条目：点上面的片名会按片名去 TMDB 查一次，
              查到之后才能订阅追更；认不出来就先用「搜索资源」按名字直接找片。
            </p>

            <div v-if="subHere" class="cal-sublines">
              <template v-if="sel.kind !== 'movie'">
                <div>
                  <span>起播线 · 只找它之后</span>
                  <b class="mono">
                    {{ waterText(subHere) }}
                    <template v-if="subHere.total_episodes"> / 共 {{ subHere.total_episodes }} 集</template>
                  </b>
                </div>
              </template>
              <div v-else><span>订阅状态</span><b>已订阅</b></div>
              <div><span>画质 / 状态</span><b>{{ subHere.quality }} · {{ subHere.state }}</b></div>
              <div v-if="subHere.last_result"><span>最近一轮</span><b class="small">{{ subHere.last_result }}</b></div>
              <!-- 播出日历 → 我的订阅 → 本集详情：起播线就在这里拖，
                   和影视详情抽屉、订阅设置用的是同一根滑杆 -->
              <div
                v-if="sel.kind !== 'movie'"
                class="cal-subline-slider"
                style="grid-column: 1 / -1"
              >
                <SubLineSlider
                  compact
                  :sub-id="subHere.id"
                  :season="subHere.watermark_season"
                  :episode="subHere.watermark_episode"
                  :total-episodes="subHere.total_episodes"
                  :title="sel.title"
                />
              </div>
              <p class="muted small" style="grid-column: 1 / -1; margin: 2px 0 0">
                拖到哪一集，就只找它之后的资源并订阅；提交成功后这条线会自己往前推。
              </p>
            </div>
            <p v-else class="muted small" style="margin-top: 10px">
              还没订阅。订了之后这里会显示起播线（只找它之后的资源）。
            </p>

            <p v-if="sel.overview" class="muted small cal-overview">{{ sel.overview }}</p>

            <div class="row wrap" style="gap: 8px; margin-top: 18px">
              <button
                class="btn"
                :class="subscribedHere ? 'btn-tertiary' : 'btn-primary'"
                :disabled="!sel.tmdb_id || subscribing"
                :title="
                  sel.tmdb_id
                    ? subscribedHere
                      ? '再点一下取消订阅'
                      : ''
                    : '豆瓣条目没有 TMDB id，先用片名在 TMDB 查一次再订阅'
                "
                @click="subscribeCurrent"
              >
                <AppIcon name="bookmark" :size="14" />
                {{ subscribing ? '处理中…' : subscribedHere ? '已订阅 · 点此取消' : '订阅追更' }}
              </button>
              <button class="btn btn-tertiary" :disabled="res.loading.value" @click="goSearch">
                <AppIcon name="search" :size="14" />
                {{ res.loading.value ? '搜索中…' : searchOpen ? '重新搜索' : '搜索资源（本页显示）' }}
              </button>
              <a v-if="sel.page_url" class="btn btn-tertiary" :href="sel.page_url" target="_blank"
                 rel="noopener">
                <AppIcon name="external" :size="14" />{{ sourceLabel(sel) }}
              </a>
              <!-- 豆瓣条目没有 TMDB id：给一条站内搜索，让用户自己去认一次 -->
              <a
                v-if="!sel.tmdb_id"
                class="btn btn-tertiary"
                :href="tmdbSearch(sel.title)"
                target="_blank"
                rel="noopener"
              >
                <AppIcon name="external" :size="14" />在 TMDB 查 ↗
              </a>
            </div>

            <!-- 搜索结果就地展开：不跳页、不丢掉当前这一周的上下文 -->
            <section v-if="searchOpen" ref="resultsRef" class="cal-res">
              <div class="row-between" style="margin-bottom: 8px">
                <h3 style="margin: 0; font-size: 14px">资源搜索</h3>
                <span class="muted small">
                  <template v-if="res.result.value">
                    共 {{ res.result.value.items.length }} 条 · {{ res.result.value.took_ms }}ms<template
                      v-if="res.result.value.cached"
                    >
                      · 缓存</template
                    >
                  </template>
                </span>
              </div>

              <div class="cal-res-bar">
                <input
                  v-model="searchQ"
                  class="input"
                  placeholder="片名关键词（默认整部剧，可自行补 SxxExx）"
                  @keydown.enter="runSearch(false)"
                />
                <button class="btn btn-sm btn-primary" :disabled="res.loading.value" @click="runSearch(false)">
                  搜索
                </button>
              </div>

              <div class="row wrap" style="gap: 6px; margin: 8px 0 6px">
                <button
                  v-for="k in KIND_FILTERS"
                  :key="k.v"
                  class="chip"
                  :class="{ 'is-active': res.kind.value === k.v }"
                  @click="res.kind.value = k.v"
                >
                  {{ k.l }}
                </button>
              </div>
              <div class="row wrap" style="gap: 6px; margin-bottom: 8px">
                <span class="small muted" style="margin-right: 2px">体积</span>
                <button
                  v-for="s in SIZE_FILTERS"
                  :key="s.v"
                  class="chip"
                  :class="{ 'is-active': res.minSize.value === s.v }"
                  @click="res.minSize.value = s.v"
                >
                  {{ s.l }}
                </button>
              </div>

              <p v-if="res.lookupNote.value" class="muted small" style="margin: 0 0 8px">
                {{ res.lookupNote.value }}
              </p>
              <p v-if="res.cachedNote.value" class="muted small" style="margin: 0 0 8px">
                {{ res.cachedNote.value }}
              </p>

              <div v-if="res.loading.value" class="col" style="gap: 8px">
                <div v-for="i in 3" :key="i" class="skeleton" style="height: 56px; border-radius: var(--r-sm)" />
              </div>
              <div v-else class="col" style="gap: 8px">
                <div v-for="r in res.items.value" :key="r.source_key + r.title" class="cal-res-row">
                  <div class="grow min-w0">
                    <p class="small cal-res-title">{{ r.title }}</p>
                    <div class="row wrap small muted" style="gap: 7px; margin-top: 3px">
                      <span class="badge" :class="r.kind === 'tg' ? 'badge-info' : ''">{{ r.source }}</span>
                      <span v-if="r.share_code" class="badge badge-success mono" :title="r.share_url">
                        115 · {{ r.share_code }}<template v-if="r.share_pwd"> · {{ r.share_pwd }}</template>
                      </span>
                      <span class="mono">{{ fmtSize(r.size) }}</span>
                      <span v-if="r.seeders">· {{ r.seeders }} 做种</span>
                      <span v-if="r.date">· {{ r.date }}</span>
                    </div>
                  </div>
                  <button
                    v-if="r.share_code"
                    class="btn btn-sm btn-primary"
                    :disabled="!!busy115"
                    :title="r.share_url"
                    @click="saveTo115(r)"
                  >
                    <AppIcon name="download" :size="14" />{{ isBusy115(r) ? '转存中…' : '存到 115' }}
                  </button>
                  <button
                    v-else-if="offlineURI(r)"
                    class="btn btn-sm btn-primary"
                    :disabled="!!busy115"
                    :title="offlineURI(r)"
                    @click="offlineTo115(r)"
                  >
                    <AppIcon name="download" :size="14" />{{ isBusy115(r) ? '提交中…' : '离线下载' }}
                  </button>
                  <a
                    v-else-if="firstLink(r)"
                    class="btn btn-sm btn-tertiary"
                    :href="firstLink(r)"
                    target="_blank"
                    rel="noreferrer"
                  >
                    <AppIcon name="external" :size="14" />打开
                  </a>
                  <button v-if="r.magnet" class="btn btn-sm btn-ghost" title="复制磁力" @click="copy(r.magnet)">
                    <AppIcon name="link" :size="14" />
                  </button>
                </div>

                <HEmpty
                  v-if="!res.items.value.length"
                  :title="res.error.value || '没搜到资源'"
                  desc="换个关键词、放开体积与来源筛选，或点「重新搜索」强制刷新"
                  icon="search"
                />
              </div>

              <router-link to="/media-transfer" class="cal-res-more">
                需要管理来源 / 改保存目录？打开完整资源搜索页 ↗
              </router-link>
            </section>
          </aside>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.cal-page {
  width: 100%;
}
.cal-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
  flex-wrap: wrap;
}
.cal-head h1 {
  margin: 0;
  font-size: 20px;
}
.cal-head p {
  margin: 4px 0 0;
}
.badge-off {
  opacity: 0.62;
}
.cal-link {
  font-size: 12px;
  color: var(--accent);
  margin-left: 4px;
}
.cal-notice,
.cal-error {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  font-size: 12.5px;
  color: var(--muted);
}
.cal-error {
  color: var(--danger);
}
.cal-error .btn {
  margin-left: auto;
}
.cal-layout {
  display: grid;
  grid-template-columns: 150px minmax(0, 1fr);
  gap: 14px;
  align-items: start;
}
.cal-side {
  display: flex;
  flex-direction: column;
  gap: 2px;
  position: sticky;
  top: 12px;
}
.cal-side-title {
  font-size: 12px;
  color: var(--muted);
  padding: 4px 8px 6px;
}
.cal-filter {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 8px 10px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--foreground);
  font-size: 13px;
  cursor: pointer;
  text-align: left;
}
.cal-filter:hover {
  background: var(--surface-secondary);
}
.cal-filter.on {
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 600;
}
.cal-count {
  font-size: 11px;
  color: var(--muted);
}
.cal-side-note {
  padding: 10px 8px;
  line-height: 1.5;
}
.cal-grid {
  display: grid;
  grid-template-columns: repeat(7, minmax(0, 1fr));
  gap: 8px;
  min-width: 0;
}
.cal-day {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.cal-day-head {
  display: flex;
  align-items: baseline;
  gap: 5px;
  padding: 6px 2px 8px;
  border-bottom: 1px solid var(--border);
  color: var(--muted);
  font-size: 11px;
  white-space: nowrap;
  overflow: hidden;
}
.cal-day-head strong {
  font-size: 20px;
  color: var(--foreground);
  line-height: 1;
}
.cal-day-count {
  margin-left: auto;
  font-size: 11px;
  color: var(--muted);
}
.cal-none {
  text-align: center;
  color: var(--muted);
  padding: 18px 0;
  font-size: 12px;
}
.cal-more {
  text-align: center;
  color: var(--muted);
  font-size: 11px;
  padding: 4px 0 8px;
  border-top: 1px dashed var(--border);
}
.cal-skeleton {
  height: 210px;
  border-radius: var(--r-md);
}
.cal-card {
  cursor: pointer;
  min-width: 0;
}
.cal-card .poster-art {
  aspect-ratio: 2 / 3;
  border-radius: 10px;
  border: 1px solid var(--border);
  overflow: hidden;
  background: var(--surface-secondary);
  position: relative;
}
.cal-card .poster-art img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.cal-card .ph {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 8px;
  text-align: center;
  color: #fff;
  font-size: 12px;
  line-height: 1.35;
}
.cal-flag {
  position: absolute;
  left: 6px;
  top: 6px;
  padding: 2px 6px;
  border-radius: 6px;
  font-size: 10px;
  color: #fff;
  background: rgb(10 8 6 / 0.72);
  max-width: calc(100% - 12px);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.flag-new {
  background: rgb(168 62 36 / 0.88);
}
.flag-pre {
  background: rgb(46 104 78 / 0.88);
}
/* 电影类型角标：和「新剧」用不同底色，两个筛选视图一眼能分清 */
.flag-movie {
  background: rgb(58 84 138 / 0.88);
}
.cal-card .rate {
  position: absolute;
  right: 6px;
  top: 6px;
  padding: 2px 5px;
  border-radius: 6px;
  font-size: 10px;
  color: #ffd7a8;
  background: rgb(10 8 6 / 0.72);
}
.cal-show,
.cal-line,
.cal-sub {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cal-show {
  font-size: 11.5px;
  color: var(--muted);
  margin-top: 5px;
}
.cal-show.is-clickable {
  cursor: pointer;
}
.cal-show.is-clickable:hover {
  color: var(--accent);
  text-decoration: underline;
  text-underline-offset: 2px;
}
.cal-line {
  font-size: 12px;
  margin-top: 1px;
}
.cal-code {
  font-weight: 700;
  color: var(--accent);
  margin-right: 4px;
}
.cal-ep {
  color: var(--foreground);
}
.cal-sub {
  font-size: 11px;
  color: var(--muted);
}
.cal-mask {
  position: fixed;
  inset: 0;
  z-index: var(--z-overlay);
  background: rgb(0 0 0 / 0.55);
  display: flex;
  justify-content: flex-end;
}
.cal-drawer {
  width: min(430px, 94vw);
  height: 100%;
  overflow-y: auto;
  padding: 20px;
  background: var(--surface);
  border-left: 1px solid var(--border);
  box-shadow: -18px 0 48px rgb(0 0 0 / 0.28);
  transition: width 0.18s ease;
}
/* 展开搜索结果后放宽一点，结果行里的标题和按钮才不会挤成一团 */
.cal-drawer.wide {
  width: min(620px, 96vw);
}
.min-w0 {
  min-width: 0;
}
.cal-res {
  margin-top: 20px;
  padding-top: 16px;
  border-top: 1px solid var(--separator);
  scroll-margin-top: 12px;
}
.cal-res-bar {
  display: flex;
  gap: 8px;
}
.cal-res-bar .input {
  flex: 1;
  min-width: 0;
}
.cal-res-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 12px;
  border-radius: var(--r-sm);
  border: 1px solid var(--border);
  background: var(--surface-secondary);
}
.cal-res-title {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  line-height: 1.5;
  margin: 0;
}
.cal-res-more {
  display: inline-block;
  margin-top: 14px;
  font-size: 12px;
  color: var(--muted);
  text-decoration: none;
}
.cal-res-more:hover {
  color: var(--accent);
}
.cal-detail-art {
  position: relative;
  margin-top: 14px;
  aspect-ratio: 16 / 9;
  border-radius: var(--r-md);
  overflow: hidden;
  background: var(--surface-secondary);
}
.cal-detail-art img,
.cal-detail-art .ph {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: grid;
  place-items: center;
  color: #fff;
}
.cal-facts {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px;
  margin-top: 12px;
}
.cal-facts > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface-secondary);
}
.cal-facts span {
  font-size: 11px;
  color: var(--muted);
}
.cal-facts b {
  font-size: 13px;
}
/* 订阅进度：和 cal-facts 同一套卡片观感，但整块略加边界以便区分 */
.cal-subline-slider {
  margin-top: 10px;
  padding-top: 10px;
  border-top: 1px dashed var(--separator);
}
.cal-sublines {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px;
  margin-top: 10px;
  padding: 10px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface);
}
.cal-sublines > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.cal-sublines span {
  font-size: 11px;
  color: var(--muted);
}
.cal-sublines b {
  font-size: 13px;
  overflow-wrap: anywhere;
}
.cal-overview {
  line-height: 1.65;
  margin-top: 12px;
}
/* ---------------- 国产剧一栏（豆瓣） ---------------- */
.cal-db-tag {
  display: inline-block;
  margin-right: 4px;
  padding: 0 4px;
  border-radius: 4px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 10px;
}
.cal-db {
  margin-top: 6px;
  padding: 12px 14px 14px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.cal-db-head {
  margin: 0;
  font-size: 13px;
}
.cal-db-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(230px, 1fr));
  gap: 10px;
}
.cal-db-card {
  display: flex;
  gap: 10px;
  min-width: 0;
  padding: 8px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface);
}
.cal-db-poster {
  position: relative;
  flex: 0 0 56px;
  width: 56px;
  aspect-ratio: 2 / 3;
  border-radius: 6px;
  overflow: hidden;
  background: var(--surface-secondary);
}
.cal-db-poster img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.cal-db-poster .ph {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 4px;
  text-align: center;
  color: #fff;
  font-size: 9px;
  line-height: 1.25;
  overflow: hidden;
}
.cal-db-body {
  min-width: 0;
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.cal-db-title,
.cal-db-line {
  margin: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cal-db-title {
  font-size: 13px;
  font-weight: 600;
}
.cal-db-line {
  font-size: 11.5px;
}
.cal-db-find {
  padding: 1px 8px;
  font-size: 11px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-secondary);
  color: var(--foreground);
  cursor: pointer;
}
.cal-db-find:hover {
  border-color: var(--accent);
  color: var(--accent);
}
@media (max-width: 1100px) {
  .cal-layout {
    grid-template-columns: minmax(0, 1fr);
  }
  .cal-side {
    position: static;
    flex-direction: row;
    flex-wrap: wrap;
    gap: 4px;
  }
  .cal-side-title,
  .cal-side-note {
    display: none;
  }
  .cal-filter {
    width: auto;
  }
}
@media (max-width: 900px) {
  .cal-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
</style>
