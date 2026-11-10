/**
 * 占位数据。
 *
 * 外壳阶段先用它把页面填满，方便对着真实产品调版式；
 * 接后端时把对应页面的 const 换成 api.xxx 的返回值即可（每个文件都留了 TODO）。
 */

export interface LibRow {
  id: number
  name: string
  account: string
  remote: string
  local: string
  items: number
  strm: number
  status: 'ok' | 'syncing' | 'warn'
  lastSync: string
}

export const libraries: LibRow[] = [
  {
    id: 1,
    name: '电影',
    account: '主号 · 115',
    remote: '/影视/电影',
    local: '/media/movies/电影',
    items: 1284,
    strm: 1284,
    status: 'ok',
    lastSync: '3 分钟前',
  },
  {
    id: 2,
    name: '剧集',
    account: '主号 · 115',
    remote: '/影视/剧集',
    local: '/media/tv/剧集',
    items: 3261,
    strm: 3258,
    status: 'syncing',
    lastSync: '正在跑',
  },
  {
    id: 3,
    name: '动漫',
    account: '副号 · 115',
    remote: '/影视/动漫',
    local: '/media/anime/动漫',
    items: 742,
    strm: 742,
    status: 'ok',
    lastSync: '26 分钟前',
  },
  {
    id: 4,
    name: '纪录片',
    account: '副号 · 115',
    remote: '/影视/纪录片',
    local: '/media/doc/纪录片',
    items: 118,
    strm: 96,
    status: 'warn',
    lastSync: '昨天 23:10',
  },
]

export interface PosterRow {
  id: number
  title: string
  year: number
  rating: number
  kind: 'movie' | 'tv'
  genre: string
  hue: number
  tag?: string
}

export const posters: PosterRow[] = [
  { id: 1, title: '沙丘 2', year: 2024, rating: 8.2, kind: 'movie', genre: '科幻', hue: 32 },
  { id: 2, title: '繁花', year: 2023, rating: 8.5, kind: 'tv', genre: '剧情', hue: 12 },
  { id: 3, title: '奥本海默', year: 2023, rating: 8.6, kind: 'movie', genre: '传记', hue: 200 },
  { id: 4, title: '三体', year: 2023, rating: 8.7, kind: 'tv', genre: '科幻', hue: 220 },
  { id: 5, title: '蜘蛛侠：纵横宇宙', year: 2023, rating: 8.4, kind: 'movie', genre: '动画', hue: 300 },
  { id: 6, title: '琅琊榜', year: 2015, rating: 9.4, kind: 'tv', genre: '古装', hue: 44 },
  { id: 7, title: '狂飙', year: 2023, rating: 8.5, kind: 'tv', genre: '犯罪', hue: 350 },
  { id: 8, title: '流浪地球 2', year: 2023, rating: 8.3, kind: 'movie', genre: '科幻', hue: 190 },
  { id: 9, title: '周处除三害', year: 2023, rating: 8.1, kind: 'movie', genre: '动作', hue: 0 },
  { id: 10, title: '漫长的季节', year: 2023, rating: 9.4, kind: 'tv', genre: '悬疑', hue: 96 },
  { id: 11, title: '隐入尘烟', year: 2022, rating: 8.5, kind: 'movie', genre: '剧情', hue: 60 },
  { id: 12, title: '灵笼', year: 2019, rating: 8.2, kind: 'tv', genre: '动画', hue: 260 },
  { id: 13, title: '封神第一部', year: 2023, rating: 7.8, kind: 'movie', genre: '奇幻', hue: 24 },
  { id: 14, title: '隐秘的角落', year: 2020, rating: 8.8, kind: 'tv', genre: '悬疑', hue: 170 },
  { id: 15, title: '疾速追杀 4', year: 2023, rating: 7.8, kind: 'movie', genre: '动作', hue: 14 },
  { id: 16, title: '重启人生', year: 2023, rating: 8.9, kind: 'tv', genre: '喜剧', hue: 340 },
  { id: 17, title: '铃芽之旅', year: 2022, rating: 7.4, kind: 'movie', genre: '动画', hue: 280 },
  { id: 18, title: '孤注一掷', year: 2023, rating: 6.9, kind: 'movie', genre: '犯罪', hue: 8 },
]

// 订阅的假数据已经删掉：MediaTransferPage 现在直接打 /api/subscriptions，
// 留一份写死的「沙丘 2 / 繁花 / 三体」只会让下一个人以为还有个 mock 分支。

export const logs: { time: string; level: 'info' | 'warn' | 'error' | 'debug'; msg: string }[] = [
  { time: '21:52:04', level: 'info', msg: 'sync: 剧集 全量同步 2024/3261，写入 STRM 12 个' },
  { time: '21:51:58', level: 'debug', msg: '115: list /影视/剧集/繁花 (2023) cid=2849912 返回 30 项' },
  { time: '21:51:41', level: 'warn', msg: 'emby: 探测超时，5s 后重试（第 2 次）' },
  { time: '21:51:02', level: 'info', msg: 'organize: 识别 沙丘2 (2024) 匹配 TMDB 693134 置信度 96' },
  { time: '21:50:47', level: 'info', msg: 'scrape: 写入 NFO movie/沙丘2 (2024)/movie.nfo' },
  { time: '21:50:12', level: 'error', msg: '115: 429 Too Many Requests，退避 30s 后继续' },
  { time: '21:49:33', level: 'debug', msg: 'tmdb: discover list=popular 抓到 20 条' },
  { time: '21:48:50', level: 'info', msg: 'webhook: Emby 删除事件 沙丘2 → 深度删除已入队' },
]
