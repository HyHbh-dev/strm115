/**
 * 资源搜索的共用逻辑。
 *
 * 这个项目真正的「找片」能力来自 internal/resource：TG 公开频道 + 种子站并发搜，
 * 中文片名会用 TMDB（退回 IMDb）换英文名，种子站和 TG 频道都按英文名再搜一遍。
 * 电影墙详情、资源搜索页两处都用它，所以状态、来源筛选与体积筛选放这里，
 * 页面只管画。
 *
 * 关键约定：一次搜索后端**把所有源都搜回来**，来源切换（全部 / 种子站 / TG）
 * 只在这里做本地过滤，绝不重新发请求 —— 以前点一下「种子站」就重搜一遍，
 * 六个种子站 + 三个频道全部重打，t.me 还容易被限流成超时。
 */

import { computed, ref, watch } from 'vue'
import { api } from '@/api/endpoints'

export interface ResItem {
  title: string
  source: string
  source_key: string
  kind: 'tg' | 'torrent' | string
  magnet?: string
  url?: string
  torrent?: string
  size?: number
  seeders?: number
  leechers?: number
  date?: string
  snippet?: string
  links?: string[]
  category?: string
  /**
   * 115 分享：后端从 TG 帖子里正则认出来的分享码 + 提取码。
   * 有它就能直接「存到 115」，不用点开 t.me 再复制粘贴。
   */
  share_code?: string
  share_pwd?: string
  share_url?: string
  share_host?: string
  /**
   * 夸克分享：和 115 分享是两条链路。夸克走「网页 Cookie 转存进夸克网盘」，
   * 播放再由夸克TV 的 302 直链负责，不经过 115。
   */
  quark_share?: string
  quark_pwd?: string
  quark_url?: string
}

export interface ResSource {
  key: string
  name: string
  kind: string
  ok: boolean
  count: number
  ms: number
  error?: string
  /** 这一轮复用了后端缓存，没有真的连这个源 */
  cached?: boolean
}

export interface ResResult {
  items: ResItem[]
  sources: ResSource[]
  query: string
  site_query?: string
  /** TG 频道额外用的英文词（TMDB 换来的） */
  channel_query?: string
  /** domestic = 豆瓣认出是国产内容，故意不换英文名 */
  en_lookup?: '' | 'ok' | 'fail' | 'domestic'
  took_ms: number
  /** 整轮全部命中缓存 */
  cached?: boolean
  error?: string
  /** 被后端当成广告跳过的条数 */
  filtered?: number
}

/** 体积档位：和旧版界面共用同一组阈值，0 表示不限 */
export const SIZE_FILTERS = [
  { v: 0, l: '不限' },
  { v: 1, l: '≥1 GB' },
  { v: 3, l: '≥3 GB' },
  { v: 5, l: '≥5 GB' },
  { v: 10, l: '≥10 GB' },
  { v: 20, l: '≥20 GB' },
]

/** 来源档位。v 只用于本地过滤，不再发给后端。 */
export const KIND_FILTERS = [
  { v: '', l: '全部来源' },
  { v: 'torrent', l: '种子站' },
  { v: 'tg', l: 'TG 频道' },
]

// 保持对外导出名 fmtSize 不变（日历 / 资源搜索 / 海报墙都从本模块 import）。
export { fmtSize } from '@/utils/format'

/** 只过一遍已有结果，不重新联网；没有体积信息的条目保留（宁多不漏） */
export function filterBySize(items: ResItem[], minGB: number): ResItem[] {
  if (!minGB) return items
  const min = minGB * 1024 ** 3
  return items.filter((it) => !it.size || it.size >= min)
}

/** 来源筛选也是纯本地：结果里每条都带 kind，后端已经把两类源都搜回来了 */
export function filterByKind(items: ResItem[], kind: string): ResItem[] {
  if (!kind) return items
  if (kind === 'tg') return items.filter((it) => it.kind === 'tg')
  if (kind === 'torrent') return items.filter((it) => it.kind !== 'tg')
  return items
}

export function useResSearch() {
  const loading = ref(false)
  const error = ref('')
  const result = ref<ResResult | null>(null)
  const minSize = ref(0)
  const kind = ref('')

  /** 一次只显示 5 条：结果动辄几十条，一屏全铺开反而找不到重点。 */
  const PAGE_SIZE = 5
  const page = ref(1)

  const items = computed(() =>
    filterBySize(filterByKind(result.value?.items ?? [], kind.value), minSize.value),
  )
  const totalPages = computed(() => Math.max(1, Math.ceil(items.value.length / PAGE_SIZE)))
  const pagedItems = computed(() => {
    const p = Math.min(page.value, totalPages.value)
    const from = (p - 1) * PAGE_SIZE
    return items.value.slice(from, from + PAGE_SIZE)
  })
  /** 过滤后总数变了（切来源 / 调体积）要回到第一页，否则可能停在不存在的页。 */
  watch([items, kind, minSize], () => {
    page.value = 1
  })
  function nextPage() {
    if (page.value < totalPages.value) page.value += 1
  }
  function prevPage() {
    if (page.value > 1) page.value -= 1
  }

  /** 中文换英文这一步的结果，直接说人话，别让用户猜为什么种子站一条都没有 */
  const lookupNote = computed(() => {
    const r = result.value
    if (!r) return ''
    if (r.en_lookup === 'ok' && r.site_query) {
      return r.channel_query
        ? `种子站按「${r.site_query}」· TG 频道也搜了「${r.channel_query}」`
        : `种子站按「${r.site_query}」搜`
    }
    if (r.en_lookup === 'domestic') return '豆瓣认出是国产片 · 按中文名直搜，不用等 TMDB 换名'
    if (r.en_lookup === 'fail') return '没换成英文名，种子站可能搜不到，建议手动输入英文片名'
    return ''
  })

  /** 缓存命中要显式说明，否则 0ms 会被当成「某个站特别快」 */
  const cachedNote = computed(() => (result.value?.cached ? '本轮复用缓存 · 点重搜可强制刷新' : ''))

  /**
   * 发起一轮搜索。refresh=true 才让后端跳过缓存；
   * 切换来源或体积筛选请用 kind / minSize，不要调这里。
   */
  async function run(params: { q: string; en?: string; limit?: number; refresh?: boolean }) {
    const q = params.q.trim()
    if (!q) {
      error.value = '先输入要搜的片名'
      return
    }
    loading.value = true
    error.value = ''
    try {
      const query: Record<string, string | number> = { q, limit: params.limit ?? 30 }
      if (params.en?.trim()) query.en = params.en.trim()
      if (params.refresh) query.refresh = 1
      const r = await api.transfer.resSearch(query)
      result.value = r as ResResult
      page.value = 1
      if (r?.error) error.value = r.error
    } catch (e: any) {
      result.value = null
      error.value = e?.message || '搜索失败'
    } finally {
      loading.value = false
    }
  }

  function reset() {
    result.value = null
    error.value = ''
    kind.value = ''
    minSize.value = 0
    page.value = 1
  }

  return {
    loading, error, result, items, pagedItems, page, pageSize: PAGE_SIZE, totalPages,
    nextPage, prevPage, minSize, kind, lookupNote, cachedNote, run, reset,
  }
}
