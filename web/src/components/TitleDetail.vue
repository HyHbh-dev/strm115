<script lang="ts">
/** 详情目标：有 tmdbId 最准，只有片名时按 q + year 让后端问 TMDB。 */
export interface TitleTarget {
  type?: string
  tmdbId?: string | number
  q?: string
  year?: string | number
}
</script>

<script setup lang="ts">
/**
 * 影视详情正文 —— 页面和右侧面板共用同一份。
 *
 * 以前这份内容只存在于 TitlePage 里，于是「搜索结果点一条看详情」只能整页
 * 跳过去，回来时搜索页被重建，一屏结果全没了。抽成组件之后，资源搜索页
 * 把同一个详情塞进右栏，左侧的搜索词和结果原样留在原地。
 *
 * 只负责「取数 + 展示」：怎么进来（路由 / 面板）由调用方决定，顶部导航条
 * 也留给调用方 —— 页面要「返回」，面板要「关闭」，两者语义不同。
 */
import { computed, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HPanel from '@/components/HPanel.vue'
import HEmpty from '@/components/HEmpty.vue'
import SubscribeDialog from '@/components/SubscribeDialog.vue'
import { api } from '@/api/endpoints'
import { fmtDate } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    target: TitleTarget
    /** 面板模式：海报小一号，两栏改单栏，按钮文案改成「用这部片搜资源」。 */
    compact?: boolean
  }>(),
  { compact: false },
)

const emit = defineEmits<{
  (e: 'find', p: { q: string; en: string }): void
}>()

interface SeasonLen {
  season: number
  episodes: number
}
interface LocalView {
  exists: boolean
  library?: string
  rel_path?: string
  episodes?: number
  poster?: string
  backdrop?: string
  local_root?: string
  local_dir?: string
}
interface SubView {
  id: number
  state: string
  quality: string
  watermark_season: number
  watermark_episode: number
  total_episodes: number
  last_result?: string
  next_check_at?: string
}
interface TitleDetailData {
  type: string
  tmdb_id: number
  imdb_id?: string
  title: string
  original?: string
  year?: number
  rating?: number
  runtime?: string
  overview?: string
  genres?: string[]
  cast?: string[]
  director?: string[]
  poster?: string
  backdrop?: string
  seasons?: number[]
  season_episodes?: SeasonLen[]
  local?: LocalView
  sub?: SubView
  note?: string
}

const d = ref<TitleDetailData | null>(null)
const loading = ref(false)
const err = ref('')
const posterBroken = ref(false)
const subOpen = ref(false)

const titleFallback = computed(() => String(props.target.q || '').trim())
const typeFallback = computed(() => (props.target.type === 'tv' ? 'tv' : 'movie'))
const displayTitle = computed(() => d.value?.title || titleFallback.value || '影视详情')

async function load() {
  loading.value = true
  err.value = ''
  posterBroken.value = false
  try {
    d.value = (await api.title.detail({
      type: String(props.target.type || typeFallback.value),
      tmdb_id: String(props.target.tmdbId || ''),
      q: titleFallback.value,
      year: String(props.target.year || ''),
    })) as TitleDetailData
  } catch (e: any) {
    // 只有片名的国产剧可能连 TMDB 都认不出：保留标题和「找资源」入口，
    // 别让详情变成一片空白。
    d.value = null
    err.value = e?.message || '详情没拉到'
  } finally {
    loading.value = false
  }
}

watch(() => JSON.stringify(props.target), () => void load(), { immediate: true })

/** 找资源：把这部片的片名与英文原名交给调用方（页面跳转 / 面板重搜）。 */
function findResource() {
  emit('find', {
    q: d.value?.title || titleFallback.value,
    en: d.value?.original || '',
  })
}

