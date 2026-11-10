<script setup lang="ts">
/**
 * 影视候选选择器（可复用）。
 *
 * 流程：输入词 → 同时问 TMDB 与豆瓣拿候选 → 用户点选锁定准确的那部 → 才去搜资源。
 * 抽成独立组件，是为了资源搜索页和电影墙详情抽屉（WallBrowser）共用同一份
 * 「认片」交互；本组件只负责候选，选中 / 跳过之后做什么由父组件决定。
 *
 * 为什么两个源一起问：国产剧和港台片在 TMDB 上常常查不到、或者查到的是
 * 英文条目（没有中文名，搜种子站就对不上），豆瓣的搜索建议反而准。两个源
 * 各自失败互不影响 —— 豆瓣被墙时 TMDB 的候选照常出来，反过来也一样。
 *
 * 为什么不用 Promise.allSettled 等全部回来：豆瓣走反爬限流常常慢好几秒，
 * 以前 TMDB 明明早就好了也得一起等，用户看着空白干等。现在谁先回来谁先
 * setState 渲染，豆瓣到了再追加，两边各自 try/catch，互不拖累。
 *
 * 直接用 http 而非 endpoints.ts：这次改动里接线方要统一改 endpoints.ts，
 * 组件自己直连 /tmdb/search 能避免两边互相覆盖。
 *
 * initial 只作为输入框初始值；要「预填关键词并立即查」请调暴露的 setQuery。
 * 这样父组件（打开候选 / 换一部 / 抽屉打开）只有一个触发入口，不会重复打请求。
 */
