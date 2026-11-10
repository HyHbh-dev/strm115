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

const props = defineProps<{ modelValue: boolean; hit: SubscribeHit | null }>()
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

const isTV = computed(() => props.hit?.kind === 'tv')
const hasTMDB = computed(() => !!props.hit && props.hit.source !== 'douban' && props.hit.id > 0)
const already = computed(() => {
  void subsVersion.value
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
  fromBeginning.value = false
  await loadEpisodes(s)
}

function chooseEpisode(e: number) {
  episode.value = e
  fromBeginning.value = false
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

            <div v-if="seasonLoading" class="muted small">正在读这一季的逐集播出日…</div>
            <div v-else class="ep-grid">
              <button
                v-for="n in episodeCountOf(season)"
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
            <p v-if="!episodeCountOf(season)" class="muted small">这一季 TMDB 没给集数，先选「从头追」。</p>
          </div>
        </template>
      </template>

      <div class="sub-foot">
        <div>
          <strong class="small">{{ startLabel }}</strong>
          <p class="muted small">
            {{ isTV ? '订阅后会按每集播出日搜资源；没搜到的集每天重试一次。' : '电影有资源就自动提交离线。' }}
          </p>
        </div>
        <button class="btn btn-primary" :disabled="saving || loading || (isTV && !fromBeginning && !episodeCountOf(season))" @click="submit">
          {{ saving ? '订阅中…' : '确认订阅' }}
        </button>
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
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.ep-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(92px, 1fr));
  gap: 8px;
  max-height: 230px;
  overflow-y: auto;
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
}
</style>
