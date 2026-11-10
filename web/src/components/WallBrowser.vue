<script setup lang="ts">
/**
 * TMDB 电影墙（本项目自有模块，不是占位数据）。
 *
 * 数据来自 GET /api/wall：按「电影 / 剧集 + 平台 + 类型 + 排序」翻页，
 * 图片走 image.tmdb.org（TMDB 返回的就是完整地址）。点卡片开抽屉，
 * 抽屉里直接搜资源（/api/res/search，中文名后端会用 TMDB 换英文名，TG 频道也一起搜）。
 */
import { computed, nextTick, onMounted, ref } from 'vue'

/**
 * 聚焦模式：从「每平台一行」的某行点「查看全部」进来，只列这一个平台，
 * 但这套筛选 + 分页网格仍然可用（切类型、换平台、排序、翻页都留着）。
 */
const props = withDefaults(
  defineProps<{
    initialProvider?: string
    initialType?: 'movie' | 'series'
    showBack?: boolean
  }>(),
  { initialProvider: '', initialType: 'movie', showBack: false },
)
const emit = defineEmits<{ back: [] }>()
import AppIcon from '@/components/AppIcon.vue'
import HPanel from '@/components/HPanel.vue'
import HEmpty from '@/components/HEmpty.vue'
import TitleLink from '@/components/TitleLink.vue'
import TitlePicker from '@/components/TitlePicker.vue'
import type { TitleHit } from '@/components/TitlePicker.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { forgetSub, loadSubsIndex, rememberSub, subIdOf } from '@/stores/subscriptions'
import { fmtSize, KIND_FILTERS, SIZE_FILTERS, useResSearch } from '@/composables/useResSearch'
import { use115Share } from '@/composables/use115Share'
import { copyText } from '@/utils/format'

interface WallItem {
  imdb_id?: string
  name: string
  original?: string
  type: string
  year?: string
  rating?: string
  genres?: string[]
  description?: string
  poster?: string
  background?: string
  link?: string
  tmdb_id: number
  in_library: boolean
}

const items = ref<WallItem[]>([])
const loading = ref(false)
const err = ref('')
const page = ref(1)
const hasMore = ref(false)
const tookMS = ref(0)

const type = ref<'movie' | 'series'>(props.initialType)
const provider = ref(props.initialProvider)
const genre = ref('')
const sort = ref('popular')

interface ProviderOption {
  code: string
  name: string
  group?: string
  note?: string
}
const providerOptions = ref<ProviderOption[]>([])
const overseasProviders = computed(() => providerOptions.value.filter((p) => p.group !== 'cn'))
const cnProviders = computed(() => providerOptions.value.filter((p) => p.group === 'cn'))
/** 选中的国内平台若是「按国产内容兜底」，把这句话摆在筛选条上，别让人误以为是平台真实片单 */
const activeProviderNote = computed(() =>
  providerOptions.value.find((p) => p.code === provider.value)?.note || '',
)
const wallCached = ref(false)
const genreOptions = ref<[string, string][]>([])

const sorts = [
  { v: 'popular', l: '现在最热' },
  { v: 'top', l: '高分优先' },
  { v: 'new', l: '新片上线' },
]

/**
 * 拉一页榜单。
 *
 * refresh=true 才让后端穿透缓存去重抓 TMDB —— 只有「刷新」按钮会传。
 * 进页面、切平台、切类型、翻页一律读缓存，不会反复打 TMDB 配额。
 */
