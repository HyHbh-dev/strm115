<script setup lang="ts">
/**
 * 从搜索结果一键订阅。
 *
 * 剧集要先按 TMDB 返回的「季 → 集」集合选起始集：这是后端生成专有播放
 * 日历的依据，选错就是白白把前面几十集排进重试队列。电影没有集数，直接订。
 *
 * 起始集用「这一集及之前不下载」的语义（后端 watermark），所以弹窗里
 * 默认给「从头追」，选到 S02E05 就表示 S02E05 及之前都不找。
 */
import { computed, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HModal from '@/components/HModal.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { loadSubsIndex, rememberSub, subIdOf, subsVersion } from '@/stores/subscriptions'

export interface SubscribeHit {
  id: number
  kind: string
  title: string
  original?: string
  year?: string | number
  source?: string
}

interface Season {
  season: number
  episode_count: number
  name?: string
  air_date?: string
}
interface Episode {
  season: number
  episode: number
  name?: string
  air_date?: string
}
/** 单集详情（GET /api/tmdb/episode），字段与后端 tmdb.SeasonEpisode 对齐。 */
interface EpisodeDetail {
  season: number
  episode: number
  name: string
  air_date: string
  still: string
  overview: string
  runtime: number
  rating: number
}

const props = defineProps<{
  modelValue: boolean
  hit: SubscribeHit | null
  /** 调用方已经知道「订过了」时直接传 true，省得等订阅索引加载完才判断。 */
  alreadySubscribed?: boolean
}>()
const emit = defineEmits<{
  'update:modelValue': [boolean]
  subscribed: [{ id: number; calendarTotal: number }]
}>()

const loading = ref(false)
const saving = ref(false)
const error = ref('')
const seasons = ref<Season[]>([])
const episodes = ref<Episode[]>([])
const fromBeginning = ref(true)
const season = ref(1)
const episode = ref(1)
const seasonLoading = ref(false)

// 选中集后铺在弹窗下方的单集详情。请求序号用来防竞态：快速连点 E208→E209 时，
// 只认最后一次请求的结果，先发的慢响应不许覆盖后发的结果。
const epDetail = ref<EpisodeDetail | null>(null)
const epLoading = ref(false)
const epError = ref(false)
const epStillFailed = ref(false)
let epReqSeq = 0

// 一季可能上百集：按 50 集分一段展示，别让一个固定高度的滚动网格吞掉后面几十集。
const EP_SEGMENT = 50
const segment = ref(0)
const jumpTo = ref<string | number>('')

const totalEpisodes = computed(() => episodeCountOf(season.value))
const segmentCount = computed(() => Math.max(1, Math.ceil(totalEpisodes.value / EP_SEGMENT)))
const segmentStart = computed(() => segment.value * EP_SEGMENT + 1)
const segmentEnd = computed(() => Math.min(totalEpisodes.value, segmentStart.value + EP_SEGMENT - 1))
const segmentEpisodes = computed(() => {
  const out: number[] = []
  for (let n = segmentStart.value; n <= segmentEnd.value; n++) out.push(n)
  return out
})
const showSegmentNav = computed(() => totalEpisodes.value > EP_SEGMENT)

function segmentLabel(i: number): string {
  const start = i * EP_SEGMENT + 1
  const end = Math.min(totalEpisodes.value, start + EP_SEGMENT - 1)
  return `E${String(start).padStart(2, '0')}–E${String(end).padStart(2, '0')}`
}
const segments = computed(() =>
  Array.from({ length: segmentCount.value }, (_, i) => ({ index: i, label: segmentLabel(i) })),
)

// 季清单是异步刷新的：集数变少时把段号夹回合法范围，免得网格空着。
watch(totalEpisodes, (total) => {
  const last = Math.max(0, Math.ceil(total / EP_SEGMENT) - 1)
  if (segment.value > last) segment.value = last
})

const isTV = computed(() => props.hit?.kind === 'tv')
const hasTMDB = computed(() => !!props.hit && props.hit.source !== 'douban' && props.hit.id > 0)
const already = computed(() => {
  void subsVersion.value
  if (props.alreadySubscribed) return true
  const h = props.hit
  if (!h || !hasTMDB.value) return false
  return subIdOf(isTV.value ? 'tv' : 'movie', h.id) > 0
})
const startLabel = computed(() => {
  if (!isTV.value) return '有资源就下一次'
  if (fromBeginning.value) return '从第 1 季第 1 集开始（从头追）'
  return `从 S${String(season.value).padStart(2, '0')}E${String(episode.value).padStart(2, '0')} 之后开始`
})

function close() {
  emit('update:modelValue', false)
}

async function loadSeasons() {
  const h = props.hit
  if (!h || !isTV.value || !hasTMDB.value) return
  loading.value = true
  error.value = ''
  try {
    const r: any = await api.subscriptions.episodes(h.id)
    seasons.value = Array.isArray(r?.seasons) ? r.seasons.filter((s: Season) => s.season > 0) : []
    fromBeginning.value = true
    segment.value = 0
    jumpTo.value = ''
    const first = seasons.value[0]
    if (first) {
      season.value = first.season
      episode.value = 1
      await loadEpisodes(first.season)
    }
  } catch (e: any) {
    error.value = e?.message || '没读到 TMDB 集数集合'
  } finally {
    loading.value = false
  }
}

async function loadEpisodes(s: number) {
  const h = props.hit
  if (!h || !hasTMDB.value) return
  seasonLoading.value = true
  try {
    const r: any = await api.subscriptions.episodes(h.id, s)
    episodes.value = Array.isArray(r?.episodes) ? r.episodes : []
  } catch (e: any) {
    // 单季失败不挡住订阅：季清单里还有 episode_count，照样能选集号。
    error.value = e?.message || '这一季的逐集信息没读到'
    episodes.value = []
  } finally {
    seasonLoading.value = false
  }
}

async function chooseSeason(s: number) {
  season.value = s
  episode.value = 1
  segment.value = 0
  jumpTo.value = ''
  fromBeginning.value = false
  await loadEpisodes(s)
}

function chooseEpisode(e: number) {
  episode.value = e
  fromBeginning.value = false
  // 从「跳到集号」进来时集号可能不在当前段，段号跟着走，grid 里才高亮得到它。
  segment.value = Math.floor((e - 1) / EP_SEGMENT)
}

function chooseSegment(i: number) {
  if (i < 0 || i >= segmentCount.value) return
  segment.value = i
}

function jumpToEpisode() {
  const total = totalEpisodes.value
  if (!total) {
    toast.error('这一季 TMDB 没给集数，先选「从头追」')
    return
  }
  const raw = String(jumpTo.value ?? '').trim()
  if (!/^\d+$/.test(raw)) {
    toast.error('请输入集号（数字）')
    return
  }
  const n = Number(raw)
  if (!Number.isSafeInteger(n) || n < 1 || n > total) {
    toast.error(`集号要在 1–${total} 之间`)
    return
  }
  chooseEpisode(n)
  jumpTo.value = ''
}

function episodeCountOf(s: number): number {
  const hit = seasons.value.find((x) => x.season === s)
  if (hit?.episode_count) return hit.episode_count
  if (s === season.value && episodes.value.length) return episodes.value.length
  return 0
}

function airDateOf(e: number): string {
  return episodes.value.find((x) => x.episode === e)?.air_date || ''
}

const showEpPanel = computed(
  () => isTV.value && hasTMDB.value && !fromBeginning.value,
)
const epCode = computed(
  () => `S${String(season.value).padStart(2, '0')}E${String(episode.value).padStart(2, '0')}`,
)
// 详情字段优先取接口返回值，缺的回落到本地逐集清单：接口 404/400 整块降级时也靠它。
const localEp = computed(() => episodes.value.find((x) => x.episode === episode.value) || null)
const epTitle = computed(
  () => epDetail.value?.name || localEp.value?.name || `第 ${episode.value} 集`,
)
const epAirDate = computed(() => epDetail.value?.air_date || localEp.value?.air_date || '')
const epStill = computed(() => epDetail.value?.still || '')
const epOverview = computed(() => epDetail.value?.overview || '')
const epRuntime = computed(() => epDetail.value?.runtime || 0)
const epRating = computed(() => epDetail.value?.rating || 0)
const epOverviewPlaceholder = computed(() =>
  epError.value ? '这一集的详情暂时读不到，已按集号与播出日显示' : 'TMDB 还没收录这一集的简介',
)

function onStillError() {
  epStillFailed.value = true
}

function resetEpDetail() {
  // 序号一加，在途的旧请求回调就不会再写状态（防快速连点竞态）。
  epReqSeq++
  epDetail.value = null
  epLoading.value = false
  epError.value = false
  epStillFailed.value = false
}

async function fetchEpisodeDetail(tmdbId: number, s: number, e: number) {
  const seq = ++epReqSeq
  epDetail.value = null
  epLoading.value = true
  epError.value = false
  epStillFailed.value = false
  try {
    const d: any = await api.subscriptions.episodeDetail(tmdbId, s, e)
    if (seq !== epReqSeq) return // 已被更晚的选择取代
    epDetail.value = {
      season: Number(d?.season ?? s),
      episode: Number(d?.episode ?? e),
      name: String(d?.name ?? ''),
      air_date: String(d?.air_date ?? ''),
      still: String(d?.still ?? ''),
      overview: String(d?.overview ?? ''),
      runtime: Number(d?.runtime ?? 0),
      rating: Number(d?.rating ?? 0),
    }
  } catch {
    if (seq !== epReqSeq) return
    // 404（没这一集）/ 400（没配 TMDB Key）/ 网络失败：只置一个降级标记，
    // 面板照常用本地集信息渲染，不显示红字、不挡确认按钮。
    epError.value = true
    epDetail.value = null
  } finally {
    if (seq === epReqSeq) epLoading.value = false
  }
}

// 只在「剧集 + 有 TMDB id + 选了具体集 + 弹窗打开」时拉详情；
// 从头追、电影、弹窗关闭都不拉，并顺手把状态清干净。
watch(
  [season, episode, () => props.hit?.id, isTV, hasTMDB, fromBeginning, () => props.modelValue],
  () => {
    const shouldFetch =
      props.modelValue &&
      isTV.value &&
      hasTMDB.value &&
      !fromBeginning.value &&
      season.value > 0 &&
      episode.value > 0
    if (!shouldFetch) {
      resetEpDetail()
      return
    }
    void fetchEpisodeDetail(props.hit!.id, season.value, episode.value)
  },
  { immediate: true },
)

async function submit() {
  const h = props.hit
  if (!h || !hasTMDB.value || saving.value) return
  saving.value = true
  error.value = ''
  try {
    const r: any = await api.subscriptions.create({
      tmdb_id: h.id,
      media_type: isTV.value ? 'tv' : 'movie',
      title: h.title,
      original_title: h.original || '',
      year: Number(h.year) || 0,
      watermark_season: isTV.value && !fromBeginning.value ? season.value : 0,
      watermark_episode: isTV.value && !fromBeginning.value ? episode.value : 0,
    })
    void rememberSub(isTV.value ? 'tv' : 'movie', h.id, Number(r?.id || 0))
    await loadSubsIndex(true)
    const total = Number(r?.calendar_total || 0)
    toast.success(
      `已订阅《${h.title}》`,
      isTV.value ? `${startLabel.value}；已生成 ${total} 集专有追更日历` : '有资源会自动提交离线',
    )
    emit('subscribed', { id: Number(r?.id || 0), calendarTotal: total })
    close()
  } catch (e: any) {
    const msg = String(e?.message || '')
    if (msg.includes('409') || msg.includes('已经')) {
      error.value = '这部片已经在订阅里了'
      void loadSubsIndex(true)
    } else {
      error.value = msg || '订阅失败'
    }
  } finally {
    saving.value = false
  }
}

watch(
  () => props.modelValue,
  (v) => {
    if (v && isTV.value && !seasons.value.length) void loadSeasons()
    if (!v) error.value = ''
  },
)
// 换了一部片，季清单必须重拉，否则会显示上一部剧的集数。
watch(
  () => props.hit?.id,
  () => {
    seasons.value = []
    episodes.value = []
    segment.value = 0
    jumpTo.value = ''
    if (props.modelValue) void loadSeasons()
  },
)
</script>

<template>
  <HModal :model-value="modelValue" :title="hit ? `订阅《${hit.title}》` : '订阅'" size="lg" @update:model-value="emit('update:modelValue', $event)">
    <template #desc>订好之后按 TMDB 的单集播出日生成你自己的追更日历，到播出日自动搜资源。</template>

    <div v-if="!hit" class="muted small">先认片再订阅。</div>
    <div v-else-if="already" class="sub-done">
      <AppIcon name="check" :size="18" />
      <div>
        <strong>已经在订阅里了</strong>
        <p class="muted small">到「发现与订阅 → 订阅」里改画质、起播线或暂停。</p>
      </div>
    </div>
    <div v-else class="col" style="gap: 14px">
      <div class="row wrap" style="gap: 8px">
        <span class="badge">{{ isTV ? '剧集' : '电影' }}</span>
        <span v-if="hit.original" class="muted small">{{ hit.original }}</span>
        <span v-if="hit.year" class="muted small">{{ hit.year }}</span>
      </div>

      <template v-if="isTV">
        <div v-if="loading" class="muted small">正在读 TMDB 集数集合…</div>
        <template v-else>
          <div class="sub-start">
            <strong class="small">从哪一集开始订阅</strong>
            <p class="muted small">起始集及之前的集不会下载，也不会进搜索队列。</p>
            <label class="row" style="gap: 8px; margin-top: 8px; cursor: pointer">
              <input v-model="fromBeginning" type="radio" :value="true" />
              <span class="small">从头追（全部集都订阅）</span>
            </label>
            <label class="row" style="gap: 8px; cursor: pointer">
              <input v-model="fromBeginning" type="radio" :value="false" />
              <span class="small">从指定集之后开始</span>
            </label>
          </div>

          <div v-if="!fromBeginning" class="sub-pick">
            <div class="row wrap" style="gap: 8px; margin-bottom: 10px">
              <span class="small muted">选季</span>
              <button
                v-for="s in seasons"
                :key="s.season"
                class="btn btn-sm"
                :class="season === s.season ? 'btn-primary' : 'btn-tertiary'"
                @click="chooseSeason(s.season)"
              >
                第 {{ s.season }} 季
                <span class="muted">· {{ s.episode_count }} 集</span>
              </button>
            </div>

            <div v-if="!seasonLoading && showSegmentNav" class="row wrap" style="gap: 8px; margin-bottom: 10px">
              <span class="small muted">分段</span>
              <button
                v-for="seg in segments"
                :key="seg.index"
                class="btn btn-sm"
                :class="segment === seg.index ? 'btn-primary' : 'btn-tertiary'"
                @click="chooseSegment(seg.index)"
              >
                {{ seg.label }}
              </button>
              <span class="small muted">共 {{ totalEpisodes }} 集 · 第 {{ segment + 1 }}/{{ segmentCount }} 段</span>
            </div>

            <div v-if="!seasonLoading && totalEpisodes" class="row wrap" style="gap: 8px; margin-bottom: 10px">
              <span class="small muted">跳到集号</span>
              <input
                v-model="jumpTo"
                class="input sub-jump-input"
                type="number"
                min="1"
                :max="totalEpisodes"
                placeholder="例如 60"
                @keydown.enter="jumpToEpisode"
              />
              <button class="btn btn-sm btn-tertiary" @click="jumpToEpisode">跳</button>
            </div>

            <div v-if="seasonLoading" class="muted small">正在读这一季的逐集播出日…</div>
            <div v-else class="ep-grid">
              <button
                v-for="n in segmentEpisodes"
                :key="n"
                class="ep-cell"
                :class="{ active: !fromBeginning && episode === n }"
                :title="airDateOf(n) ? `播出日 ${airDateOf(n)}` : 'TMDB 没给播出日'"
                @click="chooseEpisode(n)"
              >
                <strong>E{{ String(n).padStart(2, '0') }}</strong>
                <span class="muted tiny">{{ airDateOf(n) || '未定档' }}</span>
              </button>
            </div>
            <p v-if="!totalEpisodes" class="muted small">这一季 TMDB 没给集数，先选「从头追」。</p>
          </div>
        </template>
      </template>

      <div class="sub-foot">
        <div class="sub-foot-head">
          <div class="sub-foot-copy">
            <strong class="small">{{ startLabel }}</strong>
            <p class="muted small">
              {{ isTV ? '订阅后会按每集播出日搜资源；没搜到的集每天重试一次。' : '电影有资源就自动提交离线。' }}
            </p>
          </div>
          <button class="btn btn-primary" :disabled="saving || loading || (isTV && !fromBeginning && !episodeCountOf(season))" @click="submit">
            {{ saving ? '订阅中…' : '确认订阅' }}
          </button>
        </div>

        <!-- 选了具体集才展开详情；与「确认订阅」同处一张卡片，按钮始终在上方可见。 -->
        <div v-if="showEpPanel" class="ep-detail">
          <template v-if="epLoading">
            <div class="ep-detail-loading muted small">加载中…</div>
            <div class="ep-sk-line"></div>
            <div class="ep-sk-line short"></div>
          </template>
          <template v-else>
            <div class="ep-detail-head">
              <img
                v-if="epStill && !epStillFailed"
                class="ep-detail-still"
                :src="epStill"
                :alt="epTitle"
                loading="lazy"
                @error="onStillError"
              />
              <div class="ep-detail-info">
                <strong class="ep-detail-title">{{ epTitle }}</strong>
                <div class="ep-detail-meta muted small">
                  <span class="ep-code">{{ epCode }}</span>
                  <span v-if="epAirDate">· 播出 {{ epAirDate }}</span>
                  <span v-if="epRuntime > 0">· {{ epRuntime }} 分钟</span>
                  <span v-if="epRating > 0">· ★ {{ epRating.toFixed(1) }}</span>
                </div>
              </div>
            </div>
            <p class="ep-detail-overview" :class="{ 'is-placeholder': !epOverview }">
              {{ epOverview || epOverviewPlaceholder }}
            </p>
          </template>
        </div>
      </div>
      <p v-if="error" class="sub-err">{{ error }}</p>
    </div>
  </HModal>
</template>

<style scoped>
.sub-done {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 16px;
  border-radius: var(--r-md);
  border: 1px solid var(--border);
  background: var(--surface-secondary);
}
.sub-start,
.sub-pick,
.sub-foot {
  padding: 12px 14px;
  border-radius: var(--r-md);
  border: 1px solid var(--border);
  background: var(--surface-secondary);
}
.sub-foot {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.sub-foot-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.sub-foot-copy {
  flex: 1 1 240px;
  min-width: 0;
}
/* 单集详情：HModal 的 .modal-body 已经是主滚动容器，这里不做 max-height / overflow，
   避免把「确认订阅」按钮挤进双层滚动里看不见。 */
.ep-detail {
  border-top: 1px solid var(--border);
  padding-top: 12px;
}
.ep-detail-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
}
.ep-detail-still {
  display: block;
  flex: 0 0 auto;
  width: 108px;
  aspect-ratio: 16 / 9;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
  object-fit: cover;
}
.ep-detail-info {
  display: flex;
  flex: 1 1 auto;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.ep-detail-title {
  color: var(--foreground);
  font-size: 14px;
  line-height: 1.4;
}
.ep-detail-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 2px 6px;
  line-height: 1.5;
}
.ep-code {
  color: var(--accent);
  font-weight: 600;
}
.ep-detail-overview {
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 5;
  line-clamp: 5;
  overflow: hidden;
  margin: 10px 0 0;
  color: var(--foreground);
  font-size: 12.5px;
  line-height: 1.6;
}
.ep-detail-overview.is-placeholder {
  color: var(--muted);
  font-style: italic;
}
.ep-detail-loading {
  margin-bottom: 8px;
}
.ep-sk-line {
  height: 12px;
  margin-bottom: 8px;
  border-radius: 6px;
  background: linear-gradient(
    90deg,
    var(--surface-secondary) 20%,
    var(--surface-tertiary) 40%,
    var(--surface-secondary) 60%
  );
  background-size: 200% 100%;
  animation: ep-sk 1.2s ease-in-out infinite;
}
.ep-sk-line.short {
  width: 60%;
}
@keyframes ep-sk {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}
.ep-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(92px, 1fr));
  gap: 8px;
  /* 每段最多 50 格：放宽成一段短滚动。外层 .modal-body 才是主滚动容器，
     contain 防止网格滚到底又把整个弹窗带着滚（双层滚动）。 */
  max-height: 320px;
  overflow-y: auto;
  overscroll-behavior: contain;
}
.sub-jump-input {
  width: 104px;
  height: 28px;
  padding: 0 8px;
  font-size: 12.5px;
}
.ep-cell {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 2px;
  padding: 7px 9px;
  border-radius: var(--r-sm);
  border: 1px solid var(--border);
  background: var(--surface);
  color: var(--foreground);
  cursor: pointer;
  text-align: left;
}
.ep-cell:hover {
  border-color: var(--accent);
}
.ep-cell.active {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--accent);
}
.tiny {
  font-size: 10px;
}
.sub-err {
  color: var(--danger);
  font-size: 13px;
}
@media (max-width: 560px) {
  .ep-grid {
    grid-template-columns: repeat(auto-fill, minmax(76px, 1fr));
  }
  .ep-detail-still {
    width: 84px;
  }
  .ep-detail-overview {
    -webkit-line-clamp: 4;
    line-clamp: 4;
  }
}
</style>