import { nextTick, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import { http } from '@/api/client'

// 和后端 /api/tmdb/search 返回的 items 字段一一对应。导出类型方便父组件
// （资源搜索页、WallBrowser 抽屉）按需 import type 复用。
export interface TitleHit {
  id: number
  kind: string
  title: string
  original: string
  year: string
  poster: string
  vote: number
  overview: string
  /** 豆瓣候选的副标题（演员表）；不是英文原名，别拿去搜种子站 */
  subtitle?: string
  /** tmdb / douban —— 父组件据此决定点开谁（豆瓣条目没有 TMDB 详情） */
  source?: 'tmdb' | 'douban'
  /** 豆瓣条目在原站的地址（TMDB 条目用不到） */
  url?: string
}

const props = withDefaults(
  defineProps<{
    initial?: string
    compact?: boolean
    autoFocus?: boolean
  }>(),
  { initial: '', compact: false, autoFocus: false },
)

const emit = defineEmits<{
  pick: [hit: TitleHit]
  skip: [q: string]
}>()

const query = ref(props.initial)
const items = ref<TitleHit[]>([])
const loading = ref(false)
const error = ref('')
const searched = ref(false)
// 海报直连 image.tmdb.org（和电影墙一致）；加载失败按「kind:id」记，隐藏图片显示首字母占位。
const posterFailed = ref<Record<string, boolean>>({})
const inputEl = ref<HTMLInputElement | null>(null)

/** 豆瓣的返回结构（只列用到的字段）：search 只给片名/年份/海报，没有简介。 */
interface DoubanHit {
  id: number
  kind: string
  title: string
  original?: string
  year?: number
  poster?: string
  rating?: number
  url?: string
}

async function searchTMDB(q: string): Promise<TitleHit[]> {
  // type 留空 = 电影和剧集两类都搜；client.ts 会跳过空串参数，后端按两类处理。
  const r = await http.get<{ items?: TitleHit[] }>('/tmdb/search', { q, type: '' })
  const list = Array.isArray(r?.items) ? r.items : []
  return list.slice(0, 8).map((h) => ({ ...h, source: 'tmdb' as const }))
}

async function searchDouban(q: string): Promise<TitleHit[]> {
  const r = await http.get<{ items?: DoubanHit[]; disabled?: boolean }>('/douban/search', {
    q,
    limit: 8,
  })
  const list = Array.isArray(r?.items) ? r.items : []
  return list.map((d) => ({
    id: d.id,
    kind: d.kind === 'tv' ? 'tv' : 'movie',
    title: d.title || '',
    // 豆瓣 /j/subject_suggest 的 sub_title 是演员表，不是 original_title。
    // 之前把它塞进 original，点候选后会被当成英文名去搜种子站，全搜空。
    original: '',
    subtitle: d.original || '',
    year: d.year ? String(d.year) : '',
    poster: d.poster || '',
    vote: d.rating || 0,
    overview: '',
    source: 'douban' as const,
    url: d.url,
  }))
}

// 防止「上一次查询」的慢响应（尤其豆瓣走反爬、可能晚好几秒）盖掉新一次
// 查询的结果：每轮搜索自增，回调只认自己那一轮的 seq。
let searchSeq = 0

function doSearch() {
  const q = query.value.trim()
  if (!q) {
    error.value = '先输入片名'
    return
  }
  const seq = ++searchSeq
  loading.value = true
  error.value = ''
  searched.value = true
  posterFailed.value = {}
  items.value = [] // 清掉上一轮，给两源各自腾地方

  // 两个源各自独立请求、各自 setState：谁先回来谁先渲染，绝不互相等。
  // TMDB 快就先把候选摆出来（第一次 setState），豆瓣到了再追加（第二次）；
  // 一边失败不影响另一边。保留每条的 source 徽标，父组件据此决定点开谁。
  let dbHits: TitleHit[] = [] // 已到的豆瓣结果，供晚到的 TMDB 拼在后面
  let tmError = ''
  let dbError = ''
  let pending = 2 // 两个请求都落地才收起 loading / 定最终错误文案

  const settle = () => {
    if (seq !== searchSeq) return // 已有更新的查询，丢弃这轮收尾
    pending--
    if (pending > 0) return
    loading.value = false
    if (tmError && dbError) {
      error.value = tmError || dbError || '两个数据源都没查到'
    } else if (tmError) {
      error.value = 'TMDB 查询失败，下面是豆瓣的候选'
    }
    // 只挂豆瓣时不报错：TMDB 候选照常展示，不打扰用户。
  }

  // TMDB 在前：它的 id 能直接开详情、能建订阅，所以排在候选列表前面。
  void searchTMDB(q)
    .then((list) => {
      if (seq !== searchSeq) return
      items.value = [...list, ...dbHits]
    })
    .catch((e) => {
      if (seq !== searchSeq) return
      tmError = e?.message || 'TMDB 查询失败'
    })
    .finally(settle)

  // 豆瓣到了再追加：剔除旧的豆瓣项后拼到已有（TMDB）结果后面，顺序仍 TMDB 优先。
  void searchDouban(q)
    .then((list) => {
      if (seq !== searchSeq) return
      dbHits = list
      items.value = [...items.value.filter((h) => h.source !== 'douban'), ...list]
    })
    .catch((e) => {
      if (seq !== searchSeq) return
      dbError = e?.message || '豆瓣查询失败'
    })
    .finally(settle)
}

function posterKey(h: TitleHit) {
  // 两个源的 id 会撞（都是数字），必须带来源
  return (h.source || 'tmdb') + ':' + h.kind + ':' + h.id
}

/** 首字母占位：中文取第一个字，英文取首字母，都没有就用问号。 */
function initialOf(h: TitleHit) {
  return (h.title || h.original || h.subtitle || '?').trim().charAt(0).toUpperCase()
}

function kindLabel(kind: string) {
  return kind === 'tv' ? '剧集' : '电影'
}

function voteText(v: number) {
  return v > 0 ? `★ ${v.toFixed(1)}` : '★ —'
}

function onPosterError(h: TitleHit) {
  posterFailed.value = { ...posterFailed.value, [posterKey(h)]: true }
}

function pick(h: TitleHit) {
  emit('pick', h)
}

function skip() {
  emit('skip', query.value.trim())
}

/** 父组件预填关键词并立即查候选（打开候选、换一部、抽屉打开时都走这里）。 */
function setQuery(q: string) {
  query.value = q
  doSearch()
}

function focusInput() {
  nextTick(() => inputEl.value?.focus())
}

onMounted(() => {
  if (props.autoFocus) focusInput()
})

defineExpose({ focusInput, setQuery })
</script>

<template>
  <div class="title-picker" :class="{ compact }">
    <div class="tp-search">
      <AppIcon name="search" :size="15" />
      <input
        ref="inputEl"
        v-model="query"
        class="input"
        :placeholder="compact ? '输入片名找候选（TMDB + 豆瓣）' : '输入片名，先确认是哪部再搜资源'"
        @keydown.enter="doSearch"
      />
      <button class="btn btn-primary btn-sm" :disabled="loading" @click="doSearch">
        {{ loading ? '查找中…' : '找片' }}
      </button>
    </div>

    <p v-if="loading" class="tp-hint muted small">
      正在向 TMDB 与豆瓣查询候选…（先到的结果会先显示，不必等两边都回来）
    </p>
    <p v-else-if="error" class="tp-hint tp-error small">
      {{ error }}，可直接按关键词搜，或改个词再找一次。
    </p>
    <p v-else-if="searched && !items.length" class="tp-hint muted small">
      没找到相关影视，可直接按关键词搜。
    </p>

    <div v-if="items.length" class="tp-list">
      <button
        v-for="h in items"
        :key="posterKey(h)"
        class="tp-card"
        @click="pick(h)"
      >
        <span class="tp-poster">
          <img
            v-if="h.poster && !posterFailed[posterKey(h)]"
            :src="h.poster"
            :alt="h.title"
            loading="lazy"
            @error="onPosterError(h)"
          />
          <span v-else class="tp-fallback">{{ initialOf(h) }}</span>
        </span>
        <span class="tp-meta">
          <span class="tp-name">
            <strong>{{ h.title || h.original }}</strong>
            <span class="badge" :class="h.kind === 'tv' ? 'badge-info' : ''">{{ kindLabel(h.kind) }}</span>
            <!-- 来源要标出来：两个源同名条目很多，点错一个就会去搜一部不存在的片 -->
            <span class="badge" :class="h.source === 'douban' ? 'badge-success' : 'badge-accent'">
              {{ h.source === 'douban' ? '豆瓣' : 'TMDB' }}
            </span>
          </span>
          <span class="tp-sub muted small">
            <span
              v-if="(h.original || h.subtitle) && (h.original || h.subtitle) !== h.title"
              class="truncate"
            >{{ h.original || h.subtitle }}</span>
            <span v-if="h.year">{{ h.year }}</span>
            <span class="tp-vote">{{ voteText(h.vote) }}</span>
          </span>
          <span v-if="h.overview" class="tp-desc muted small">{{ h.overview }}</span>
        </span>
      </button>
    </div>

    <button class="btn btn-tertiary btn-sm tp-skip" @click="skip">
      跳过，直接按关键词搜
      <AppIcon name="close" :size="12" />
    </button>
  </div>
</template>

<style scoped>
.title-picker {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.title-picker.compact {
  padding: 8px;
  gap: 8px;
}
.tp-search {
  display: flex;
  align-items: center;
  gap: 8px;
}
.tp-search .input {
  flex: 1;
  min-width: 0;
}
.tp-search > svg {
  flex: none;
  color: var(--muted);
}
.tp-hint {
  margin: 0;
  line-height: 1.5;
}
.tp-error {
  color: var(--danger, #e05555);
}
.tp-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  max-height: 340px;
  overflow-y: auto;
}
.tp-card {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  width: 100%;
  padding: 8px;
  font: inherit;
  text-align: left;
  color: var(--foreground);
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
  cursor: pointer;
}
.tp-card:hover {
  border-color: var(--accent);
}
.tp-poster {
  flex: none;
  width: 46px;
  height: 68px;
  display: grid;
  place-items: center;
  overflow: hidden;
  border-radius: 6px;
  background: var(--accent-soft, var(--surface-secondary));
}
.tp-poster img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}
.tp-fallback {
  font-size: 20px;
  font-weight: 700;
  color: var(--accent);
}
.tp-meta {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.tp-name {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.tp-name strong {
  font-size: 13px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tp-sub {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.tp-sub .truncate {
  max-width: 200px;
}
.tp-vote {
  color: var(--accent);
}
.tp-desc {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  line-height: 1.45;
}
.tp-skip {
  align-self: flex-start;
}
.title-picker.compact .tp-poster {
  width: 38px;
  height: 56px;
}
.title-picker.compact .tp-list {
  max-height: 260px;
}
</style>