/**
 * 订阅弹窗要的是「认准的那部片」：tmdb_id 是后端拉季集、生成专有追更日历
 * 的唯一依据，所以详情接口没认出 TMDB 时订阅按钮要禁用，不能硬建一条
 * 永远查不到集数的订阅。
 *
 * 这里用结构兼容而不用 import type：SubscribeDialog 的 props 是同一份字段，
 * 少一层 SFC 类型导出的耦合。
 */
const subHit = computed(() => {
  const x = d.value
  const tmdbId = Number(x?.tmdb_id || 0)
  if (!tmdbId) return null
  return {
    id: tmdbId,
    kind: x?.type || typeFallback.value,
    title: x?.title || titleFallback.value,
    original: x?.original || '',
    year: x?.year || props.target.year || '',
    source: 'tmdb',
  }
})
const canSubscribe = computed(() => !!subHit.value)
const subTitleTip = computed(() => {
  if (d.value?.sub) return '已经在订阅里了，点开看追更状态'
  if (!canSubscribe.value) return '详情没认出 TMDB ID，先在搜索里选中候选再订阅'
  return '按 TMDB 季集集合选起始集，生成专有追更日历'
})

/** 订阅成功后重拉详情：「入库与订阅」面板里的状态和起播线要立刻跟着变。 */
function onSubscribed() {
  void load()
}

const tmdbURL = computed(() => {
  const id = d.value?.tmdb_id
  if (!id) return ''
  const seg = (d.value?.type || typeFallback.value) === 'tv' ? 'tv' : 'movie'
  return `https://www.themoviedb.org/${seg}/${id}`
})
const imdbURL = computed(() =>
  d.value?.imdb_id ? `https://www.imdb.com/title/${d.value.imdb_id}/` : '',
)
const poster = computed(() => d.value?.poster || d.value?.local?.poster || '')
const backdrop = computed(() => d.value?.backdrop || d.value?.local?.backdrop || '')
const seasonLens = computed(() => d.value?.season_episodes ?? [])
const subText: Record<string, string> = {
  watching: '追更中',
  waiting: '等待新集',
  done: '已入库',
  paused: '已暂停',
}
</script>