async function load(reset = false, refresh = false) {
  if (loading.value) return
  loading.value = true
  err.value = ''
  const next = reset ? 1 : page.value + 1
  try {
    const params: Record<string, string | number> = {
      type: type.value,
      providers: provider.value,
      genre: genre.value,
      sort: sort.value,
      page: next,
      limit: 30,
    }
    if (refresh) params.refresh = 1
    const r = await api.local.wall(params)
    providerOptions.value = r.provider_options ?? providerOptions.value
    genreOptions.value = r.genre_options ?? []
    items.value = reset ? (r.items ?? []) : [...items.value, ...(r.items ?? [])]
    page.value = r.page ?? next
    hasMore.value = !!r.has_more
    wallCached.value = !!r.cached
    tookMS.value = Date.now()
  } catch (e: any) {
    err.value = e?.message || '电影墙拉取失败'
    if (!items.value.length) toast.warning('电影墙没拉到数据', err.value)
  } finally {
    loading.value = false
  }
}

function switchType(v: 'movie' | 'series') {
  if (type.value === v) return
  type.value = v
  genre.value = ''
  // 聚焦模式下切「电影 / 剧集」要留在同一个平台，别把行筛选清空。
  if (!props.initialProvider) provider.value = ''
  if (v === 'movie') sort.value = sort.value === 'new' ? 'new' : sort.value
  load(true)
}

function pickProvider(code: string) {
  provider.value = provider.value === code ? '' : code
  load(true)
}

function pickGenre(code: string) {
  genre.value = genre.value === code ? '' : code
  load(true)
}

function pickSort(v: string) {
  sort.value = v
  load(true)
}

const broken = ref<Record<number, boolean>>({})
function markBroken(id: number) {
  broken.value = { ...broken.value, [id]: true }
}
function hueOf(s: string) {
  let h = 0
  for (const ch of s) h = (h * 31 + ch.charCodeAt(0)) % 360
  return h
}
function phStyle(it: WallItem) {
  const h = hueOf(it.name || 'x')
  return { background: `linear-gradient(160deg, hsl(${h} 52% 40%), hsl(${h + 24} 48% 20%))` }
}

/* ---------------- 详情抽屉 ---------------- */
const sel = ref<WallItem | null>(null)
const res = useResSearch()
// 抽屉里搜到的 115 分享直接存，转存目录用设置里那份（资源搜索页可改）
const {
  busyKey: busy115,
  loadTarget: loadShareTarget,
  isBusy: isBusy115,
  save: saveTo115,
  offline: offlineTo115,
  offlineURI,
} = use115Share()
function open(it: WallItem) {
  sel.value = it
  picked.value = null
  pickerOpen.value = false
  res.reset()
  // 打开就按片名搜一轮：中文名后端会自动换英文名。
  // 不强制刷新 —— 刚在别处搜过同一部片时直接吃缓存，秒出结果。
  res.run({ q: it.name, en: it.original })
}

function close() {
  sel.value = null
}

/** 用户点「重新搜索」才刷新；上面的来源切换是本地过滤，不重打网络 */
function searchRes() {
  if (!sel.value) return
  res.run({
    q: picked.value?.title || sel.value.name,
    en: picked.value?.original || sel.value.original,
    refresh: true,
  })
}

/* ---------------- 先认片、再搜资源（与资源搜索页同一套候选） ---------------- */
const pickerOpen = ref(false)
const pickerRef = ref<InstanceType<typeof TitlePicker> | null>(null)
const picked = ref<TitleHit | null>(null)

async function openPicker() {
  pickerOpen.value = true
  await nextTick()
  // setQuery 会预填关键词并立刻查 TMDB 候选，是唯一的触发入口
  pickerRef.value?.setQuery(sel.value?.name || '')
}

/** 选中后同一轮把 TG 频道和种子站都搜一遍：中文名给中文源、原名给英文源 */
function onPick(hit: TitleHit) {
  picked.value = hit
  pickerOpen.value = false
  // 豆瓣候选的 sub_title 是演员表，不是英文原名；留空让后端问豆瓣，
  // 国产片就会直接按中文名搜，不用再等 TMDB/IMDb 换名。
  const en = hit.source === 'tmdb' ? hit.original : ''
  res.run({ q: hit.title, en, refresh: true })
  toast.info(
    `已锁定《${hit.title}》`,
    en ? 'TG 频道与种子站按中文名 + 原名一起搜' : '按中文名搜 TG 频道与种子站',
  )
}

