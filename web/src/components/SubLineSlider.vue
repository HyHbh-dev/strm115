<script setup lang="ts">
/**
 * 起播线滑杆 —— 全站唯一一份「像调音量那样拖到哪一集」的实现。
 *
 * 水位线和限制线合并之后，这条线只表达一件事：**从这一集开始追（含这一集）**，
 * 之前的集不找资源。
 * 程序提交离线成功会自己往前推，用户也可以在这里把线拖到任意位置。
 *
 * 为什么抽出来：同一个滑杆要在三个地方出现，而且语义必须完全一致 ——
 *   1. 影视详情抽屉（MediaDetailDrawer，有分季集数，最准）
 *   2. 「发现与订阅 → 订阅设置」里每条剧集订阅一行一个
 *   3. 「播出日历 → 我的订阅 → 本集详情」抽屉
 * 之前只有第 1 处是滑杆，第 2 处还是两个 S/E 数字框、第 3 处干脆只读，
 * 复制三份换算逻辑迟早会漂。
 *
 * 滑杆的值是「全剧连续编号」的第几集：把各季集数依次累加，0 = 从头追。
 * 有多季时也只是一条直线，不用先选季再选集。
 */
import { computed, ref, watch } from 'vue'
import { api } from '@/api/endpoints'
import { loadSubsIndex } from '@/stores/subscriptions'
import { toast } from '@/stores/toast'
import { pad2 } from '@/utils/format'

export interface SeasonLen {
  season: number
  episodes: number
}

const props = defineProps<{
  /** 订阅 id；保存时 PUT /api/subscriptions/{id} */
  subId: number
  /** 服务端当前的起播线 */
  season?: number
  episode?: number
  /** 没有分季集数时，用它撑滑杆长度 */
  totalEpisodes?: number
  /** TMDB /tv/{id} 给的每季集数；给了才能跨季拖 */
  seasonEpisodes?: SeasonLen[]
  /** 往回拖时的确认框里显示片名 */
  title?: string
  /** 订阅设置列表里用紧凑排版（少一行说明） */
  compact?: boolean
}>()

const emit = defineEmits<{
  saved: [line: { season: number; episode: number }]
}>()

const idx = ref(0)
const saving = ref(false)

const seasonLens = computed<SeasonLen[]>(() => {
  const raw = (props.seasonEpisodes || []).filter((s) => s.episodes > 0)
  if (raw.length) return [...raw].sort((a, b) => a.season - b.season)
  // 老剧 / 刚定档的新季 / 订阅列表里没有分季数据：退成「一季」。
  // 长度取总集数、当前起播线里最大的那个，宁可滑杆长一点也别让人拖不到
  // 自己现在那条线。
  const n = Math.max(props.totalEpisodes || 0, props.episode || 0, 1)
  return [{ season: Math.max(1, props.season || 1), episodes: n }]
})

const max = computed(() => seasonLens.value.reduce((sum, s) => sum + s.episodes, 0))

/** 连续编号 → 季/集；超出范围停在最后一集。 */
function episodeAt(n: number): { season: number; episode: number } {
  let left = Math.max(0, n)
  for (const s of seasonLens.value) {
    if (left <= s.episodes) return { season: s.season, episode: left }
    left -= s.episodes
  }
  const last = seasonLens.value[seasonLens.value.length - 1]
  return { season: last?.season || 1, episode: last?.episodes || 0 }
}

/** 季/集 → 连续编号；季号对不上（TMDB 改过分季）就从 0 起显示。 */
function indexOf(season: number, episode: number): number {
  let base = 0
  for (const s of seasonLens.value) {
    if (s.season === season) return base + Math.min(Math.max(0, episode), s.episodes)
    base += s.episodes
  }
  return 0
}

/** 某一季末集的编号，给 S01 / S02 快捷按钮用。 */
function seasonEnd(season: number): number {
  let base = 0
  for (const s of seasonLens.value) {
    base += s.episodes
    if (s.season === season) return base
  }
  return base
}

const nowIdx = computed(() => indexOf(props.season || 0, props.episode || 0))
const ep = computed(() => episodeAt(idx.value))
const dirty = computed(() => idx.value !== nowIdx.value)
const backward = computed(() => idx.value < nowIdx.value)

function label(e: { season: number; episode: number }): string {
  return e.episode ? `S${pad2(e.season)}E${pad2(e.episode)}` : '从头追'
}
const nowLabel = computed(() => label(episodeAt(nowIdx.value)))

// 外部数据变了（换片、订阅列表刷新、保存成功后父级回填）就把滑杆对回去。
// 保存成功时也走这里：dirty 归零，按钮自然变回「和当前一致」。
watch(
  [() => props.subId, () => props.season, () => props.episode, () => props.totalEpisodes, seasonLens],
  () => {
    idx.value = nowIdx.value
  },
  { immediate: true },
)