<template>
  <div class="tp" :class="{ compact }">
    <div v-if="loading" class="panel tp-skeleton">
      <div class="skeleton" style="height: 22px; width: 45%" />
      <div class="skeleton" style="height: 14px; width: 70%; margin-top: 14px" />
      <div class="skeleton" style="height: 14px; width: 60%" />
    </div>

    <template v-else>
      <!-- 详情头：背景只做很淡的一层装饰，正文始终用主题色，明暗主题都清楚 -->
      <div
        class="panel tp-hero"
        :style="
          backdrop
            ? {
                backgroundImage: `linear-gradient(90deg, var(--surface) 30%, color-mix(in oklab, var(--surface) 78%, transparent)), url(${backdrop})`,
              }
            : undefined
        "
      >
        <div class="tp-poster">
          <img v-if="poster && !posterBroken" :src="poster" alt="" @error="posterBroken = true" />
          <div v-else class="tp-ph">{{ displayTitle }}</div>
        </div>
        <div class="tp-meta">
          <div class="row wrap" style="gap: 8px">
            <span class="badge badge-accent">{{ (d?.type || typeFallback) === 'tv' ? '剧集' : '电影' }}</span>
            <span v-if="d?.local?.exists" class="badge badge-success">已入库</span>
            <span v-if="d?.sub" class="badge badge-warning">已订阅</span>
            <span class="grow" />
            <a v-if="tmdbURL" class="tp-ext" :href="tmdbURL" target="_blank" rel="noopener" title="TMDB">
              <AppIcon name="external" :size="13" />TMDB
            </a>
            <a v-if="imdbURL" class="tp-ext" :href="imdbURL" target="_blank" rel="noopener" title="IMDb">
              <AppIcon name="external" :size="13" />IMDb
            </a>
          </div>
          <h2 class="tp-title">{{ displayTitle }}</h2>
          <p v-if="d?.original && d.original !== d.title" class="muted small tp-original">
            {{ d.original }}
          </p>
          <p class="muted small tp-facts">
            <span v-if="d?.year">{{ d.year }}</span>
            <span v-if="d?.rating"> · ★ {{ Number(d.rating).toFixed(1) }}</span>
            <span v-if="d?.runtime"> · {{ d.runtime }}</span>
            <span v-for="g in (d?.genres || []).slice(0, 4)" :key="g"> · {{ g }}</span>
          </p>
          <p v-if="d?.overview" class="tp-overview">{{ d.overview }}</p>
          <p v-else class="muted small">这部片暂时没有简介。</p>

          <div class="row wrap" style="gap: 8px; margin-top: 12px">
            <button class="btn btn-primary" :disabled="!displayTitle" @click="findResource">
              <AppIcon name="search" :size="15" />{{ compact ? '用这部片搜资源' : '找资源' }}
            </button>
            <button
              class="btn"
              :class="d?.sub ? 'btn-tertiary' : 'btn-primary'"
              :disabled="!canSubscribe"
              :title="subTitleTip"
              @click="subOpen = true"
            >
              <AppIcon :name="d?.sub ? 'check' : 'bell'" :size="15" />
              {{ d?.sub ? '已订阅' : '订阅追更' }}
            </button>
            <span class="muted small">
              {{ compact ? '把片名填回左侧搜索框，或直接订阅追更' : '搜 TG 频道与种子站，或订阅后按播出日自动追更' }}
            </span>
          </div>
        </div>
      </div>

      <p v-if="err" class="err small">
        详情接口没认出这部片（{{ err }}）；仍可用上面的「找资源」按片名搜索。
      </p>

      <div class="tp-cols" :class="compact ? '' : 'grid-2'">
        <HPanel title="主创与分季" desc="来自 TMDB 的元数据">
          <div class="col" style="gap: 12px">
            <div v-if="d?.director?.length">
              <span class="muted small">导演</span>
              <p class="tp-line">{{ d.director.join(' / ') }}</p>
            </div>
            <div v-if="d?.cast?.length">
              <span class="muted small">主演</span>
              <p class="tp-line">{{ d.cast.slice(0, 12).join(' / ') }}</p>
            </div>
            <div v-if="seasonLens.length">
              <span class="muted small">分季集数</span>
              <div class="row wrap" style="gap: 6px; margin-top: 6px">
                <span v-for="s in seasonLens" :key="s.season" class="badge">
                  第 {{ s.season }} 季 · {{ s.episodes }} 集
                </span>
              </div>
            </div>
            <div v-else-if="d?.seasons?.length">
              <span class="muted small">季</span>
              <div class="row wrap" style="gap: 6px; margin-top: 6px">
                <span v-for="s in d.seasons" :key="s" class="badge">第 {{ s }} 季</span>
              </div>
            </div>
            <HEmpty
              v-if="!d?.director?.length && !d?.cast?.length && !seasonLens.length && !d?.seasons?.length"
              title="暂无更多元数据"
              desc="认片时只拿到了片名，详情接口没有返回演职员或分季信息"
              icon="info"
            />
          </div>
        </HPanel>

        <HPanel title="入库与订阅" desc="本地台账和追更状态">
          <div class="col" style="gap: 12px">
            <div class="row-between small">
              <span class="muted">本地媒体库</span>
              <span>{{ d?.local?.library || (d?.local?.exists ? '已入库' : '未入库') }}</span>
            </div>
            <div v-if="d?.local?.rel_path" class="row-between small">
              <span class="muted">STRM 路径</span>
              <span class="mono truncate tp-path" :title="d.local.rel_path">{{ d.local.rel_path }}</span>
            </div>
            <div v-if="d?.local?.episodes" class="row-between small">
              <span class="muted">已入库集数</span>
              <span>{{ d.local.episodes }} 集</span>
            </div>
            <hr class="divider" />
            <template v-if="d?.sub">
              <div class="row-between small">
                <span class="muted">订阅状态</span>
                <span class="badge badge-accent">{{ subText[d.sub.state] || d.sub.state }}</span>
              </div>
              <div class="row-between small">
                <span class="muted">起播线</span>
                <span class="mono">S{{ d.sub.watermark_season }}E{{ d.sub.watermark_episode }}</span>
              </div>
              <div v-if="d.sub.total_episodes" class="row-between small">
                <span class="muted">总集数</span>
                <span class="mono">{{ d.sub.total_episodes }}</span>
              </div>
              <div v-if="d.sub.next_check_at" class="row-between small">
                <span class="muted">下次检查</span>
                <span>{{ fmtDate(d.sub.next_check_at) }}</span>
              </div>
              <p v-if="d.sub.last_result" class="muted small tp-line">{{ d.sub.last_result }}</p>
            </template>
            <div v-else class="col" style="gap: 8px; align-items: flex-start">
              <p class="muted small">
                还没订阅这部片。订阅后按 TMDB 的单集播出日生成专有追更日历，到播出日自动搜资源。
              </p>
              <button
                class="btn btn-sm btn-tertiary"
                :disabled="!canSubscribe"
                :title="subTitleTip"
                @click="subOpen = true"
              >
                <AppIcon name="bell" :size="13" />订阅这部片
              </button>
            </div>
          </div>
        </HPanel>
      </div>

    </template>

    <!-- 页面和搜索页右侧面板共用同一个弹窗：剧集按季集选起始集，电影直接订。
         放在 loading 分支之外，订阅成功后重拉详情不会把弹窗中途卸载。 -->
    <SubscribeDialog
      v-model="subOpen"
      :hit="subHit"
      :already-subscribed="!!d?.sub"
      @subscribed="onSubscribed"
    />
  </div>
