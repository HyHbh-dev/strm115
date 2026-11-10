<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HField from '@/components/HField.vue'
import HPanel from '@/components/HPanel.vue'
import HTabs from '@/components/HTabs.vue'
import HSwitch from '@/components/HSwitch.vue'
import { api } from '@/api/endpoints'
import { session } from '@/stores/session'
import { toast } from '@/stores/toast'
import { copyText } from '@/utils/format'

/** 「获取地址」：官方申请页 / 文档，点得开也复制得了（有些环境禁止跳转外链）。 */
interface ApiLink {
  label: string
  url: string
  note: string
}
const tmdbLinks: ApiLink[] = [
  {
    label: '申请 API Key',
    url: 'https://www.themoviedb.org/settings/api',
    note: '登录 → 账户设置 → API → Create；需先同意使用条款',
  },
  { label: '注册账号', url: 'https://www.themoviedb.org/signup', note: '还没有 TMDB 账号从这里开始（免费）' },
  { label: '接口文档', url: 'https://developer.themoviedb.org/docs', note: 'v3 参考；本项目用的是 v3 Key' },
  {
    label: '自建反代项目',
    url: 'https://github.com/imaliang/tmdb-proxy',
    note: '大陆直连超时时用，部署完把 /3 填到下面的「接口根」',
  },
]
const tvdbLinks: ApiLink[] = [
  {
    label: '申请 v4 API Key',
    url: 'https://thetvdb.com/dashboard/account/apikeys',
    note: '登录 → Dashboard → Account → API Keys → Create a v4 API Key',
  },
  {
    label: '申请说明',
    url: 'https://support.thetvdb.com/kb/faq.php?id=81',
    note: 'Key 按项目申请，选「End-User Subscriptions」才会需要 PIN',
  },
  { label: '订阅 / 获取 PIN', url: 'https://thetvdb.com/subscribe', note: '用户支持型 Key 的 Subscriber PIN 在这里拿' },
  {
    label: 'v4 接口文档',
    url: 'https://thetvdb.github.io/v4-api/',
    note: 'POST /login 换 Token；GET /series/{id}/extended 取播出时刻',
  },
]
const tvmazeLinks: ApiLink[] = [
  {
    label: 'TVmaze 官网',
    url: 'https://www.tvmaze.com',
    note: '免注册、免 API Key；数据 CC BY-SA，日历页会自动带署名链接',
  },
  {
    label: 'API 文档',
    url: 'https://www.tvmaze.com/api',
    note: '本项目用 /schedule 与 /schedule/web 两条排期接口，限流 20 次 / 10 秒',
  },
]

// 模板 @click 直接绑 copyAddr，保留名字；剪贴板与提示统一走 copyText。
async function copyAddr(url: string) {
  await copyText(url, '地址已复制')
}

const tab = ref('tmdb')
const tabs = [
  { value: 'tmdb', label: '电影数据源' },
  { value: 'proxy', label: '代理配置' },
  { value: 'emby', label: 'EMBY 管理' },
  { value: 'security', label: '登录安全' },
  { value: 'backup', label: '备份恢复' },
  { value: 'update', label: '版本更新' },
]

/* ---- 电影数据源：TMDB Key + IMDb Key，都真的存进设置项 ---- */
const tmdb = ref({ key: '', imdbKey: '', base: '', imageBase: '' })
// 反代地址不是密钥：留空保存 = 清空。存一份「已保存值」用来比对，
// 免得只改 IMDb Key 时把反代地址顺手冲掉。
const savedTmdbBase = ref('')
const savedTmdbImageBase = ref('')
const tmdbUpstream = ref<{ api: string; image: string }>({ api: '', image: '' })
const tmdbKeySet = ref(false)
const imdbKeySet = ref(false)

const tmdbChecking = ref(false)
const tmdbState = ref<'idle' | 'ok' | 'fail'>('idle')
const tmdbMsg = ref('')

const imdbChecking = ref(false)
const imdbState = ref<'idle' | 'ok' | 'warn' | 'fail'>('idle')
const imdbMsg = ref('')

const saving = ref(false)

/* ---- TVDB：播出日历的增强数据源 ----
   TMDB 的分集只有日期没有时刻，电视网也不全；TVDB v4 的 airsTime /
   originalNetwork / episodes[].aired 正好补上。它是可选项：不填日历照常用。 */
const tvdb = ref({ key: '', pin: '', base: '' })
const tvdbKeySet = ref(false)
const tvdbPinSet = ref(false)
// 接口根不是密钥：留空保存 = 恢复官方。存一份「已保存值」用来判断有没有改动。
const savedTvdbBase = ref('')
const tvdbUpstream = ref<{ api: string }>({ api: '' })
const tvdbChecking = ref(false)
const tvdbState = ref<'idle' | 'ok' | 'fail'>('idle')
const tvdbMsg = ref('')

/* ---- 播出日历时刻源 ----
   TheTVDB v4 已不再向个人免费发项目 Key，默认改用免 Key 的 TVMaze；
   TVDB 配置完整保留，拿到授权的用户切回去就能用。 */
const calendarSource = ref<'tvmaze' | 'tvdb' | 'off'>('tvmaze')
const savedCalendarSource = ref<'tvmaze' | 'tvdb' | 'off'>('tvmaze')
const tvmaze = ref({ base: '' })
const savedTVMazeBase = ref('')
const tvmazeUpstream = ref<{ api: string }>({ api: '' })
const tvmazeChecking = ref(false)
const tvmazeState = ref<'idle' | 'ok' | 'fail'>('idle')
const tvmazeMsg = ref('')

async function checkTVMaze() {
  tvmazeChecking.value = true
  tvmazeState.value = 'idle'
  tvmazeMsg.value = ''
  try {
    // base 即使为空也要传：后端要区分「没传（读库）」和「清空（按官方测）」。
    const r = await api.calendar.testTVMaze({ base: tvmaze.value.base.trim() })
    tvmazeState.value = r?.ok ? 'ok' : 'fail'
    tvmazeMsg.value = r?.ok ? r?.message || 'TVMaze 可用' : r?.error || '连接失败'
  } catch (e: any) {
    tvmazeState.value = 'fail'
    tvmazeMsg.value = e?.message || '连接失败'
  } finally {
    tvmazeChecking.value = false
  }
}

async function checkTvdb() {
  tvdbChecking.value = true
  tvdbState.value = 'idle'
  tvdbMsg.value = ''
  try {
    // 把输入框里还没保存的新凭据 / 新接口根一起带过去，测的就是用户刚填的那份，
    // 而不是库里旧的那份（否则填完点测试会误判新 Key 无效）。
    // base 即使为空也要传：后端要区分「没传（读库）」和「清空（按官方测）」。
    const body: Record<string, string> = { base: tvdb.value.base.trim() }
    if (tvdb.value.key.trim()) body.api_key = tvdb.value.key.trim()
    if (tvdb.value.pin.trim()) body.pin = tvdb.value.pin.trim()
    const r = await api.calendar.testTVDB(body)
    tvdbState.value = r?.ok ? 'ok' : 'fail'
    tvdbMsg.value = r?.ok ? r?.message || 'TVDB 可用' : r?.error || '连接失败'
  } catch (e: any) {
    tvdbState.value = 'fail'
    tvdbMsg.value = e?.message || '连接失败'
  } finally {
    tvdbChecking.value = false
  }
}

/* ---- 豆瓣：国产剧与国内档期 ----
   国产剧在 TMDB 上常常缺集数、缺档期，豆瓣的「更新至第 N 集 / 11 月 10 日
   上映」才是准的。默认开；部署在境外（豆瓣整站被墙）时可以关掉，
   否则每次搜索都要白等十几秒。 */
const doubanOn = ref(true)
const savedDoubanOn = ref(true)
const douban = ref({ base: '' })
const savedDoubanBase = ref('')
const doubanChecking = ref(false)
const doubanState = ref<'idle' | 'ok' | 'fail'>('idle')
const doubanMsg = ref('')

