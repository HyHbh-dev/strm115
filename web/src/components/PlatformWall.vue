<script setup lang="ts">
/**
 * 发现与订阅的主视图：每个平台一行，行内横向海报墙。
 *
 * 和旧的 WallBrowser（筛选 + 分页网格）不同，这里刻意只留一个选择：
 * 「电影 / 剧集」。平台、类型、排序这些筛选交给某行的「查看全部」，
 * 用户打开页面不需要先做四五次选择才能看到片子。
 *
 * 平台清单走 /api/wall/providers（静态、不打 TMDB），所以所有行标题立刻
 * 就能画出来；每行的片单用 IntersectionObserver 滚到附近才拉，避免一进
 * 页面同时发十几个 TMDB 请求。切电影/剧集会把所有行标脏并重新加载可见行。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import { api } from '@/api/endpoints'
import { rememberOrigin } from '@/stores/navMemory'

interface WallItem {
  tmdb_id: number
  name: string
  original?: string
  type: string
  year?: string
  rating?: string
  poster?: string
  in_library?: boolean
}
interface Provider {
  code: string
  name: string
}
interface RowState {
  items: WallItem[]
  loading: boolean
  loaded: boolean
  err: string
}

const emit = defineEmits<{
  focus: [v: { code: string; name: string; type: 'movie' | 'series' }]
}>()

const router = useRouter()
const route = useRoute()
const type = ref<'movie' | 'series'>('movie')
const typeLabel = computed(() => (type.value === 'movie' ? '电影' : '剧集'))
const providers = ref<Provider[]>([])
const rows = ref<Record<string, RowState>>({})
const providersLoading = ref(false)
const providersErr = ref('')
const broken = ref<Record<string, boolean>>({})

function blankRow(): RowState {
  return { items: [], loading: false, loaded: false, err: '' }
}

async function loadProviders() {
  providersLoading.value = true
  try {
    const r: any = await api.local.wallProviders()
    const list: Provider[] = Array.isArray(r?.provider_options) ? r.provider_options : []
    providers.value = list
    const next: Record<string, RowState> = {}
    for (const p of list) next[p.code] = blankRow()
    rows.value = next
  } catch (e: any) {
    // 拿不到清单就整面墙没内容，给一句能看懂的话，不静默空白。
    rows.value = {}
    providersErr.value = e?.message || '平台清单没拉到'
  } finally {
    providersLoading.value = false
  }
}

async function loadRow(code: string, refresh = false) {
  const row = rows.value[code]
  if (!row || row.loading || (row.loaded && !refresh)) return
  row.loading = true
  row.err = ''
  try {
    const params: Record<string, string | number> = {
      type: type.value,
      providers: code,
      sort: 'popular',
      page: 1,
      limit: 12,
    }
    if (refresh) params.refresh = 1
    const r: any = await api.local.wall(params)
    row.items = Array.isArray(r?.items) ? r.items : []
    row.loaded = true
  } catch (e: any) {
    row.err = e?.message || '这一行没拉到片单'
  } finally {
    row.loading = false
  }
}

function switchType(v: 'movie' | 'series') {
  if (type.value === v) return
  type.value = v
  broken.value = {}
  for (const code of Object.keys(rows.value)) rows.value[code] = blankRow()
  // IntersectionObserver 对「已经在视口里、只是内容变了」的元素不会再回调，
  // 所以切类型后手动把当前可见的行重新拉一遍。
  void nextTick(() => {
    for (const [code, el] of rowEls) {
      const rect = el.getBoundingClientRect()
      if (el.isConnected && rect.top < window.innerHeight + 240 && rect.bottom > -240) {
        void loadRow(code)
      }
    }
  })
}

function refreshVisible() {
  for (const [code, row] of Object.entries(rows.value)) {
    if (row.loaded) void loadRow(code, true)
  }
}

function markBro(key: string) {
  broken.value = { ...broken.value, [key]: true }
}

function artKey(it: WallItem) {
  return `${it.type}:${it.tmdb_id}`
}

/** 点卡片进影视详情页，不再一点就跳去资源搜索。 */
function openTitle(it: WallItem) {
  // 详情页的返回按钮据此回到这张墙（同一个平台、同一个类型、同一段滚动位置）。
  rememberOrigin(route.fullPath, '返回海报墙')
  router.push({
    name: 'title',
    query: {
      type: it.type === 'tv' ? 'tv' : 'movie',
      tmdb_id: String(it.tmdb_id || ''),
      q: it.name,
      year: it.year || '',
    },
  })
}