</template>

<style scoped>
.tp {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.tp-hero {
  display: flex;
  align-items: flex-start;
  gap: 22px;
  padding: 20px;
  background-size: cover;
  background-position: center right;
  overflow: hidden;
}
.compact .tp-hero {
  flex-direction: column;
  align-items: stretch;
  gap: 14px;
  padding: 14px;
}
.tp-poster {
  flex: none;
  width: 168px;
  aspect-ratio: 2 / 3;
  border-radius: var(--r-md);
  overflow: hidden;
  border: 1px solid var(--border);
  background: var(--surface-secondary);
  box-shadow: var(--shadow-md);
}
.compact .tp-poster {
  width: 108px;
}
.tp-poster img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.tp-ph {
  width: 100%;
  height: 100%;
  display: grid;
  place-items: center;
  padding: 14px;
  text-align: center;
  font-size: 13px;
  color: var(--muted);
}
.tp-meta {
  flex: 1;
  min-width: 0;
}
.tp-title {
  font-size: 25px;
  font-weight: 680;
  letter-spacing: -0.02em;
  margin: 8px 0 0;
}
.compact .tp-title {
  font-size: 20px;
}
.tp-original {
  margin: 3px 0 0;
}
.tp-facts {
  margin: 8px 0 0;
}
.tp-overview {
  margin: 12px 0 0;
  font-size: 13.5px;
  line-height: 1.7;
  max-width: 78ch;
}
.tp-ext {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11.5px;
  color: var(--muted);
  text-decoration: none;
}
.tp-ext:hover {
  color: var(--accent);
}
.tp-skeleton {
  padding: 28px;
}
.tp-cols {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.tp-cols.grid-2 {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  align-items: start;
}
.tp-line {
  margin: 4px 0 0;
  font-size: 13px;
  line-height: 1.6;
}
.tp-path {
  max-width: 62%;
  text-align: right;
}
@media (max-width: 720px) {
  .tp-hero {
    flex-direction: column;
    align-items: center;
  }
  .tp-poster {
    width: 132px;
  }
  .tp-title {
    font-size: 21px;
  }
}
</style>
