/**
 * 「这部剧我订过没有」的唯一答案。
 *
 * 墙上和日历里都有「订阅」按钮，但它们以前完全不看订阅表：
 *  - 墙上订完之后按钮还是「订阅」，再点只会吃到 409「已经在订阅里了」；
 *  - 日历里订完直接把按钮 **disabled** 成「已订阅」，用户点它毫无反应
 *    —— 报「点了取消不了」的就是这里。
 *
 * 所以缓存一份 `类型:tmdb_id → 订阅 id` 的索引，两个入口共用：
 * 有 id 就说明订过了，按钮变成「取消订阅」，点下去真的 DELETE。
 *
 * 除了 id，还顺手留一份整行（水位线 / 限制线 / 总集数）。播出日历的
 * 本集详情要显示「已交到第几集」，总不能每点一张卡片都再打一次接口。
 *
 * 为什么不每次都问后端：这两个按钮在榜单/日历里会渲染几十次，
 * 一次列表请求换一份索引，比每个按钮各查一次便宜得多。
 */
import { ref } from 'vue'
import { api } from '@/api/endpoints'

/** 键：`tv:1396` / `movie:693134`。电影和剧的 tmdb_id 会撞号，必须带类型。 */
export function subKey(type: string | undefined, tmdbId: number | string | undefined): string {
  if (tmdbId === undefined || tmdbId === null || tmdbId === '') return ''
  const t = type === 'movie' ? 'movie' : 'tv'
  return `${t}:${tmdbId}`
}

/** 订阅列表里一行的字段（只列页面用得上的，接口多给的忽略）。 */
export interface SubRow {
  id: number
  media_type: string
  tmdb_id: number
  title: string
  state: string
  quality: string
  watermark_season: number
  watermark_episode: number
  total_episodes: number
  last_result?: string
}

const index = ref<Record<string, number>>({})
const rows = ref<Record<string, SubRow>>({})
let loaded = false
let inflight: Promise<void> | null = null

/**
 * 订阅集合的版本号。每次本地增删都 +1。
 *
 * 索引只记「id」，而订阅页签的表格要的是整行（状态、水位线、最近一轮）。
 * 所以在详情抽屉里点完订阅，那一页得知道自己该重拉 —— 光靠索引变化
 * 是看不出来的（按钮会变，表格不会）。
 */
export const subsVersion = ref(0)

/** 订阅 id，0 表示没订过。 */
export function subIdOf(type: string | undefined, tmdbId: number | string | undefined): number {
  return index.value[subKey(type, tmdbId)] ?? 0
}

/** 整行订阅信息；没订过返回 null。日历详情靠它显示水位线 / 限制线。 */
export function subRowOf(
  type: string | undefined,
  tmdbId: number | string | undefined,
): SubRow | null {
  const k = subKey(type, tmdbId)
  return k ? rows.value[k] ?? null : null
}

/**
 * 拉一次订阅列表建索引。force 用于建/删之后对齐（比如另一个标签页动过）。
 * 失败就当「都没订过」：点订阅会走 409 提示，比把按钮全锁死好。
 */
export async function loadSubsIndex(force = false): Promise<void> {
  if (loaded && !force) return
  if (inflight) return inflight
  inflight = (async () => {
    try {
      const r: any = await api.subscriptions.list()
      const next: Record<string, number> = {}
      const nextRows: Record<string, SubRow> = {}
      for (const s of r?.items ?? []) {
        const k = subKey(s.media_type, s.tmdb_id)
        if (!k) continue
        next[k] = s.id
        nextRows[k] = s as SubRow
      }
      index.value = next
      rows.value = nextRows
      loaded = true
    } catch {
      /* 见函数注释 */
    } finally {
      inflight = null
    }
  })()
  return inflight
}

/** 刚订阅成功：本地先记上，按钮立刻变「取消订阅」，不用等下一次列表请求。 */
export function rememberSub(type: string | undefined, tmdbId: number | string | undefined, id: number) {
  const k = subKey(type, tmdbId)
  if (!k || !id) return
  // 详情抽屉每次打开都会拿接口的权威答案来对齐索引。已经一致时别 bump
  // 版本号 —— 否则「点开看看」会让订阅页签白白重拉一次列表。
  if (index.value[k] === id) return
  index.value = { ...index.value, [k]: id }
  // 只先放一个最小占位，完整行由随后那次 loadSubsIndex(true) 补齐。
  rows.value = {
    ...rows.value,
    [k]: {
      id,
      media_type: type === 'movie' ? 'movie' : 'tv',
      tmdb_id: Number(tmdbId) || 0,
      title: '',
      state: 'watching',
      quality: '1080p',
      watermark_season: 0,
      watermark_episode: 0,
      total_episodes: 0,
    },
  }
  subsVersion.value++
}

/** 取消订阅成功（或别处删掉了）：抹掉这一条。 */
export function forgetSub(type: string | undefined, tmdbId: number | string | undefined) {
  const k = subKey(type, tmdbId)
  if (!k || !(k in index.value)) return
  const next = { ...index.value }
  delete next[k]
  index.value = next
  const nextRows = { ...rows.value }
  delete nextRows[k]
  rows.value = nextRows
  subsVersion.value++
}