function onSkip(q: string) {
  picked.value = null
  pickerOpen.value = false
  res.run({ q, refresh: true })
}

// 「订阅」= 让后端每轮帮我问这部剧该出到第几集，有新的就自动下。
// 这里只传身份信息（tmdb_id / 类型 / 标题），画质用后端默认的 1080p，
// 建完可以在「订阅」页签里逐条改 —— 墙上点一下不该先弹一个表单。
const subscribing = ref(false)

/** 详情里这部片已经订过的话，拿到它的订阅 id；0 = 没订 */
const subscribedID = computed(() =>
  sel.value ? subIdOf(sel.value.type === 'movie' ? 'movie' : 'tv', sel.value.tmdb_id) : 0,
)

/**
 * 一个按钮两种行为：没订就订，订了就取消。
 *
 * 以前这里永远只发 POST，订过的再点只会吃到 409「已经在订阅里了」，
 * 想反悔得绕到「订阅」页签里找垃圾桶 —— 这就是「点了取消不掉」。
 */
async function subscribe(it: WallItem | null) {
  if (!it || subscribing.value) return
  const type = it.type === 'movie' ? 'movie' : 'tv'
  subscribing.value = true
  try {
    if (subscribedID.value) {
      await api.subscriptions.remove(subscribedID.value)
      forgetSub(type, it.tmdb_id)
      toast.success(`已取消订阅「${it.name}」`, '已经交出去的离线任务不受影响')
      return
    }
    const r: any = await api.subscriptions.create({
      tmdb_id: it.tmdb_id,
      media_type: type,
      title: it.name,
      original_title: it.original,
      year: Number(it.year) || 0,
      imdb_id: it.imdb_id,
    })
    rememberSub(type, it.tmdb_id, Number(r?.id) || 0)
    toast.success(`已订阅「${it.name}」`, '出新集会自动提交离线，画质默认 1080p')
  } catch (e: any) {
    // 重复订阅是 409：这不是错误，用户只是忘了自己订过（可能是在别的页面订的）。
    if (e?.status === 409) {
      toast.info('已经在订阅里了')
      void loadSubsIndex(true)
    } else {
      toast.warning('操作失败', e?.message || '')
    }
  } finally {
    subscribing.value = false
  }
}

// 模板 @click 直接绑 copy，保留名字；剪贴板与提示统一走 copyText。
async function copy(text?: string) {
  if (!text) {
    toast.warning('这条没有可直接复制的链接')
    return
  }
  await copyText(text, '链接已复制')
}

function firstLink(it: { magnet?: string; torrent?: string; url?: string; links?: string[] }) {
  return it.magnet || it.torrent || it.url || it.links?.[0] || ''
}

const imdbURL = computed(() =>
  sel.value?.imdb_id ? `https://www.imdb.com/title/${sel.value.imdb_id}/` : '',
)