async function save() {
  if (!props.subId || saving.value) return
  if (!dirty.value) {
    toast.info('没有变化')
    return
  }
  const e = ep.value
  // 往回退 = 已经交过的集会被重新搜索、重新提交离线，花配额，先问一句。
  if (
    backward.value &&
    !window.confirm(
      `把「${props.title || '这部剧'}」的起播线从 ${nowLabel.value} 退回 ${label(e)}？\n\n` +
        '退回去的集会被重新搜索、重新提交离线。',
    )
  ) {
    return
  }
  saving.value = true
  let ok = false
  try {
    await api.subscriptions.update(props.subId, {
      watermark_season: e.season,
      watermark_episode: e.episode,
    })
    ok = true
    toast.success(
      '起播线已保存',
      e.episode ? `从 ${label(e)} 开始找（含这一集）` : '从第一集开始找',
    )
    emit('saved', e)
    // 共享订阅索引里有整行（水位线 / 总集数），日历抽屉靠它显示。
    // 保存完立刻刷新，不然那边还停在旧值，要等下次进页面才对。
    void loadSubsIndex(true)
  } catch (err: any) {
    toast.warning('保存失败', err?.message || '稍后再试')
  } finally {
    saving.value = false
    // 请求失败时把滑杆弹回服务端那条线，别留一个「看起来改了其实没改」的状态。
    // 成功时不弹：父级回填 props 有一点延迟，弹回去会闪一下。
    if (!ok && dirty.value) idx.value = nowIdx.value
  }
}
</script>

<template>
  <div class="sub-line" :class="{ 'is-compact': compact }">
    <div class="sub-line-head">
      <span class="muted small">起播线</span>
      <b class="mono">{{ label(ep) }}</b>
      <span class="muted small">{{ ep.episode ? '起（含这一集）' : '不设线，从第一集开始' }}</span>
    </div>
    <input
      v-model.number="idx"
      class="sub-range"
      type="range"
      min="0"
      :max="max"
      step="1"
      aria-label="起播线"
    />
    <div class="sub-range-scale">
      <span class="muted small">从头</span>
      <span class="muted small">第 {{ idx }} / {{ max }} 集</span>
    </div>
    <div class="row wrap sub-shortcuts">
      <button class="btn btn-sm btn-ghost" @click="idx = 0">从头追</button>
      <button
        v-for="s in seasonLens"
        :key="s.season"
        class="btn btn-sm btn-ghost"
        :title="`滑到 S${pad2(s.season)} 末集：从这一集开始追（含）`"
        @click="idx = seasonEnd(s.season)"
      >
        S{{ pad2(s.season) }}
      </button>
    </div>
    <div class="row-between sub-line-foot">
      <span class="muted small">
        {{
          !dirty
            ? '和当前一致'
            : backward
              ? '往回拖会重新找已经交过的集'
              : '会跳过前面这些集'
        }}
      </span>
      <button
        class="btn btn-sm"
        :class="{ 'btn-primary': dirty }"
        :disabled="!dirty || saving"
        @click="save"
      >
        {{ saving ? '保存中…' : '保存起播线' }}
      </button>
    </div>
    <p v-if="!compact" class="muted small sub-line-note">
      自动提交成功后这条线会自己往前推，不用手动维护。
    </p>
  </div>
</template>

<style scoped>
/* 一条滑杆代替原来的「水位线 + 限制线」两行数字 */
.sub-line {
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px dashed var(--separator);
}
.sub-line.is-compact {
  margin-top: 0;
  padding-top: 0;
  border-top: none;
  width: 100%;
}
.sub-line-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-bottom: 4px;
}
.sub-range {
  -webkit-appearance: none;
  appearance: none;
  display: block;
  width: 100%;
  height: 22px;
  margin: 0;
  background: transparent;
  cursor: pointer;
}
.sub-range:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 4px;
  border-radius: 999px;
}
.sub-range::-webkit-slider-runnable-track {
  height: 4px;
  border-radius: 999px;
  background: var(--border);
}
.sub-range::-webkit-slider-thumb {
  -webkit-appearance: none;
  appearance: none;
  width: 16px;
  height: 16px;
  margin-top: -6px;
  border-radius: 50%;
  background: var(--accent);
  border: 2px solid var(--surface-secondary);
  box-shadow: 0 1px 4px oklch(0% 0 0 / 0.35);
}
.sub-range::-moz-range-track {
  height: 4px;
  border-radius: 999px;
  background: var(--border);
}
.sub-range::-moz-range-thumb {
  width: 14px;
  height: 14px;
  border-radius: 50%;
  background: var(--accent);
  border: 2px solid var(--surface-secondary);
}
.sub-range-scale {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  margin-top: 2px;
}
.sub-shortcuts {
  gap: 6px;
  margin-top: 8px;
}
.sub-line-foot {
  margin-top: 10px;
  gap: 10px;
}
.sub-line-note {
  margin-top: 8px;
}
.is-compact .sub-shortcuts {
  margin-top: 4px;
}
.is-compact .sub-line-foot {
  margin-top: 6px;
}
</style>
