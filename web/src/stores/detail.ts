/**
 * 全局「影视详情」开关。
 *
 * 以前详情抽屉是三份各写各的：海报墙一份（WallBrowser）、台账一份
 * （LibraryWall）、日历一份（cal-drawer）。订阅列表里的片名干脆没得点 ——
 * 那一页手里只有 tmdb_id，拿不到 catalog.item 要求的 emby:/local: key。
 *
 * 现在统一成一个抽屉，任何页面调 openDetail() 就能开。目标只用「手上有的
 * 最小标识」表达：有本地台账行就给 mediaId，没有就给 type + tmdbId。
 */
import { ref } from 'vue'

export interface DetailTarget {
  /** tv / movie。只给 mediaId 时可以不填，后端会从台账行里推。 */
  type?: 'tv' | 'movie'
  tmdbId?: number
  /** media_meta 的主键，有就最准（能直接定位到那一集）。 */
  mediaId?: number
  /** 只有片名、没有 id 时用（首页最近入库、整理记录）：服务端按片名问 TMDB。 */
  query?: string
  year?: string | number
  /** 接口回来之前先拿它占位，免得抽屉标题空白闪一下。 */
  title?: string
}

export const detailTarget = ref<DetailTarget | null>(null)

export function openDetail(t: DetailTarget) {
  detailTarget.value = { ...t }
}

export function closeDetail() {
  detailTarget.value = null
}