onMounted(() => {
  void load(true)
  void loadShareTarget()
  // 先建索引，详情里的按钮才知道该显示「订阅」还是「已订阅 · 点此取消」
  void loadSubsIndex()
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <!-- 筛选条 -->
    <div class="panel wall-bar">
      <div class="row wrap" style="gap: 10px">
        <button v-if="showBack" class="btn btn-sm btn-tertiary" @click="emit('back')">
          <AppIcon name="chevronLeft" :size="14" />返回全部平台
        </button>
        <div class="segmented">
          <button :class="{ 'is-active': type === 'movie' }" @click="switchType('movie')">电影</button>
          <button :class="{ 'is-active': type === 'series' }" @click="switchType('series')">剧集</button>
        </div>

        <div class="col" style="gap: 6px">
          <div class="row wrap" style="gap: 6px">
            <span class="muted small wall-group">海外</span>
            <button class="chip" :class="{ 'is-active': !provider }" @click="pickProvider('')">全部平台</button>
            <button
              v-for="p in overseasProviders"
              :key="p.code"
              class="chip"
              :class="{ 'is-active': provider === p.code }"
              @click="pickProvider(p.code)"
            >
              {{ p.name }}
            </button>
          </div>
          <div class="row wrap" style="gap: 6px">
            <span class="muted small wall-group">国内</span>
            <button
              v-for="p in cnProviders"
              :key="p.code"
              class="chip"
              :class="{ 'is-active': provider === p.code }"
              :title="p.note || p.name"
              @click="pickProvider(p.code)"
            >
              {{ p.name }}
            </button>
            <span v-if="activeProviderNote" class="muted small">{{ activeProviderNote }}</span>
          </div>
        </div>

        <div class="grow" />

        <select class="select-native" style="width: 132px" :value="sort" @change="pickSort(($event.target as HTMLSelectElement).value)">
          <option v-for="s in sorts" :key="s.v" :value="s.v">{{ s.l }}</option>
        </select>
        <button class="btn btn-sm btn-tertiary" :disabled="loading" title="重新抓 TMDB（平时只读缓存）" @click="load(true, true)">
          <AppIcon name="refresh" :size="14" />{{ loading ? '加载中…' : '刷新重抓' }}
        </button>
      </div>

      <div v-if="genreOptions.length" class="row wrap" style="gap: 6px">
        <button class="chip" :class="{ 'is-active': !genre }" @click="pickGenre('')">全部类型</button>
        <button
          v-for="g in genreOptions"
          :key="g[0]"
          class="chip"
          :class="{ 'is-active': genre === g[0] }"
          @click="pickGenre(g[0])"
        >
          {{ g[1] }}
        </button>
      </div>
    </div>

    <!-- 海报墙 -->
    <HPanel title="榜单" :desc="`${items.length} 部 · 数据源 TMDB · 点击看简介并直接搜资源`">
      <template #actions>
        <span v-if="wallCached" class="badge" title="点右上「刷新重抓」才会重新请求 TMDB">缓存榜单</span>
        <span v-else class="badge badge-accent">TMDB 实时</span>
      </template>

      <div v-if="items.length" class="poster-grid">
        <button v-for="it in items" :key="`${it.tmdb_id}-${it.name}`" class="poster" @click="open(it)">
          <div class="poster-art">
            <img
              v-if="it.poster && !broken[it.tmdb_id]"
              :src="it.poster"
              alt=""
              loading="lazy"
              decoding="async"
              @error="markBroken(it.tmdb_id)"
            />
            <div v-else class="ph" :style="phStyle(it)">{{ it.name }}</div>
            <span v-if="it.rating" class="rate">{{ it.rating }}</span>
            <span v-if="it.in_library" class="inlib">已入库</span>
          </div>
          <span class="poster-title">
            <TitleLink :type="it.type" :tmdb-id="it.tmdb_id" :title="it.name" :year="it.year">{{ it.name }}</TitleLink>
          </span>
          <span class="poster-sub">{{ it.year || '—' }} · {{ it.type === 'tv' ? '剧集' : '电影' }}</span>
        </button>
      </div>

      <div v-else-if="loading" class="poster-grid">
        <div v-for="i in 12" :key="i" class="poster">
          <div class="skeleton" style="aspect-ratio: 2 / 3; border-radius: var(--r-md)" />
          <div class="skeleton" style="height: 12px; width: 80%" />
          <div class="skeleton" style="height: 10px; width: 50%" />
        </div>
      </div>

      <HEmpty v-else title="这组筛选下没有片目" :desc="err || '换个平台或类型试试'" icon="search" />

      <template #footer>
        <div class="row-between">
          <span class="muted small">第 {{ page }} 页 · 每页 30 部</span>
          <button v-if="hasMore" class="btn btn-sm btn-tertiary" :disabled="loading" @click="load(false)">
            {{ loading ? '加载中…' : '加载更多' }}
          </button>
          <span v-else class="muted small">已经到底了</span>
        </div>
      </template>
    </HPanel>

    <!-- 详情抽屉 -->
    <Teleport to="body">
      <Transition name="fade">
        <div v-if="sel" class="overlay drawer-overlay" @click.self="close">
          <aside class="drawer">
            <header class="drawer-head" :style="sel.background ? { backgroundImage: `linear-gradient(180deg, rgba(0,0,0,.25), var(--overlay) 88%), url(${sel.background})` } : undefined">
              <div class="row" style="gap: 14px; align-items: flex-start">
                <div class="d-art">
                  <img v-if="sel.poster && !broken[sel.tmdb_id]" :src="sel.poster" alt="" />
                  <div v-else class="ph" :style="phStyle(sel)">{{ sel.name }}</div>
                </div>
                <div class="col" style="gap: 6px; min-width: 0">
                  <h2 class="d-title">
                    <TitleLink :type="sel.type" :tmdb-id="sel.tmdb_id" :title="sel.name" :year="sel.year">{{ sel.name }}</TitleLink>
                  </h2>
                  <p v-if="sel.original && sel.original !== sel.name" class="muted small truncate">
                    {{ sel.original }}
                  </p>
                  <div class="row wrap" style="gap: 6px">
                    <span class="badge badge-accent">{{ sel.type === 'tv' ? '剧集' : '电影' }}</span>
                    <span v-if="sel.year" class="badge">{{ sel.year }}</span>
                    <span v-if="sel.rating" class="badge badge-info">★ {{ sel.rating }}</span>
                    <span v-if="sel.in_library" class="badge badge-success">已入库</span>
                    <span v-for="g in sel.genres || []" :key="g" class="badge">{{ g }}</span>
                  </div>
                </div>
              </div>
              <button class="btn btn-icon" aria-label="关闭" @click="close">
                <AppIcon name="close" />
              </button>
            </header>

            <div class="drawer-body">
              <p v-if="sel.description" class="d-desc">{{ sel.description }}</p>

              <div class="row wrap" style="gap: 8px; margin-bottom: 14px">
                <a v-if="imdbURL" class="btn btn-sm btn-tertiary" :href="imdbURL" target="_blank" rel="noreferrer">
                  <AppIcon name="external" :size="14" />IMDb
                </a>
                <a v-if="sel.link" class="btn btn-sm btn-tertiary" :href="sel.link" target="_blank" rel="noreferrer">
                  <AppIcon name="link" :size="14" />TMDB
                </a>
                <button class="btn btn-sm btn-primary" :disabled="res.loading.value" @click="searchRes">
                  <AppIcon name="search" :size="14" />{{ res.loading.value ? '搜索中…' : '搜资源' }}
                </button>
                <button
                  class="btn btn-sm btn-tertiary"
                  :style="subscribedID ? 'color: var(--danger)' : ''"
                  :disabled="subscribing"
                  :title="subscribedID ? '再点一下取消订阅' : '出新集自动提交离线'"
                  @click="subscribe(sel)"
                >
                  <AppIcon name="bookmark" :size="14" />{{
                    subscribing ? '处理中…' : subscribedID ? '已订阅 · 点此取消' : '订阅'
                  }}
                </button>
              </div>

              <div class="row-between" style="margin: 16px 0 8px">
                <div class="row" style="gap: 8px">
                  <p class="section-title" style="margin: 0">资源搜索</p>
                  <button class="btn btn-sm btn-tertiary" @click="openPicker">
                    <AppIcon name="search" :size="13" />认片 / 换一部
                  </button>
                </div>
                <span class="muted small">
                  <template v-if="res.result.value">
                    共 {{ res.result.value.items.length }} 条 · {{ res.result.value.took_ms }}ms<template v-if="res.result.value.cached"> · 缓存</template>
                  </template>
                </span>
              </div>

              <div v-if="picked" class="row wrap" style="gap: 6px; margin-bottom: 8px">
                <span class="badge badge-accent">已锁定 {{ picked.title }}</span>
                <span v-if="picked.original" class="muted small mono">{{ picked.original }}</span>
              </div>
              <div v-if="pickerOpen" style="margin-bottom: 12px">
                <TitlePicker ref="pickerRef" compact @pick="onPick" @skip="onSkip" />
              </div>

              <div class="row wrap" style="gap: 6px; margin-bottom: 6px">
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

              <p v-if="res.lookupNote.value" class="muted small" style="margin-bottom: 8px">
                {{ res.lookupNote.value }}
              </p>

              <div v-if="res.loading.value" class="col" style="gap: 8px">
                <div v-for="i in 3" :key="i" class="skeleton" style="height: 56px; border-radius: var(--r-sm)" />
              </div>

              <div v-else class="col" style="gap: 8px">
                <div v-for="r in res.pagedItems.value" :key="r.source_key + r.title" class="res">
                  <div class="grow">
                    <p class="small res-title">{{ r.title }}</p>
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
                  <button
                    v-if="r.magnet"
                    class="btn btn-sm btn-ghost"
                    title="复制磁力"
                    @click="copy(r.magnet)"
                  >
                    <AppIcon name="link" :size="14" />
                  </button>
                </div>

                <div v-if="res.totalPages.value > 1" class="pager">
                  <button class="btn btn-sm btn-tertiary" :disabled="res.page.value <= 1" @click="res.prevPage()">
                    上一页
                  </button>
                  <span class="small muted">第 {{ Math.min(res.page.value, res.totalPages.value) }} / {{ res.totalPages.value }} 页</span>
                  <button
                    class="btn btn-sm btn-primary"
                    :disabled="res.page.value >= res.totalPages.value"
                    @click="res.nextPage()"
                  >
                    下一页
                  </button>
                </div>

                <HEmpty
                  v-if="!res.items.value.length"
                  :title="res.error.value || '没搜到资源'"
                  desc="换关键词、放开体积筛选，或到「发现与订阅 → 资源搜索」里开更多来源"
                  icon="search"
                />
              </div>
            </div>
          </aside>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.wall-bar {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px 16px;
}
/* 平台分两组，组名固定宽度，chip 换行时仍对齐 */
.wall-group {
  width: 30px;
  flex: none;
  line-height: 28px;
}
.poster-art img {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.poster-art .inlib {
  position: absolute;
  left: 7px;
  bottom: 7px;
  padding: 1px 7px;
  border-radius: var(--r-full);
  background: color-mix(in oklab, var(--success) 88%, black);
  color: #fff;
  font-size: 10.5px;
  font-weight: 600;
}

.drawer-overlay {
  justify-items: end;
  padding: 0;
}
.drawer-head {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 18px 20px 14px;
  border-bottom: 1px solid var(--separator);
  background-size: cover;
  background-position: center 18%;
}
.d-title {
  font-size: 18px;
  line-height: 1.3;
}
.d-art {
  position: relative;
  flex: none;
  width: 88px;
  aspect-ratio: 2 / 3;
  border-radius: var(--r-md);
  overflow: hidden;
  border: 1px solid var(--border);
  background: var(--surface-secondary);
}
.d-art img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.d-art .ph {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 8px;
  color: #fff;
  font-size: 11.5px;
  font-weight: 600;
  text-align: center;
  line-height: 1.35;
}
.d-desc {
  font-size: 13px;
  line-height: 1.8;
  color: var(--muted);
  margin-bottom: 14px;
  max-height: 5.4em;
  overflow: auto;
}
.pager {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding-top: 6px;
}
.res {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  border-radius: var(--r-sm);
  border: 1px solid var(--border);
  background: var(--surface);
}
.res-title {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  line-height: 1.5;
}
</style>