async function checkDouban() {
  doubanChecking.value = true
  doubanState.value = 'idle'
  doubanMsg.value = ''
  try {
    const r: any = await api.douban.test()
    doubanState.value = r?.ok ? 'ok' : 'fail'
    doubanMsg.value = r?.ok ? r?.message || '豆瓣可用' : r?.error || '连接失败'
  } catch (e: any) {
    doubanState.value = 'fail'
    doubanMsg.value = e?.message || '连接失败'
  } finally {
    doubanChecking.value = false
  }
}

async function saveDouban() {
  const patch: Record<string, string> = {}
  if (doubanOn.value !== savedDoubanOn.value) patch.douban_enabled = doubanOn.value ? '1' : '0'
  // 接口根语义同 TMDB / TVMaze：留空保存 = 恢复官方
  const base = douban.value.base.trim()
  if (base !== savedDoubanBase.value) patch.douban_base = base
  if (!Object.keys(patch).length) {
    toast.info('没有要保存的改动')
    return
  }
  saving.value = true
  try {
    await api.config.saveSettings(patch)
    savedDoubanOn.value = doubanOn.value
    savedDoubanBase.value = base
    toast.success('已保存', '发现与订阅、播出日历下一次请求就生效')
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

/* 一次保存「时刻源 + TVMaze 反代 + TVDB 凭据」：切完数据源只点一个保存，
   免得选了 TVMaze 却还留着旧的 TVDB Key 以为它生效了。 */
async function saveCalendar() {
  const patch: Record<string, string> = {}
  if (calendarSource.value !== savedCalendarSource.value) {
    patch.calendar_source = calendarSource.value
  }
  // 接口根不是密钥：留空保存 = 清空（恢复官方公共 API）。
  const tzBase = tvmaze.value.base.trim()
  if (tzBase !== savedTVMazeBase.value) patch.tvmaze_base = tzBase
  // 密钥语义相反：留空 = 沿用已存的那份。
  if (tvdb.value.key.trim()) patch.tvdb_api_key = tvdb.value.key.trim()
  if (tvdb.value.pin.trim()) patch.tvdb_pin = tvdb.value.pin.trim()
  const base = tvdb.value.base.trim()
  const baseChanged = base !== savedTvdbBase.value
  if (baseChanged) patch.tvdb_base = base
  if (!Object.keys(patch).length) {
    toast.info('没有要保存的改动', '密钥留空表示沿用已存的那份')
    return
  }
  saving.value = true
  try {
    await api.config.saveSettings(patch)
    if (patch.tvdb_api_key) tvdbKeySet.value = true
    if (patch.tvdb_pin) tvdbPinSet.value = true
    tvdb.value.key = ''
    tvdb.value.pin = ''
    savedCalendarSource.value = calendarSource.value
    savedTVMazeBase.value = tzBase
    if (baseChanged) savedTvdbBase.value = base
    const cur = await api.config.settings()
    tvdbUpstream.value = cur?.tvdb_upstream || { api: '' }
    tvmazeUpstream.value = cur?.tvmaze_upstream || { api: '' }
    toast.success('已保存', '日历下一轮生效；想立刻看效果请到播出日历点「重抓」')
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

/* 用「拉一页榜单」验证 TMDB：能返回条目就说明 Key 与网络都通。
   比另造一个 test 接口更实在 —— 测的就是电影墙真正用的那条链路。 */
async function checkTmdb() {
  tmdbChecking.value = true
  tmdbState.value = 'idle'
  tmdbMsg.value = ''
  const t0 = performance.now()
  try {
    const r = await api.local.wall({ type: 'movie', limit: 3, page: 1 })
    const n = r?.items?.length ?? 0
    if (!n) throw new Error('接口通了但没返回条目，可能是配额用尽')
    tmdbState.value = 'ok'
    tmdbMsg.value = `连接正常 · 已读到 ${n} 条 · ${Math.round(performance.now() - t0)}ms（${r.source || 'tmdb'}）`
    toast.success('TMDB 可用', '电影墙数据源正常')
  } catch (e: any) {
    tmdbState.value = 'fail'
    tmdbMsg.value = e?.message || '连接失败'
    toast.warning('TMDB 不可用', tmdbMsg.value)
  } finally {
    tmdbChecking.value = false
  }
}

/* 检测 IMDb：拿输入框里的 Key（留空用库里那份）真打一次查询，
   能把「没网 / Key 无效 / 只能走免 Key 接口」三种情况分开。 */
async function checkImdb() {
  imdbChecking.value = true
  imdbState.value = 'idle'
  imdbMsg.value = ''
  try {
    const r = await api.config.imdbCheck(tmdb.value.imdbKey.trim() || undefined)
    const ms = r?.ms ? ` · ${r.ms}ms` : ''
    const swap = r?.sample && r?.english ? ` · ${r.sample} → ${r.english}` : ''
    if (r?.ok && r?.mode === 'official') {
      imdbState.value = 'ok'
      imdbMsg.value = `${r.note || '官方接口可用'}${swap}${ms}`
    } else if (r?.ok) {
      imdbState.value = 'warn'
      imdbMsg.value = `${r.note || '走免 Key 建议接口，能换名但不保证稳定'}${swap}${ms}`
    } else {
      imdbState.value = 'fail'
      imdbMsg.value = r?.error || '检测失败'
    }
  } catch (e: any) {
    imdbState.value = 'fail'
    imdbMsg.value = e?.message || '检测失败'
  } finally {
    imdbChecking.value = false
  }
}

async function saveDataSources() {
  saving.value = true
  try {
    const patch: Record<string, string> = {}
    // 密钥留空 = 不修改（后端约定），所以只有真填了才提交
    if (tmdb.value.key.trim()) patch.tmdb_api_key = tmdb.value.key.trim()
    if (tmdb.value.imdbKey.trim()) patch.imdb_api_key = tmdb.value.imdbKey.trim()
    const base = tmdb.value.base.trim()
    const baseChanged = base !== savedTmdbBase.value
    const imageBase = tmdb.value.imageBase.trim()
    const imageChanged = imageBase !== savedTmdbImageBase.value
    // 空串也要真的提交（= 恢复官方直连 / 交回后端自动推导），
    // 所以判断的是「有没有变」而非「非空」。
    if (baseChanged) patch.tmdb_base = base
    if (imageChanged) patch.tmdb_image_base = imageBase
    if (!Object.keys(patch).length) {
      toast.info('没有要保存的改动', '密钥留空表示沿用已存的那份')
      return
    }
    await api.config.saveSettings(patch)
    if (patch.tmdb_api_key) tmdbKeySet.value = true
    if (patch.imdb_api_key) imdbKeySet.value = true
    tmdb.value.key = ''
    tmdb.value.imdbKey = ''
    const upstreamChanged = baseChanged || imageChanged
    if (upstreamChanged) {
      if (baseChanged) savedTmdbBase.value = base
      if (imageChanged) savedTmdbImageBase.value = imageBase
      // 图片根可能是后端从 /3 推导出来的，重新读一次才能把「此刻生效的」
      // 两个地址都显示在界面上，而不是让用户自己猜。
      const cur = await api.config.settings()
      tmdbUpstream.value = cur?.tmdb_upstream || { api: '', image: '' }
    }
    toast.success(
      '已保存',
      upstreamChanged
        ? base
          ? '已切到反代，电影墙与刮削立即生效'
          : '已恢复官方直连'
        : '电影墙与换名会立即用新 Key',
    )
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

/* ---- 代理 + 真实网络检测 ---- */
const proxy = ref({ url: '' })
const savedProxy = ref('')
/** 后端真正生效的代理（容器里可能已把 127.0.0.1 改写成宿主机网关） */
const proxyStatus = ref({ effective: '', source: '', rewritten: false })
const netChecking = ref(false)

function readProxyStatus(s: any) {
  proxyStatus.value = {
    effective: s?.net_proxy_effective || '',
    source: s?.net_proxy_source || '',
    rewritten: !!s?.net_proxy_rewritten,
  }
}
const netResults = ref<
  {
    key: string
    name: string
    host: string
    group: string
    via: string
    ok: boolean
    ms: number
    status?: number
    error?: string
  }[]
>([])
const netSummary = ref<{ ok: number; total: number; at?: string }>({ ok: 0, total: 0 })

/**
 * 同一个目标会测两遍（直连 + 代理）。只要有一条通，这个目标就算通 ——
 * 在国内直连 Google / Telegram 超时是常态，把它们标成红色的「不可达」，
 * 用户看到的就是「一片红」，而实际情况往往完全正常。
 */
const netRows = computed(() => {
  const best = new Map<string, boolean>()
  for (const r of netResults.value) best.set(r.key, (best.get(r.key) ?? false) || r.ok)
  return netResults.value.map((r) => ({ ...r, alt: !r.ok && best.get(r.key) === true }))
})

async function testNet() {
  netChecking.value = true
  netResults.value = []
  try {
    const r = await api.config.netCheck()
    const all: typeof netResults.value = r?.results ?? []
    // 没配代理时后端也会跑一遍「走代理」的分支，结果和直连完全重复，这里只留直连
    netResults.value = r?.proxy ? all : all.filter((x) => x.via !== 'proxy')
    // 按「目标」统计而不是按「行」：同一目标的两条通路只算一次，
    // 否则配了代理时总数会翻倍，看起来永远有一半不通。
    const keys = new Map<string, boolean>()
    for (const x of netResults.value) keys.set(x.key, (keys.get(x.key) ?? false) || x.ok)
    const ok = [...keys.values()].filter(Boolean).length
    netSummary.value = { ok, total: keys.size, at: r?.checked_at }
  } catch (e: any) {
    toast.warning('网络检测失败', e?.message)
  } finally {
    netChecking.value = false
  }
}

async function saveProxy() {
  saving.value = true
  try {
    await api.config.saveSettings({ net_proxy: proxy.value.url.trim() })
    savedProxy.value = proxy.value.url.trim()
    // 保存后后端立刻重算「当前生效」的地址，回显一次，
    // 否则用户填 127.0.0.1 却不知道 TMDB / Telegram 到底走没走代理。
    const s: any = await api.config.settings()
    readProxyStatus(s)
    const hint = proxyStatus.value.rewritten
      ? '容器内已自动改写成宿主机地址，立即生效'
      : savedProxy.value ? '已对所有外网请求立即生效' : '已改为直连'
    toast.success('代理已保存', hint)
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

onMounted(async () => {
  try {
    const s = await api.config.settings()
    tmdbKeySet.value = s?.tmdb_api_key_set === '1'
    imdbKeySet.value = s?.imdb_api_key_set === '1'
    tvdbKeySet.value = s?.tvdb_api_key_set === '1'
    tvdbPinSet.value = s?.tvdb_pin_set === '1'
    tvdb.value.base = s?.tvdb_base || ''
    savedTvdbBase.value = tvdb.value.base
    tvdbUpstream.value = s?.tvdb_upstream || { api: '' }
    const src = s?.calendar_source
    calendarSource.value = src === 'tvdb' || src === 'off' ? src : 'tvmaze'
    savedCalendarSource.value = calendarSource.value
    tvmaze.value.base = s?.tvmaze_base || ''
    savedTVMazeBase.value = tvmaze.value.base
    tvmazeUpstream.value = s?.tvmaze_upstream || { api: '' }
    tmdb.value.base = s?.tmdb_base || ''
    savedTmdbBase.value = tmdb.value.base
    tmdb.value.imageBase = s?.tmdb_image_base || ''
    savedTmdbImageBase.value = tmdb.value.imageBase
    tmdbUpstream.value = s?.tmdb_upstream || { api: '', image: '' }
    doubanOn.value = s?.douban_enabled !== '0'
    savedDoubanOn.value = doubanOn.value
    douban.value.base = s?.douban_base || ''
    savedDoubanBase.value = douban.value.base
    proxy.value.url = s?.net_proxy || ''
    savedProxy.value = proxy.value.url
    readProxyStatus(s)
  } catch {
    /* 读不到设置不阻塞页面 */
  }
})

/* ---- Emby：全部走真实接口（以前这里是写死的假数据 + setTimeout toast） ---- */
interface ServerLib {
  id: string
  name: string
  type: string
  paths: string[]
  linked: boolean
}
const emby = ref({
  kind: 'emby',
  url: '',
  key: '',
  keySet: false,
  pathFrom: '',
  pathTo: '',
  // 302 网关：开关 + UA 例外。它和播放跳转共用一个端口，没有自己的端口号；
  // gwRunning 是进程回报的真实状态（配置不全时即使开关打开也不会接管）。
  gwOn: false,
  gwBypass: '',
  gwRunning: false,
  gwAddr: '',
})
const embyLibs = ref<ServerLib[]>([])
const embyStatus = ref({ connected: false, serverName: '', version: '', libraries: 0, error: '' })
const embyBusy = ref(false)
const embyLoaded = ref(false)
const embyConnectOpen = ref(false)
const embyConnect = ref({ username: '', password: '' })

async function loadEmby() {
  embyBusy.value = true
  try {
    const [cfg, st]: any[] = await Promise.all([api.config.settings(), api.config.serverStatus()])
    emby.value.kind = cfg?.server_kind || 'emby'
    emby.value.url = cfg?.server_url || ''
    emby.value.key = '' // 密钥不回传明文，留空 = 不修改
    emby.value.keySet = cfg?.server_key_set === '1'
    emby.value.pathFrom = cfg?.server_path_from || ''
    emby.value.pathTo = cfg?.server_path_to || ''
    // 老配置只有 emby_gateway_port（填了即启用），后端已把它迁移成 emby_gateway_on。
    const on = String(cfg?.emby_gateway_on ?? '').toLowerCase()
    emby.value.gwOn = ['1', 'on', 'true', 'yes'].includes(on)
    emby.value.gwBypass = cfg?.emby_gateway_bypass || ''
    emby.value.gwRunning = !!cfg?.emby_gateway?.enabled
    emby.value.gwAddr = String(cfg?.emby_gateway?.listen || cfg?.emby_gateway?.addr || '')
    embyStatus.value = {
      connected: !!st?.connected,
      serverName: st?.server_name || '',
      version: st?.version || '',
      libraries: Number(st?.libraries || 0),
      error: st?.error || '',
    }
    const libs: any = await api.config.serverLibraries()
    embyLibs.value = Array.isArray(libs?.libraries) ? libs.libraries : []
    if (libs?.error) embyStatus.value.error = libs.error
  } catch (err: any) {
    embyStatus.value.error = err?.message || '读取媒体服务器信息失败'
  } finally {
    embyBusy.value = false
    embyLoaded.value = true
  }
}

/** 切到这个页签才去问 Emby：没配服务器时不该拖慢设置页首屏。 */
watch(tab, (v) => {
  if (v === 'emby' && !embyLoaded.value) void loadEmby()
})

async function saveEmby() {
  embyBusy.value = true
  try {
    await api.config.saveSettings({
      server_kind: emby.value.kind,
      server_url: emby.value.url.trim(),
      server_key: emby.value.key.trim(), // 留空 = 后端不修改已存的 Key
      server_path_from: emby.value.pathFrom.trim(),
      server_path_to: emby.value.pathTo.trim(),
    })
    emby.value.key = ''
    toast.success('Emby 配置已保存', '连接状态与媒体库列表已重新读取')
    await loadEmby()
  } catch (err: any) {
    toast.warning('保存失败', err?.message || '')
  } finally {
    embyBusy.value = false
  }
}

async function testEmby() {
  embyBusy.value = true
  try {
    const r: any = await api.config.testServer({
      kind: emby.value.kind,
      url: emby.value.url.trim(),
      apiKey: emby.value.key.trim(),
    })
    toast.success('Emby 连接正常', `${r?.libraries ?? 0} 个媒体库可见`)
    await loadEmby()
  } catch (err: any) {
    toast.warning('连接失败', err?.message || '')
  } finally {
    embyBusy.value = false
  }
}

/** 用媒体服务器自己的管理员账号一键建 API Key，不用来回抄 32 位十六进制。 */
async function connectEmby() {
  if (!embyConnect.value.username.trim()) {
    toast.warning('填一下媒体服务器的管理员账号')
    return
  }
  embyBusy.value = true
  try {
    const r: any = await api.config.connectServer({
      kind: emby.value.kind,
      url: emby.value.url.trim(),
      username: embyConnect.value.username.trim(),
      password: embyConnect.value.password,
    })
    embyConnectOpen.value = false
    embyConnect.value = { username: '', password: '' }
    toast.success('已连接 ' + (r?.server_name || '媒体服务器'), `自动创建 API Key，${r?.libraries ?? 0} 个库`)
    await loadEmby()
  } catch (err: any) {
    toast.warning('连接失败', err?.message || '')
  } finally {
    embyBusy.value = false
  }
}

/** 保存 302 网关配置：后端即时切换 handler，不用重启容器。 */
async function saveGateway() {
  embyBusy.value = true
  try {
    await api.config.saveSettings({
      emby_gateway_on: emby.value.gwOn ? '1' : '0',
      emby_gateway_bypass: emby.value.gwBypass.trim(),
    })
    await loadEmby()
    toast.success(
      emby.value.gwRunning ? '302 网关已接管播放' : '302 网关未启用',
      emby.value.gwRunning
        ? `与播放跳转共用 ${emby.value.gwAddr || ''}，播放器地址不用改`
        : '播放继续走 Emby 中转；确认已连上媒体服务器再打开',
    )
  } catch (err: any) {
    toast.warning('保存失败', err?.message || '')
  } finally {
    embyBusy.value = false
  }
}

async function scanEmby(libId?: string) {
  embyBusy.value = true
  try {
    await api.config.scanServerLibrary(libId)
    toast.success('已触发扫描', libId ? '媒体库开始重新扫描' : '所有媒体库开始重新扫描')
  } catch (err: any) {
    toast.warning('扫描失败', err?.message || '')
  } finally {
    embyBusy.value = false
  }
}

function libTypeLabel(t: string) {
  return t === 'tvshows' ? '电视节目' : t === 'movies' ? '电影' : t || '混合'
}

/* ---- 安全 ---- */
const sec = ref({ current: '', next: '', confirm: '', otp: false })

function changePwd() {
  if (!sec.value.current || !sec.value.next) {
    toast.warning('先填当前密码和新密码')
    return
  }
  if (sec.value.next !== sec.value.confirm) {
    toast.warning('两次输入的新密码不一致')
    return
  }
  toast.success('密码已更新', '其它设备的会话会失效')
  sec.value = { current: '', next: '', confirm: '', otp: sec.value.otp }
}

/* ---- 备份 ---- */
const backup = ref({ enabled: true, cron: '0 4 * * *', keep: 7, dir: '/data/backups' })
const backups = [
  { name: 'backup-20261107-040000.zip', size: '18.4 MB', at: '今天 04:00', tag: '自动' },
  { name: 'backup-20261106-040000.zip', size: '18.2 MB', at: '昨天 04:00', tag: '自动' },
  { name: 'backup-20261105-213012.zip', size: '17.9 MB', at: '11-05 21:30', tag: '手动' },
]

/* ---- 版本 ----
 * 版本号由 scripts/bump-version.py 统一写入 VERSION / Dockerfile / compose /
 * main.go / package.json。这里只显示后端真实回报的版本，不再假装能联网查更新：
 * 本项目没有发布通道，那个「下载并替换二进制」按钮以前点了只会弹个 toast。
 */
const currentVersion = computed(() => session.version || 'v0.4.5')
/** 本版变更：每次跑 bump-version.py 时顺手改这一栏，用户能一眼看出换没换版本。 */
const releaseNotes = [
  '新增版本号机制（VERSION + scripts/bump-version.py），每次修复递增',
  '首页加 CPU / 内存 / 媒体盘与 115、夸克网盘容量',
  '影视库新建媒体库支持夸克账号，夸克 Cookie 自动保活 + 扫码登录',
  '发现与订阅：豆瓣数据异步加载，国产片不再等 TMDB 换名',
  '播出日历：新剧与电影筛选分开，国产剧单独一栏',
  'TG 搜索先出 TMDB 候选，资源清单直接点片名入库',
  '系统配置：EMBY 管理接上真实接口',
  'v0.2.1：.env.example 的 VERSION 纳入同步；vite build 自动保留 webdist/.gitkeep',
  'v0.2.2：代理设置对所有外网请求立即生效；容器内 127.0.0.1 自动改写成宿主机地址',
  'v0.2.2：夸克扫码修复「登录请求过期」（request_id 复用 + 2 分钟自动换码）',
  'v0.2.3：新增 scripts/fast-update.py，只换二进制即可更新，不用重建镜像',
  'v0.2.3：emby.example.com 按 /d/ 分流，公网可同时打开 Emby Web 与播放 .strm',
  'v0.2.4：台账为空的旧媒体目录也能在页面「保存并改写已有 STRM」一次改好播放地址',
  'v0.2.5：夸克列目录 page_size 自动收敛到 100，账号卡片不再串出红色报错',
  'v0.2.5：「保存到夸克目录」改成网盘目录选择器，不用再手填 fid',
  'v0.2.5：顶栏右侧滑过即展开、点击可固定；去掉弹层模糊与重复轮询，公网访问不再发涩',
  'v0.2.6：网盘文件页/媒体库页统一 115 与夸克TV，夸克不支持的移动删除会明确禁用',
  'v0.2.6：媒体库页新增「目录设置」，转存目录可直接选、媒体库目录与各库本地路径一目了然',
  'v0.2.6：发现与订阅改成每平台一行的大海报墙，电影 / 剧集一键切换，行内可横滑、可查看全部',
  'v0.2.6：认片搜索与海报点击先进独立「影视详情页」，看完简介/分季/入库状态再决定找资源',
  'v0.2.7：一个媒体库可以同时挂 115 和夸克的目录，都写进同一个本地目录，Emby 只认一个库',
  'v0.2.7：夸克文件的 .strm 地址带 q/ 前缀（/d/q/{fid}），115 的地址格式一个都没变',
  'v0.2.7：两个网盘都有同一部片时只留体积大的那份；一个来源的清理不会波及另一个来源',
  'v0.2.9：自动整理支持多条规则：115 和夸克各配一条，各自搬进自己的目标库',
  'v0.2.9：夸克也能自动整理了（网页接口移动/改名/删除，需要账号带网页 Cookie）',
  'v0.2.9：网络检测按「目标」统计，直连不通但代理可通不再标红；新增 115 播放 CDN 检测',
  'v0.3.0：公网访问时菜单秒开 —— 首屏稳定后预取各页面 JS，悬停侧栏即优先加载',
  'v0.3.0：TG 频道填成机器人（@xxxbot）会直接说清楚「不是公开频道」，不再只显示 0 条',
  'v0.3.1：新增「播放直连（302 网关）」：Emby 播 .strm 时把播放器 302 到网盘直链，视频不再经 NAS 中转',
  'v0.3.1：海报墙默认改读 Emby（接了媒体服务器时），并加了「从 Emby 同步」按钮',
  'v0.3.2：所有页面合成一个包，点左侧菜单立即切换（不再是「点了等一会才刷新」）',
  'v0.3.2：整理通知更实：手动整理无论有没有搬动都会回一条，并带上片名；自动整理现在也覆盖夸克转存目录',
  'v0.3.3：302 网关与播放跳转合并成一个端口（6086），不用再记两个端口，Emby 客户端直接填它即可',
  'v0.3.4：新增「资源搜索」独立页：从「发现与订阅」和影视详情都能跳进来，点开详情再返回，搜索词和结果都还在',
  'v0.3.4：TG 机器人新增 /name（或 /n）片名，直接返回 TMDB 英文名，点一下就能复制',
  'v0.3.5：起播更快：客户端一问「怎么播」就先把网盘直链换好，取流时命中缓存直接 302',
  'v0.3.5：刮削写文件失败时，日志会直接给出该执行的 chown 命令（之前只报 permission denied）',
  'v0.3.6：影视详情页的「返回」跟着来路走：从搜索结果进就退回搜索结果（搜索词与结果都还在），不再一律回「发现与订阅」',
  'v0.3.6：搜索结果里锁定影片后多了「详情」按钮，可先看海报简介再决定要不要转存',
]
</script>

<template>
  <div class="col" style="gap: 16px">
    <HTabs v-model="tab" :items="tabs" />

    <!-- 电影数据源 -->
    <template v-if="tab === 'tmdb'">
      <HPanel title="TMDB" desc="电影墙、海报与元数据的主数据源；源码不内置 Key，请在这里填，或注入 STRM115_TMDB_KEY 环境变量">
        <HField
          label="API Key"
          tip="v3 Key。填完点「测试连接」，它真的去拉一页榜单，能返回条目才算通"
          :hint="tmdbKeySet ? '当前已配置一份（不回显明文）；留空保存表示不修改' : '还没配置：请填自己的 Key（或给容器注入 STRM115_TMDB_KEY），否则电影墙与刮削不可用'"
        >
          <div class="row" style="gap: 8px">
            <input
              v-model="tmdb.key"
              class="input mono"
              style="max-width: 400px"
              placeholder="32 位十六进制 Key（留空不修改）"
            />
            <button class="btn btn-tertiary" :disabled="tmdbChecking" @click="checkTmdb">
              <AppIcon name="shield" :size="14" />{{ tmdbChecking ? '测试中…' : '测试连接' }}
            </button>
          </div>
          <p
            v-if="tmdbState !== 'idle'"
            class="field-hint"
            :style="{ color: tmdbState === 'ok' ? 'var(--success)' : 'var(--danger)' }"
          >
            {{ tmdbMsg }}
          </p>
        </HField>

        <HField
          label="获取地址"
          tip="下面是 TMDB 官方的 Key 申请页、注册页、接口文档，以及本项目用的反代源码。点开新标签页，或点右侧按钮复制地址"
          hint="请申请一份自己的 v3 Key 填到上面；源码不内置任何 Key。地址只做跳转，不会把 Key 发给任何第三方"
        >
          <div class="keylinks">
            <div v-for="l in tmdbLinks" :key="l.url" class="keylink">
              <a class="btn btn-sm btn-tertiary keylink-btn" :href="l.url" target="_blank" rel="noopener noreferrer">
                <AppIcon name="external" :size="13" />{{ l.label }}
              </a>
              <div class="keylink-text">
                <span class="mono keylink-url">{{ l.url }}</span>
                <span class="muted small">{{ l.note }}</span>
              </div>
              <button class="btn btn-sm btn-ghost" title="复制地址" @click="copyAddr(l.url)">
                <AppIcon name="link" :size="13" />
              </button>
            </div>
          </div>
        </HField>

        <HField
          label="反代地址（可选）"
          tip="大陆直连 api.themoviedb.org 常超时。填 Vercel 反代的 v3 根地址，电影墙、演员头像、订阅会一起切过去，不用重启容器"
          :hint="
            tmdbUpstream.api
              ? `当前生效：${tmdbUpstream.api} ｜ 图片 ${tmdbUpstream.image}`
              : savedTmdbBase
                ? `已配置 ${savedTmdbBase}`
                : '未配置：直连官方接口'
          "
        >
          <div class="col" style="gap: 8px">
            <div class="row" style="gap: 8px">
              <span class="muted small" style="width: 64px; flex: none">接口根</span>
              <input
                v-model="tmdb.base"
                class="input mono"
                style="max-width: 400px"
                placeholder="https://你的域名/3"
              />
            </div>
            <div class="row" style="gap: 8px">
              <span class="muted small" style="width: 64px; flex: none">图片根</span>
              <input
                v-model="tmdb.imageBase"
                class="input mono"
                style="max-width: 400px"
                placeholder="留空 = 自动推导 /t/p"
              />
            </div>
          </div>
          <p class="field-hint">
            形如 <span class="mono">https://你的域名/3</span>，末尾要带 <span class="mono">/3</span>。
            上面「自建反代项目」的 vercel.json 把 <span class="mono">/3/*</span> 转给 TMDB 接口、
            <span class="mono">/t/p/*</span> 转给图片 CDN，所以图片根留空会自动推导；
            想让图片直连官方就填 <span class="mono">https://image.tmdb.org/t/p</span>（本机直连通常比反代快）。
            <strong>接口根留空保存 = 恢复官方直连</strong>，和上面密钥的「留空不修改」正好相反。
            改完点上面的「测试连接」，它真的会拉一页榜单。
          </p>
        </HField>

        <HField
          label="IMDb 连通性"
          tip="资源搜索要把中文片名换成英文名再搜种子站。这里用 Key 真查一次「沙丘2」，能区分断网 / Key 无效 / 只能走免 Key 接口"
          :hint="
            imdbState !== 'idle'
              ? ''
              : imdbKeySet
                ? '当前已配置一份（不回显明文）'
                : '未配置：走免 Key 建议接口，能换名但稳定性差'
          "
        >
          <div class="row" style="gap: 8px">
            <input
              v-model="tmdb.imdbKey"
              class="input mono"
              style="max-width: 400px"
              placeholder="IMDb GraphQL Key（可留空）"
            />
            <button class="btn btn-tertiary" :disabled="imdbChecking" @click="checkImdb">
              <AppIcon name="search" :size="14" />{{ imdbChecking ? '检测中…' : '检测 IMDb' }}
            </button>
          </div>
          <p
            v-if="imdbState !== 'idle'"
            class="field-hint"
            :style="{
              color:
                imdbState === 'ok' ? 'var(--success)' : imdbState === 'warn' ? '#b45309' : 'var(--danger)',
            }"
          >
            {{ imdbMsg }}
          </p>
        </HField>

        <HField label="固定项" hint="片名/简介固定 zh-CN；海报列表 w342、详情 w780、背景 w1280">
          <div class="row wrap" style="gap: 6px">
            <span class="badge">语言 zh-CN</span>
            <span class="badge">列表 w342</span>
            <span class="badge">背景 w1280</span>
            <span class="badge">发现接口 /discover</span>
            <span class="badge">区域 US（平台筛选需要）</span>
          </div>
        </HField>

        <template #footer>
          <button class="btn btn-primary" :disabled="saving" @click="saveDataSources">
            {{ saving ? '保存中…' : '保存配置' }}
          </button>
        </template>
      </HPanel>

      <HPanel
        title="播出日历 · 时刻数据源"
        desc="TMDB 的分集只有日期、没有「几点播」。默认用免 Key 的 TVMaze 补时刻与电视网；TVDB v4 已不再向个人发免费项目 Key，有授权的用户可切回去。"
      >
        <HField
          label="时刻数据源"
          tip="决定播出日历用谁补「几点播」；剧单、分集、海报永远来自 TMDB"
          hint="切换后点底部保存，再到播出日历点「重抓」立即生效"
        >
          <select v-model="calendarSource" class="input" style="max-width: 320px">
            <option value="tvmaze">TVMaze（推荐 · 免 API Key）</option>
            <option value="tvdb">TVDB v4（需项目 Key / PIN）</option>
            <option value="off">关闭（只显示日期）</option>
          </select>
          <p class="field-hint">
            TVMaze 公共 API 免费、无需注册，数据按 <span class="mono">CC BY-SA</span> 授权，
            日历页会自动显示署名链接。任一增强源失败都只影响「时刻」这一列，日历主体照常出结果。
          </p>
        </HField>

        <template v-if="calendarSource === 'tvmaze'">
          <HField
            label="TVMaze 获取地址"
            tip="不需要申请 Key，这里只是官网与接口文档；点开新标签页，或点右侧按钮复制地址"
            hint="本项目用 /schedule 与 /schedule/web 两条排期接口"
          >
            <div class="keylinks">
              <div v-for="l in tvmazeLinks" :key="l.url" class="keylink">
                <a class="btn btn-sm btn-tertiary keylink-btn" :href="l.url" target="_blank" rel="noopener noreferrer">
                  <AppIcon name="external" :size="13" />{{ l.label }}
                </a>
                <div class="keylink-text">
                  <span class="mono keylink-url">{{ l.url }}</span>
                  <span class="muted small">{{ l.note }}</span>
                </div>
                <button class="btn btn-sm btn-ghost" title="复制地址" @click="copyAddr(l.url)">
                  <AppIcon name="link" :size="13" />
                </button>
              </div>
            </div>
          </HField>

          <HField
            label="接口根（可选）"
            tip="直连不稳时填自建反代根，保存后下一轮日历立即生效，不用重启容器"
            :hint="tvmazeUpstream.api ? `当前生效：${tvmazeUpstream.api}` : '未配置：直连官方 api.tvmaze.com'"
          >
            <div class="row" style="gap: 8px">
              <input
                v-model="tvmaze.base"
                class="input mono"
                style="max-width: 400px"
                placeholder="https://api.tvmaze.com（留空 = 官方）"
              />
              <button class="btn btn-tertiary" :disabled="tvmazeChecking" @click="checkTVMaze">
                <AppIcon name="shield" :size="14" />{{ tvmazeChecking ? '测试中…' : '测试连接' }}
              </button>
            </div>
            <p
              v-if="tvmazeState !== 'idle'"
              class="field-hint"
              :style="{ color: tvmazeState === 'ok' ? 'var(--success)' : 'var(--danger)' }"
            >
              {{ tvmazeMsg }}
            </p>
            <p class="field-hint">
              要带 <span class="mono">http://</span> 或 <span class="mono">https://</span>，末尾不带斜杠；
              <strong>留空保存 = 恢复官方接口</strong>。代理跟随「代理配置」页的全局设置。
            </p>
          </HField>
        </template>

        <template v-if="calendarSource === 'tvdb'">
          <p class="field-hint" style="color: var(--danger)">
            TheTVDB 已改为项目级授权：个人账号不再发放免费 v4 Key，若没有项目 Key / 订阅 PIN，
            建议切回上面的 TVMaze。
          </p>
        <HField
          label="获取地址"
          tip="TVDB v4 的 Key 按项目申请，不是个人随手生成；用户支持型 Key 还要配套订阅 PIN。点开新标签页，或点右侧按钮复制地址"
          hint="申请页需要先登录 thetvdb.com；选「End-User Subscriptions」时 Key 立即批准，PIN 在订阅页拿"
        >
          <div class="keylinks">
            <div v-for="l in tvdbLinks" :key="l.url" class="keylink">
              <a class="btn btn-sm btn-tertiary keylink-btn" :href="l.url" target="_blank" rel="noopener noreferrer">
                <AppIcon name="external" :size="13" />{{ l.label }}
              </a>
              <div class="keylink-text">
                <span class="mono keylink-url">{{ l.url }}</span>
                <span class="muted small">{{ l.note }}</span>
              </div>
              <button class="btn btn-sm btn-ghost" title="复制地址" @click="copyAddr(l.url)">
                <AppIcon name="link" :size="13" />
              </button>
            </div>
          </div>
        </HField>

        <HField
          label="API Key"
          tip="thetvdb.com 账户里的 v4 API Key；用户支持型 Key 还需要下面的 PIN。测试连接会真的去换一次 Bearer Token"
          :hint="
            tvdbKeySet
              ? '当前已配置一份（不回显明文）；留空保存表示不修改'
              : '还没配置：日历仍会显示 TMDB 的日期与集标题'
          "
        >
          <div class="row" style="gap: 8px">
            <input
              v-model="tvdb.key"
              class="input mono"
              style="max-width: 400px"
              placeholder="TVDB v4 API Key（留空不修改）"
            />
            <button class="btn btn-tertiary" :disabled="tvdbChecking" @click="checkTvdb">
              <AppIcon name="shield" :size="14" />{{ tvdbChecking ? '测试中…' : '测试连接' }}
            </button>
          </div>
          <p
            v-if="tvdbState !== 'idle'"
            class="field-hint"
            :style="{ color: tvdbState === 'ok' ? 'var(--success)' : 'var(--danger)' }"
          >
            {{ tvdbMsg }}
          </p>
        </HField>

        <HField
          label="Subscriber PIN（可选）"
          tip="v4 登录用 {apikey, pin} 换 Token，Token 有效期一个月，后端会自动缓存与重登"
          :hint="tvdbPinSet ? '当前已配置一份（不回显明文）' : '没用订阅型 Key 可以留空'"
        >
          <input
            v-model="tvdb.pin"
            class="input mono"
            type="password"
            style="max-width: 400px"
            placeholder="PIN（留空不修改）"
          />
        </HField>

        <HField
          label="接口地址（可选）"
          tip="api4.thetvdb.com 直连不稳时，可填自建 v4 反代的根地址；保存后播出日历下一轮立即生效，不用重启容器"
          :hint="
            tvdbUpstream.api
              ? `当前生效：${tvdbUpstream.api}`
              : '未配置：直连官方 v4 接口'
          "
        >
          <input
            v-model="tvdb.base"
            class="input mono"
            style="max-width: 400px"
            placeholder="https://api4.thetvdb.com/v4（留空 = 官方）"
          />
          <p class="field-hint">
            要带 <span class="mono">http://</span> 或 <span class="mono">https://</span>，
            末尾不带斜杠；<strong>留空保存 = 恢复官方接口</strong>（和密钥「留空不修改」相反）。
            上面的「测试连接」会连这份还没保存的地址一起测，改完可直接验。
          </p>
        </HField>

        <HField label="固定项" hint="剧单与分集以 TMDB 为准；增强源只补时刻 / 电视网，两边季集不一致时以 TMDB 的 SxxExx 为准">
          <div class="row wrap" style="gap: 6px">
            <span class="badge">v4 /login</span>
            <span class="badge">/series/{id}/extended</span>
            <span class="badge">Token 缓存 20 天</span>
            <span class="badge">整周日历缓存</span>
            <span class="badge">无 Key 自动降级</span>
          </div>
        </HField>
        </template>

        <template #footer>
          <button class="btn btn-primary" :disabled="saving" @click="saveCalendar">
            {{ saving ? '保存中…' : '保存日历数据源' }}
          </button>
        </template>
      </HPanel>

      <HPanel
        title="豆瓣 · 国产剧与国内档期"
        desc="国产剧的「更新至第 N 集」和国内电影的上映日期只有豆瓣准，TMDB 常常缺。开启后「发现与订阅」的搜索框会同时给豆瓣候选，播出日历里的国产剧与档期也走豆瓣。"
      >
        <HField
          label="启用豆瓣数据源"
          tip="关掉之后不会再有请求打到豆瓣；国产剧和国内档期在日历里会消失，搜索只剩 TMDB"
          hint="默认开启。部署在境外、豆瓣整站被墙时可以关掉"
        >
          <label class="checkline"><HSwitch v-model="doubanOn" />使用豆瓣数据</label>
        </HField>

        <HField
          label="接口地址（可选）"
          tip="m.douban.com 直连不稳时填自建反代根；反代需同时转发 /rexxar 与 /j 两条路径"
          :hint="douban.base ? `当前填写：${douban.base}` : '未配置：直连官方 m.douban.com / movie.douban.com'"
        >
          <div class="row wrap" style="gap: 8px; align-items: center">
            <input
              v-model="douban.base"
              class="input mono"
              style="max-width: 400px"
              placeholder="https://m.douban.com（留空 = 官方）"
            />
            <button class="btn btn-tertiary" :disabled="doubanChecking" @click="checkDouban">
              <AppIcon name="shield" :size="14" />{{ doubanChecking ? '测试中…' : '测试连接' }}
            </button>
          </div>
          <p
            v-if="doubanState !== 'idle'"
            class="field-hint"
            :style="{ color: doubanState === 'ok' ? 'var(--success)' : 'var(--danger)' }"
          >
            {{ doubanMsg }}
          </p>
          <p class="field-hint">
            豆瓣没有公开 API，本项目用的是网页自己在用的三条 JSON 接口（搜索建议、
            榜单、详情）。请求会带浏览器 UA 与 Referer，并做了串行节流与缓存 ——
            这些都在后端，界面这侧只会看到「稍微慢一点」。
          </p>
        </HField>

        <template #footer>
          <button class="btn btn-primary" :disabled="saving" @click="saveDouban">
            {{ saving ? '保存中…' : '保存豆瓣设置' }}
          </button>
        </template>
      </HPanel>
    </template>

    <!-- 代理 -->
    <template v-else-if="tab === 'proxy'">
      <HPanel title="代理配置" desc="访问 TMDB / IMDb / Telegram 等外网服务">
        <HField
          label="代理地址"
          tip="HTTP(S) / SOCKS5 代理，格式 http://IP:端口；留空表示直连"
          hint="对 TMDB、豆瓣、TVMaze、Telegram、夸克登录等所有外网请求立即生效，不用重启"
        >
          <input v-model="proxy.url" class="input mono" placeholder="如 http://127.0.0.1:7890，留空不代理" />
        </HField>
        <div v-if="proxyStatus.effective" class="row wrap" style="gap: 8px; align-items: center; margin-top: 10px">
          <span class="badge badge-success">当前生效</span>
          <span class="mono small">{{ proxyStatus.effective }}</span>
          <span class="muted small">来源：{{ proxyStatus.source }}</span>
        </div>
        <p v-if="proxyStatus.rewritten" class="muted small" style="margin: 6px 0 0; line-height: 1.65">
          你填的是 <span class="mono">127.0.0.1</span>，但服务跑在 Docker 里，那个地址指向容器自己，
          连不上宿主机的代理客户端。已自动改写成上面的宿主机地址；库里仍保存你填的原值，
          在宿主机直接运行时不受影响。
        </p>
        <template #footer>
          <button class="btn btn-tertiary" :disabled="netChecking" @click="testNet">
            <AppIcon name="refresh" :size="14" />{{ netChecking ? '探测中…' : '网络检测' }}
          </button>
          <button class="btn btn-primary" :disabled="saving || proxy.url.trim() === savedProxy" @click="saveProxy">
            {{ proxy.url.trim() === savedProxy ? '已保存' : '保存配置' }}
          </button>
        </template>
      </HPanel>

      <HPanel
        v-if="netResults.length || netChecking"
        title="网络检测结果"
        :desc="`${netSummary.ok} / ${netSummary.total} 个目标可达 · 同一目标任一路通即算通；代理回 5xx 按失败算`"
      >
        <div v-if="netChecking" class="grid-3">
          <div v-for="i in 6" :key="i" class="skeleton" style="height: 62px; border-radius: var(--r-md)" />
        </div>
        <div v-else class="grid-3">
          <div v-for="r in netRows" :key="`${r.key}-${r.via}`" class="net" :class="{ bad: !r.ok && !r.alt }">
            <div class="row-between">
              <strong class="small">{{ r.name }}</strong>
              <span class="badge" :class="r.ok ? 'badge-success' : r.alt ? 'badge-warning' : 'badge-danger'">
                {{ r.ok ? '可达' : r.alt ? '直连不通' : '不可达' }}
              </span>
            </div>
            <p class="muted small mono truncate">{{ r.host }}</p>
            <p class="muted small">
              {{ r.ok ? `${r.ms}ms · HTTP ${r.status || '—'}` : r.error }}
              <span class="badge" style="margin-left: 6px">{{ r.via === 'proxy' ? '代理' : '直连' }}</span>
            </p>
          </div>
        </div>
      </HPanel>
    </template>

    <!-- EMBY -->
    <template v-else-if="tab === 'emby'">
      <HPanel title="媒体服务器" desc="连接、API Key 与路径映射都走真实接口">
        <div class="row wrap" style="gap: 8px; margin-bottom: 12px">
          <span class="badge" :class="embyStatus.connected ? 'badge-success' : ''">
            {{ embyStatus.connected ? '已连接' : '未连接' }}
          </span>
          <span v-if="embyStatus.serverName" class="muted small">
            {{ embyStatus.serverName }} <template v-if="embyStatus.version">· {{ embyStatus.version }}</template>
            · {{ embyStatus.libraries }} 个库
          </span>
          <span v-if="embyStatus.error" class="small text-danger">{{ embyStatus.error }}</span>
          <span v-if="embyBusy" class="muted small">处理中…</span>
        </div>
        <HField label="类型">
          <select v-model="emby.kind" class="select-native" style="max-width: 220px">
            <option value="emby">Emby</option>
            <option value="jellyfin">Jellyfin</option>
          </select>
        </HField>
        <HField
          label="服务器地址"
          required
          tip="容器里访问同一台机器的 Emby 要写容器能解析的地址，例如 http://emby:8096"
        >
          <input v-model="emby.url" class="input mono" placeholder="http://192.168.1.10:8096" />
        </HField>
        <HField
          label="API Key"
          :hint="emby.keySet ? '已配置；留空表示不修改' : 'Emby 后台 → 高级 → API 密钥，或用下面的管理员账号一键创建'"
        >
          <input
            v-model="emby.key"
            class="input mono"
            type="password"
            :placeholder="emby.keySet ? '已配置，留空不修改' : 'Emby API Key'"
          />
        </HField>
        <HField label="路径映射" hint="两边挂同一个目录时留空即可；容器里是 /media、Emby 里是别的路径时才要填">
          <div class="row wrap" style="gap: 8px">
            <input v-model="emby.pathFrom" class="input mono" placeholder="容器内：/media" />
            <span class="muted">→</span>
            <input v-model="emby.pathTo" class="input mono" placeholder="Emby 看到：/media" />
          </div>
        </HField>
        <template #footer>
          <button class="btn btn-tertiary" :disabled="embyBusy" @click="testEmby">
            <AppIcon name="shield" :size="14" />测试连接
          </button>
          <button class="btn btn-tertiary" @click="embyConnectOpen = !embyConnectOpen">
            <AppIcon name="key" :size="14" />用管理员账号连接
          </button>
          <button class="btn btn-primary" :disabled="embyBusy" @click="saveEmby">保存</button>
        </template>
      </HPanel>

      <HPanel v-if="embyConnectOpen" title="用管理员账号连接" desc="自动创建专属 API Key，密码只在本次请求里用">
        <div class="row wrap" style="gap: 10px">
          <input v-model="embyConnect.username" class="input" placeholder="Emby 管理员账号" style="max-width: 220px" />
          <input
            v-model="embyConnect.password"
            class="input"
            type="password"
            placeholder="Emby 管理员密码"
            style="max-width: 220px"
          />
          <button class="btn btn-primary btn-sm" :disabled="embyBusy" @click="connectEmby">连接</button>
        </div>
        <p class="muted small" style="margin: 8px 0 0">
          账号密码是<strong>媒体服务器</strong>的，不是 strm115 的；不会写进数据库或日志。
        </p>
      </HPanel>

      <HPanel title="媒体库" desc="来自媒体服务器的真实库列表，不再是写死的示例" flush>
        <template #actions>
          <button class="btn btn-sm btn-tertiary" :disabled="embyBusy" @click="loadEmby">
            <AppIcon name="refresh" :size="14" />刷新
          </button>
          <button class="btn btn-sm btn-tertiary" :disabled="embyBusy || !embyStatus.connected" @click="scanEmby()">
            <AppIcon name="play" :size="14" />扫描全部
          </button>
        </template>
        <div class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>名称</th>
                <th>类型</th>
                <th>路径</th>
                <th>状态</th>
                <th class="num">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="l in embyLibs" :key="l.id">
                <td><strong>{{ l.name }}</strong></td>
                <td class="muted small">{{ libTypeLabel(l.type) }}</td>
                <td class="mono small muted">{{ l.paths?.join('、') || '—' }}</td>
                <td>
                  <span class="badge" :class="l.linked ? 'badge-success' : ''">
                    {{ l.linked ? '路径已对齐' : '路径未对齐' }}
                  </span>
                </td>
                <td class="num">
                  <button
                    class="btn btn-sm btn-ghost"
                    :disabled="embyBusy"
                    title="让媒体服务器重新扫描这个库"
                    @click="scanEmby(l.id)"
                  >
                    <AppIcon name="refresh" :size="13" />
                  </button>
                </td>
              </tr>
              <tr v-if="!embyLibs.length">
                <td colspan="5" class="muted small" style="padding: 18px">
                  {{ embyStatus.error || '还没有读到媒体库；先填好地址与 API Key，测试连接后点「刷新」。' }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </HPanel>
      <HPanel
        title="播放直连（302 网关）"
        desc="和播放跳转合并成同一个端口：播 .strm 时把客户端 302 到网盘直链，视频不再经过 Emby 中转"
      >
        <div class="row wrap" style="gap: 8px; margin-bottom: 14px">
          <span class="badge" :class="emby.gwRunning ? 'badge-success' : ''">
            {{ emby.gwRunning ? '已接管 ' + emby.gwAddr : '未启用（播放走 Emby 中转）' }}
          </span>
          <span class="muted small">
            Emby 自己不做 302：它把 .strm 的字节拉下来再转发，外网看片等于「网盘 → NAS → 隧道 → 播放器」。
            打开这个开关后，播放流量直接从网盘到播放器。
          </span>
        </div>
        <HField label="启用直连" hint="关掉后播放退回 Emby 中转，端口本身仍然可用">
          <button
            class="switch"
            :aria-checked="emby.gwOn ? 'true' : 'false'"
            @click="emby.gwOn = !emby.gwOn"
          />
        </HField>
        <HField label="不接管的客户端" hint="按 UA 子串匹配，命中就继续走 Emby 中转（个别播放器不跟 302）">
          <input v-model="emby.gwBypass" class="input" placeholder="例如：TV Box, Kodi" />
        </HField>
        <p class="muted small" style="margin: 0 0 12px; line-height: 1.7">
          播放器地址就填 <code class="mono">http://内网IP:6086</code>（公网用 <code class="mono">emby.你的域名</code>）——
          它和 .strm 里的 <code class="mono">/d/</code> 是同一个入口，原来那个独立的 18096 端口已经取消。
          转码请求不会被动，仍然走 Emby。
        </p>
        <template #footer>
          <button class="btn btn-primary" :disabled="embyBusy" @click="saveGateway">
            {{ embyBusy ? '保存中…' : '保存并应用' }}
          </button>
        </template>
      </HPanel>

      <p class="muted small" style="margin: 0; line-height: 1.7">
        说明：Emby webhook 尚未接入，目前新增/删除条目靠同步任务与「扫描」按钮触发；
        这个面板只做已上线的能力，不再摆一个点了没反应的开关。
      </p>
    </template>

    <!-- 安全 -->
    <template v-else-if="tab === 'security'">
      <HPanel title="管理员账号" desc="改密码后其它设备的会话立即失效">
        <HField label="当前密码" required>
          <input v-model="sec.current" class="input" type="password" style="max-width: 320px" />
        </HField>
        <HField label="新密码" required hint="至少 6 位">
          <input v-model="sec.next" class="input" type="password" style="max-width: 320px" />
        </HField>
        <HField label="确认新密码" required>
          <input v-model="sec.confirm" class="input" type="password" style="max-width: 320px" />
        </HField>
        <template #footer>
          <button class="btn btn-primary" @click="changePwd">保存新密码</button>
        </template>
      </HPanel>

      <HPanel title="两步验证" desc="登录时多要一次 6 位验证码">
        <HField label="启用 OTP" hint="用任意 TOTP App 扫码，丢失后需要改数据库才能找回">
          <label class="checkline"><HSwitch v-model="sec.otp" />登录需要验证码</label>
        </HField>
        <template #footer>
          <button class="btn btn-tertiary" @click="toast.info('生成新的密钥')">重新生成密钥</button>
          <button class="btn btn-primary" @click="toast.success('两步验证设置已保存')">保存</button>
        </template>
      </HPanel>

      <HPanel title="最近登录" desc="发现陌生 IP 就换密码" flush>
        <div class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>时间</th>
                <th>IP</th>
                <th>设备</th>
                <th>结果</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td class="mono small">今天 21:30</td>
                <td class="mono small">192.168.200.17</td>
                <td class="small muted">Chrome · Windows</td>
                <td><span class="badge badge-success">成功</span></td>
              </tr>
              <tr>
                <td class="mono small">昨天 08:12</td>
                <td class="mono small">192.168.200.17</td>
                <td class="small muted">Safari · iPhone</td>
                <td><span class="badge badge-success">成功</span></td>
              </tr>
            </tbody>
          </table>
        </div>
      </HPanel>
    </template>

    <!-- 备份 -->
    <template v-else-if="tab === 'backup'">
      <HPanel title="备份恢复" desc="备份包含全部配置与数据库；恢复需要重启才生效">
        <HField label="自动备份">
          <label class="checkline"><HSwitch v-model="backup.enabled" />定时打包配置与台账</label>
        </HField>
        <HField label="备份时间" hint="cron 表达式，默认每天 04:00">
          <input v-model="backup.cron" class="input mono" style="max-width: 200px" />
        </HField>
        <HField label="保留份数">
          <input v-model.number="backup.keep" class="input" type="number" min="1" style="max-width: 120px" />
        </HField>
        <HField label="目录" hint="留空 = /data/backups">
          <input v-model="backup.dir" class="input mono" />
        </HField>
        <template #footer>
          <button class="btn btn-tertiary" @click="toast.info('正在打包…')">
            <AppIcon name="download" :size="14" />立即备份
          </button>
          <button class="btn btn-primary" @click="toast.success('备份设置已保存')">保存配置</button>
        </template>
      </HPanel>

      <HPanel title="备份列表" desc="恢复前会先把当前状态也备份一份" flush>
        <div class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>文件</th>
                <th>大小</th>
                <th>时间</th>
                <th>来源</th>
                <th style="width: 140px">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="b in backups" :key="b.name">
                <td class="mono small">{{ b.name }}</td>
                <td class="mono small">{{ b.size }}</td>
                <td class="muted small">{{ b.at }}</td>
                <td><span class="badge">{{ b.tag }}</span></td>
                <td>
                  <div class="row" style="gap: 2px">
                    <button class="btn btn-sm btn-ghost" @click="toast.info('下载备份')">
                      <AppIcon name="download" :size="14" />
                    </button>
                    <button class="btn btn-sm btn-ghost" @click="toast.warning('恢复需要重启，确认后执行')">恢复</button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </HPanel>
    </template>

    <!-- 版本 -->
    <template v-else>
      <HPanel title="版本" desc="当前运行的是哪个版本，以及本版改了什么">
        <div class="grid-3">
          <div class="tipbox">
            <p class="muted small">当前版本</p>
            <strong class="mono">{{ currentVersion }}</strong>
          </div>
          <div class="tipbox">
            <p class="muted small">运行时长</p>
            <strong class="mono">{{ Math.floor(session.uptimeSec / 60) }} 分钟</strong>
          </div>
          <div class="tipbox">
            <p class="muted small">版本来源</p>
            <strong class="mono small">VERSION 文件</strong>
          </div>
        </div>
        <p class="muted small" style="margin: 14px 0 0; line-height: 1.7">
          每次修复或功能更新都会递增版本号，并同步到镜像标签、二进制
          <span class="mono">-ldflags</span> 与前端。更新代码后在仓库根目录执行
          <code class="mono">docker compose up -d --build strm115</code> 重建容器，
          浏览器强刷（Ctrl+Shift+R）即可；<code class="mono">VERSION</code> 与
          <code class="mono">scripts/bump-version.py</code> 是唯一权威来源。
        </p>
      </HPanel>

      <HPanel :title="`近期更新 · ${currentVersion}`" desc="每次发版随代码一起写进来">
        <ul class="notes">
          <li v-for="(n, i) in releaseNotes" :key="i">{{ n }}</li>
        </ul>
      </HPanel>
    </template>
  </div>
</template>

<style scoped>
.checkline {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
.keylinks {
  display: flex;
  flex-direction: column;
  gap: 6px;
  max-width: 780px;
}
.keylink {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 9px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface-secondary);
}
.keylink-btn {
  flex: none;
  min-width: 132px;
  justify-content: flex-start;
  text-decoration: none;
}
.keylink-text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  flex: 1;
}
.keylink-url {
  font-size: 11.5px;
  color: var(--accent);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.keylink .btn-ghost {
  flex: none;
}
@media (max-width: 720px) {
  .keylink {
    flex-wrap: wrap;
  }
  .keylink-text {
    flex-basis: 100%;
  }
}
.net {
  padding: 13px 14px;
  border-radius: var(--r-md);
  border: 1px solid var(--border);
  background: var(--surface-secondary);
}
.net.bad {
  border-color: color-mix(in oklab, var(--danger) 28%, transparent);
}
.net p {
  margin-top: 4px;
}
.tipbox {
  padding: 14px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.tipbox strong {
  font-size: 16px;
}
.notes {
  margin: 0;
  padding-left: 18px;
  color: var(--muted);
  font-size: 13px;
  line-height: 2;
}
.notes code {
  padding: 1px 5px;
  border-radius: 5px;
  background: var(--surface-secondary);
}
</style>
