<script setup lang="ts">
/**
 * 片名链接 —— 全站「点影视名称看详情」的统一入口。
 *
 * 为什么不用 <a>：首页最近入库的卡片本身包在 RouterLink 里，a 套 a 是非法
 * HTML；海报墙的卡片是 <button>，button 里塞交互元素同样不行。用 span +
 * role=link + tabindex，放到任何父容器里都不会破坏结构。
 *
 * 手里有什么标识就传什么，优先级 mediaId > tmdbId > 片名+年份。只给片名时
 * 后端会先按名字问 TMDB 换 id（整理记录、最近入库这类页面只有名字）。
 */
import { computed } from 'vue'
import { openDetail } from '@/stores/detail'

const props = defineProps<{
  /** tv / series / movie / film 都能认；认不出来按剧集处理 */
  type?: string
  tmdbId?: number | string
  mediaId?: number | string
  /** 显示用的名字，同时也是「只给片名」时的查询词 */
  title?: string
  year?: string | number
  /** 想用别的名字查 TMDB（比如显示的是中文译名、要查原名）时单独给 */
  query?: string
  /** 卡片里点标题不要连带打开卡片自己的抽屉。默认 true */
  stop?: boolean
}>()

const tmdb = computed(() => Number(props.tmdbId) || 0)
const media = computed(() => Number(props.mediaId) || 0)
const name = computed(() => (props.query ?? props.title ?? '').trim())
const canOpen = computed(() => tmdb.value > 0 || media.value > 0 || !!name.value)

function kind(): 'tv' | 'movie' | undefined {
  const t = (props.type || '').toLowerCase()
  if (t === 'movie' || t === 'film') return 'movie'
  if (t === 'tv' || t === 'series' || t === 'show' || t === 'episode') return 'tv'
  return undefined
}

function go(e: MouseEvent | KeyboardEvent) {
  if (!canOpen.value) return
  if (props.stop !== false) e.stopPropagation()
  e.preventDefault()
  openDetail({
    type: kind(),
    tmdbId: tmdb.value || undefined,
    mediaId: media.value || undefined,
    query: !tmdb.value && !media.value ? name.value : undefined,
    year: props.year,
    title: props.title || name.value,
  })
}
</script>

<template>
  <span
    v-if="canOpen"
    class="title-link"
    role="link"
    tabindex="0"
    :title="`查看《${title || name}》详情`"
    @click="go"
    @keydown.enter="go"
  >
    <slot />
  </span>
  <span v-else><slot /></span>
</template>

<style scoped>
/* 看起来是普通文字，鼠标移上去才知道能点 —— 列表里几十个片名，
   全画成蓝色链接会晃眼。 */
.title-link {
  font: inherit;
  font-weight: 600;
  color: inherit;
  cursor: pointer;
  border-radius: 3px;
}
.title-link:hover,
.title-link:focus-visible {
  color: var(--accent);
  text-decoration: underline;
  text-underline-offset: 2px;
  outline: none;
}
</style>