function scrollRow(code: string, dir: -1 | 1) {
  const track = document.querySelector<HTMLElement>(`.pw-track[data-code="${code}"]`)
  track?.scrollBy({ left: dir * Math.max(360, track.clientWidth * 0.8), behavior: 'smooth' })
}

/* ---------------- 按视口懒加载每一行 ---------------- */
const rowEls = new Map<string, HTMLElement>()
let io: IntersectionObserver | null = null

function setRowRef(code: string, el: any) {
  const node = el?.$el ?? el
  if (!(node instanceof HTMLElement)) return
  rowEls.set(code, node)
  io?.observe(node)
}

watch(
  providers,
  () => {
    void nextTick(() => {
      for (const el of rowEls.values()) if (el.isConnected) io?.observe(el)
    })
  },
  { flush: 'post' },
)

onMounted(() => {
  io = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue
        const code = (e.target as HTMLElement).dataset.code
        if (code) void loadRow(code)
      }
    },
    { rootMargin: '240px 0px' },
  )
  void loadProviders()
})

onBeforeUnmount(() => {
  io?.disconnect()
  io = null
  rowEls.clear()
})
</script>

<template>
  <div class="pw">
    <div class="panel pw-bar">
      <div class="row wrap" style="gap: 10px">
        <div class="segmented">
          <button :class="{ 'is-active': type === 'movie' }" @click="switchType('movie')">电影</button>
          <button :class="{ 'is-active': type === 'series' }" @click="switchType('series')">剧集</button>
        </div>
        <span class="muted small">按平台分行的 {{ typeLabel }}片单 · 点海报看详情</span>
        <div class="grow" />
        <button class="btn btn-sm btn-tertiary" title="重新抓 TMDB（平时只读缓存）" @click="refreshVisible">
          <AppIcon name="refresh" :size="14" />刷新已展开的行
        </button>
      </div>
    </div>

    <div v-if="providersLoading" class="muted small pad">正在读取平台清单…</div>
    <div v-else-if="providersErr" class="err small pad">{{ providersErr }}</div>

    <div class="pw-group">
      <section
        v-for="p in providers"
        :key="p.code"
        :ref="(el) => setRowRef(p.code, el)"
        :data-code="p.code"
        class="pw-row panel"
      >
        <header class="row-between pw-head">
          <div class="row" style="gap: 8px; min-width: 0">
            <h4 class="pw-title">{{ p.name }} <span class="muted">- {{ typeLabel }}</span></h4>
          </div>
          <div class="row" style="gap: 4px; flex: none">
            <button class="btn btn-icon btn-sm" title="向左" @click="scrollRow(p.code, -1)">
              <AppIcon name="chevronLeft" :size="14" />
            </button>
            <button class="btn btn-icon btn-sm" title="向右" @click="scrollRow(p.code, 1)">
              <AppIcon name="chevronRight" :size="14" />
            </button>
            <button class="btn btn-sm btn-ghost" @click="emit('focus', { code: p.code, name: p.name, type })">
              查看全部<AppIcon name="chevronRight" :size="13" />
            </button>
          </div>
        </header>

        <div class="pw-track" :data-code="p.code">
          <button
            v-for="it in rows[p.code]?.items || []"
            :key="artKey(it)"
            class="pw-card"
            :title="it.name"
            @click="openTitle(it)"
          >
            <div class="pw-art">
              <img
                v-if="it.poster && !broken[artKey(it)]"
                :src="it.poster"
                alt=""
                loading="lazy"
                decoding="async"
                @error="markBro(artKey(it))"
              />
              <div v-else class="pw-ph">{{ it.name }}</div>
              <span v-if="it.rating" class="pw-rate">{{ it.rating }}</span>
              <span v-if="it.in_library" class="pw-inlib">已入库</span>
            </div>
            <span class="pw-name">{{ it.name }}</span>
            <span class="pw-sub">{{ it.year || '—' }}</span>
          </button>

          <div v-if="rows[p.code]?.loading && !(rows[p.code]?.items || []).length" class="pw-track-skeleton">
            <div v-for="i in 8" :key="i" class="pw-card">
              <div class="skeleton pw-art" />
              <div class="skeleton" style="height: 11px; width: 80%" />
            </div>
          </div>
        </div>

        <p v-if="rows[p.code]?.err" class="err small pw-msg">{{ rows[p.code]?.err }}</p>
        <p v-else-if="rows[p.code]?.loaded && !(rows[p.code]?.items || []).length" class="muted small pw-msg">
          这个平台暂时没有{{ typeLabel }}片单
        </p>
      </section>
    </div>
  </div>
</template>

<style scoped>
.pw {
  display: flex;
  flex-direction: column;
  gap: 18px;
}
.pw-bar {
  padding: 10px 14px;
}
.pw-group {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.pw-row {
  padding: 12px 14px 14px;
}
.pw-head {
  margin-bottom: 10px;
  gap: 10px;
}
.pw-title {
  font-size: 15px;
  font-weight: 650;
  margin: 0;
  white-space: nowrap;
}
.pw-track {
  display: flex;
  gap: 12px;
  overflow-x: auto;
  padding-bottom: 6px;
  scroll-behavior: smooth;
  scrollbar-width: thin;
}
.pw-track-skeleton {
  display: flex;
  gap: 12px;
}
.pw-card {
  flex: none;
  width: 140px;
  display: flex;
  flex-direction: column;
  gap: 5px;
  padding: 0;
  border: 0;
  background: transparent;
  text-align: left;
  color: var(--foreground);
  cursor: pointer;
}
.pw-art {
  position: relative;
  width: 100%;
  aspect-ratio: 2 / 3;
  border-radius: var(--r-md);
  overflow: hidden;
  background: var(--surface-secondary);
  border: 1px solid var(--border);
  transition: transform 0.18s var(--ease-out-fluid), box-shadow 0.18s ease;
}
.pw-card:hover .pw-art {
  transform: translateY(-3px);
  box-shadow: var(--shadow-md);
}
.pw-art img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.pw-ph {
  width: 100%;
  height: 100%;
  display: grid;
  place-items: center;
  padding: 10px;
  font-size: 12px;
  text-align: center;
  color: var(--muted);
  background: linear-gradient(140deg, var(--surface-secondary), var(--surface));
}
.pw-rate,
.pw-inlib {
  position: absolute;
  padding: 2px 6px;
  border-radius: var(--r-full);
  font-size: 10.5px;
  font-weight: 700;
  background: rgb(10 8 6 / 0.72);
  color: #fff;
}
.pw-rate {
  left: 7px;
  top: 7px;
}
.pw-inlib {
  right: 7px;
  top: 7px;
  background: var(--success);
}
.pw-name {
  font-size: 12.5px;
  font-weight: 550;
  line-height: 1.35;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.pw-sub {
  font-size: 11px;
  color: var(--muted);
}
.pw-msg {
  margin: 6px 0 0;
}
@media (max-width: 720px) {
  .pw-card {
    width: 108px;
  }
  .pw-title {
    font-size: 14px;
  }
}
</style>
