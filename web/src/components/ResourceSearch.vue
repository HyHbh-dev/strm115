<script setup lang="ts">
/**
 * 资源搜索（本项目自有模块）。
 *
 * 后端 GET /api/res/search 会并发搜两类源：
 *   - TG 公开频道（t.me/s/<频道>?q=），中文帖 + 英文标题帖各搜一遍
 *   - 种子站（海盗湾 / Nyaa / 动漫花园 / Mikan / UIndex / TheRarBG / 磁力猫），英文索引
 * 中文片名会用 TMDB（查不到再退回 IMDb）换英文名，输入框里直接打中文就行。
 *
 * 一轮搜索把两类源都搜回来，上面的「全部 / 种子站 / TG 频道」只做本地过滤，
 * 不会重新打网络；后端还有几分钟的结果缓存，想拿最新的点「重搜」。
 *
 * 来源清单存在设置项里（tg_channels / res_sites），这个页面直接改，不用去别处找。
 */
import { nextTick, onMounted, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import TitlePicker from '@/components/TitlePicker.vue'
import HPanel from '@/components/HPanel.vue'
import HField from '@/components/HField.vue'
import HSwitch from '@/components/HSwitch.vue'
import HEmpty from '@/components/HEmpty.vue'
import DirPicker from '@/components/DirPicker.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { fmtSize, KIND_FILTERS, SIZE_FILTERS, useResSearch } from '@/composables/useResSearch'
import type { ResItem } from '@/composables/useResSearch'
import { use115Share } from '@/composables/use115Share'
import { copyText } from '@/utils/format'

/**
 * 从别的页面/标签跳过来时预填的关键词。
 *
 * 「发现与订阅」顶部那个搜索框选完片就直接切到这个标签 —— 用户在那边已经
 * 认过片了，这里不该再让他把片名敲一遍、也不该再弹一次候选。
 * 用 runId 而不是 watch(initialQ)：同一部片连点两次也要能重新搜。
 */
const props = withDefaults(
  defineProps<{ initialQ?: string; initialEn?: string; runId?: number }>(),
  { initialQ: '', initialEn: '', runId: 0 },
)

/**
 * 详情不再整页跳走，而是交给父级（资源搜索页）开在右栏。
 *
 * 以前这里 router.push('/title')，靠 KeepAlive 保搜索页的状态；刷新一次或
 * 换个设备就没有缓存，回来是一屏空白。现在点结果只「选中」，不导航。
 */
const emit = defineEmits<{
  (
    e: 'open-detail',
    t: { type?: string; tmdbId?: string | number; q?: string; year?: string | number },
  ): void
}>()

const keyword = ref('')
const en = ref('')
const showEn = ref(false)

// 「先认片、再搜资源」：picked 是用户从 TMDB 候选里锁定的那部；
// pickerShown 控制候选面板显隐，pickerRef 用来预填关键词并触发查询。
// 这里本地声明一份和 TitlePicker 一致的候选结构，避免跨 .vue import 类型。
interface TitleHit {
  id: number
  kind: string
  title: string
  original: string
  year: string
  poster: string
  vote: number
  overview: string
  /** tmdb / douban —— 豆瓣条目没有 TMDB id，详情页只能按片名认 */
  source?: 'tmdb' | 'douban'
}
const picked = ref<TitleHit | null>(null)
const pickerShown = ref(false)
const pickerRef = ref<InstanceType<typeof TitlePicker> | null>(null)

const res = useResSearch()
const {
  busyKey: busy115,
  loadTarget: loadShareTarget,
  setTarget: setShareTarget,
  targetLabel,
  isBusy: isBusy115,
  offlineURI,
  save: saveTo115,
  offline: offlineTo115,
} = use115Share()
const dirPicker = ref(false)

/**
 * 夸克转存：和 115 那条完全独立。
 * TG 里夸克方向的频道（yunpanquark / quarkshare）全是 pan.quark.cn 分享，
 * 后端已经认出 quark_share / quark_pwd，这里点一下就走「网页 Cookie 转存」。
 * 播放不在这一步：转存完由夸克TV 的 302 直链接管。
 */
const quarkBusy = ref("")

function quarkKey(r: ResItem) {
  return r.source_key + r.title
}

async function saveToQuark(r: ResItem) {
  const key = quarkKey(r)
  if (quarkBusy.value) return
  quarkBusy.value = key
  try {
    const out: any = await api.quark.save({
      url: r.quark_url || undefined,
      share_code: r.quark_share,
      passcode: r.quark_pwd,
    })
    toast.success(
      "已存入夸克",
      `${out?.title || r.title} · ${out?.files ?? 0} 个${out?.dir ? " → " + out.dir : ""}`,
    )
    if (out?.synced) toast.info("已自动跑一轮同步", "稍后就能在媒体库里看到")
  } catch (e: any) {
    // Cookie 过期是最常见的失败：后端已经把「去重新粘贴」写进消息里，别覆盖它。
    toast.warning("夸克转存失败", e?.message || "稍后再试")
  } finally {
    quarkBusy.value = ""
  }
}

/** 换转存目录：立即写设置，海报墙详情那边也用同一份 */
async function onPickDir(v: { cid: string; path: string }) {
  await setShareTarget(v.cid, v.path)
}

/**
 * 「搜索」/回车 = 新流程第一步：只弹 TMDB 候选，不直接搜资源。
 * 选中某部（onPick）或跳过（onSkip）之后才会 res.run —— 输入词 → 先返回
 * TMDB 信息 → 用户点选 → 才搜资源。关键词通过组件暴露的 setQuery 预填并
 * 立即查候选；WallBrowser 抽屉复用同一入口（打开抽屉时 setQuery 即可）。
 */
function openPicker(q: string) {
  const v = q.trim()
  if (!v) {
    toast.warning('先输入要搜的片名')
    return
  }
  pickerShown.value = true
  nextTick(() => pickerRef.value?.setQuery(v))
}

function search() {
  openPicker(keyword.value)
}

/** 点选候选：锁定中文名 + 英文原名，同一轮里 TG 与种子站都会用上原名 */
function onPick(hit: TitleHit) {
  keyword.value = hit.title
  en.value = hit.original || ''
  picked.value = hit
  pickerShown.value = false
  // 选中即看详情：右侧面板同步展示这部片的海报 / 简介 / 入库状态，
  // 左侧继续把资源搜回来 —— 不用先跳页面再回来。
  emit('open-detail', {
    type: hit.kind === 'tv' ? 'tv' : 'movie',
    tmdbId: hit.source === 'tmdb' ? hit.id || '' : '',
    q: hit.title,
    year: hit.year || '',
  })
  res.run({ q: hit.title, en: hit.original, refresh: true })
  toast.success(`已锁定《${hit.title}》，同时按英文原名搜 TG 与种子站`)
}

/** 跳过候选：退回旧行为，按关键词（可带手动英文名）直接搜 */
function onSkip(q: string) {
  picked.value = null
  pickerShown.value = false
  res.run({ q, en: en.value, refresh: true })
}

// 外部带着关键词跳进来：直接搜，不弹候选（那边已经认过片了）
watch(
  () => props.runId,
  () => {
    const q = props.initialQ.trim()
    if (!q) return
    keyword.value = q
    if (props.initialEn) {
      en.value = props.initialEn
      showEn.value = true
    }
    picked.value = null
    pickerShown.value = false
    res.run({ q, en: props.initialEn || '', refresh: true })
  },
)

/** 重搜：重搜当前锁定 / 关键词，不再弹候选（和 search 区分开） */
function rerun() {
  const q = picked.value?.title || keyword.value
  const e = picked.value?.original || en.value
  res.run({ q, en: e, refresh: true })
}

/**
 * 打开当前锁定影片的详情（右侧面板）。
 *
 * 搜索页以前没有这个入口：想看一眼海报/简介就得先回「发现与订阅」再点一次，
 * 回来还得重搜。现在详情开在右栏，左侧结果一动不动。
 */
function openTitle() {
  const hit = picked.value
  if (!hit) return
  emit('open-detail', {
    type: hit.kind === 'tv' ? 'tv' : 'movie',
    tmdbId: hit.source === 'tmdb' ? hit.id || '' : '',
    q: hit.title || keyword.value,
    year: hit.year || '',
  })
}

/**
 * 点结果行看详情。
 *
 * 已经锁定影视时直接看锁定那部 —— 整屏结果本来就是按它搜的；
 * 没锁定（直接按关键词搜）才拿行标题猜片名，交给后端问 TMDB。
 */
function openRow(r: ResItem) {
  if (picked.value) {
    openTitle()
    return
  }
  emit('open-detail', { q: guessTitle(r.title) })
}

/** 从种子/帖子标题里粗提片名：去年份之后的技术参数、去括号前缀与扩展名。 */
function guessTitle(raw: string) {
  let s = raw.replace(/\.(mkv|mp4|avi|ts|m2ts|iso|rmvb|flv)$/i, ' ')
  s = s.replace(/【[^】]*】|\[[^\]]*\]/g, ' ')
  s = s.replace(/(19|20)\d{2}\b.*$/g, ' ')
  s = s.replace(/[._]+/g, ' ')
  s = s.replace(/\s+/g, ' ').trim()
  return s || raw
}

/** 结果面板里的「换一部」：重新打开候选，预填锁定片名方便重选 */
function changeTitle() {
  openPicker(picked.value?.title || keyword.value)
}

function pickKind(v: string) {
  res.kind.value = v
}

// 模板 @click 直接绑 copy，保留名字；剪贴板与提示统一走 copyText。
async function copy(text?: string) {
  if (!text) {
    toast.warning('这条没有可复制的链接')
    return
  }
  await copyText(text)
}

function firstLink(it: { magnet?: string; torrent?: string; url?: string; links?: string[] }) {
  return it.magnet || it.torrent || it.url || it.links?.[0] || ''
}

/* ---------------- 来源配置 ---------------- */
// 内置种子站。key 对应后端 resource.Sites()。
const SITES = [
  { key: 'tpb', name: '海盗湾', desc: '英文综合站，片源最全，中文搜不到需用英文名' },
  { key: 'nyaa', name: 'Nyaa', desc: '日系资源，动漫/日剧补全' },
  { key: 'dmhy', name: '动漫花园', desc: '中文字幕组，RSS 可按字幕组分流' },
  { key: 'mikan', name: 'Mikan', desc: '蜜柑计划，番剧按季整理' },
  { key: 'uindex', name: 'UIndex', desc: '影视/剧集表格站，磁力直接给；部分 IP 会遇 Cloudflare' },
  { key: 'therarbg', name: 'TheRarBG', desc: 'RARBG 镜像，按做种排序；磁力要翻详情页' },
  { key: 'clm', name: '磁力猫', desc: '中文 DHT 索引，中文片名直接搜；磁力由 infohash 拼出' },
]

// 与后端 resource.DefaultChannels 保持一致：只放 115 / 夸克方向的频道。
// 阿里、百度的频道留着没意义 —— 这个项目的转存只有 115 一条路。
const DEFAULT_CHANNELS = ['QukanMovie', 'yunpanquark', 'quarkshare']

interface TestResult {
  kind: string
  name: string
  ok: boolean
  count: number
  ms: number
  query?: string
  url?: string
  error?: string
  notice?: string
  samples?: string[]
  cached?: boolean
}

interface CustomRow {
  name: string
  url: string
  raw: string // 保存时原样写回，解析交给后端
}

const channelList = ref<string[]>([])
const customList = ref<CustomRow[]>([])
const enabledSites = ref<string[]>(SITES.map((s) => s.key))
const savedSig = ref('')

const newChannel = ref('')
const newCustom = ref('')

const loadingCfg = ref(false)
const savingCfg = ref(false)
const testingAll = ref(false)
// 测试结果按 target 存：频道名 / 站 key / 自定义源那一行，三者不会撞车。
const tests = ref<Record<string, TestResult>>({})
const testing = ref<Record<string, boolean>>({})

onMounted(async () => {
  await Promise.all([loadCfg(), loadShareTarget()])
})

/** 把 @xxx、t.me/xxx、尾部斜杠都收敛成纯频道名。 */
function normalizeChannel(raw: string) {
  let s = raw.trim()
  if (!s) return ''
  s = s.replace(/^@/, '')
  const i = s.toLowerCase().indexOf('t.me/')
  if (i >= 0) s = s.slice(i + 5).replace(/^s\//, '')
  return s.replace(/\/+$/, '').trim()
}

/**
 * 与后端 NormalizeChannels 对齐。Telegram 的 username 只能是 ASCII 字母数字下划线，
 * 「Torrent Search Bot 🔎」这类频道**显示名**不是 username —— 按空格拆会变成
 * 四个不存在的频道，每个都白打一次 t.me 还占并发槽。含空格又带非 ASCII 的整条丢掉。
 */
function parseChannelSetting(raw: string): string[] {
  const entries = String(raw || '').split(/[,，;；、\n\r]+/)
  const kept = entries.filter((e) => !(/\s/.test(e) && /[^\x00-\x7F]/.test(e)))
  return kept
    .join(',')
    .split(/[,，\s;；、]+/)
    .map(normalizeChannel)
    .filter((x) => /[A-Za-z0-9]/.test(x))
}

function uniq(list: string[]) {
  const seen = new Set<string>()
  return list.filter((x) => {
    const k = x.toLowerCase()
    if (!k || seen.has(k)) return false
    seen.add(k)
    return true
  })
}

function hostOf(u: string) {
  try {
    return new URL(u).host
  } catch {
    return u
  }
}

/** 前端的显示用解析，规则和后端 ParseCustomSites 对齐（真正的解析在后端）。 */
function parseCustomLine(line: string): CustomRow | null {
  const s = line.trim()
  if (!s || s.startsWith('#')) return null
  let name = ''
  let url = ''
  const byBar = s.match(/^([\s\S]+?)\s*[|｜]\s*(https?:\/\/\S+)$/i)
  if (byBar) {
    name = byBar[1].trim()
    url = byBar[2].trim()
  } else {
    const parts = s.split(/\s+/)
    const i = parts.findIndex((x) => /^https?:\/\//i.test(x))
    if (i >= 0) {
      url = parts[i]
      name = parts.slice(0, i).join(' ')
    }
  }
  if (!url) return null
  return { name: name || hostOf(url), url, raw: s }
}

function signature() {
  return JSON.stringify([channelList.value, enabledSites.value, customList.value.map((c) => c.raw)])
}

async function loadCfg() {
  loadingCfg.value = true
  try {
    const s: any = await api.config.settings()
    const chs = parseChannelSetting(s?.tg_channels)
    channelList.value = chs.length ? uniq(chs) : DEFAULT_CHANNELS.slice()

    const list = String(s?.res_sites || '')
      .split(/[,，\s]+/)
      .map((x) => x.trim().toLowerCase())
      .filter(Boolean)
    // 没配过就用后端默认（四站全开）
    enabledSites.value = list.length ? list : SITES.map((x) => x.key)

    customList.value = String(s?.res_sites_custom || '')
      .split('\n')
      .map(parseCustomLine)
      .filter((x): x is CustomRow => !!x)

    savedSig.value = signature()
  } catch {
    /* 拉不到就用默认值，搜索不受影响 */
  } finally {
    loadingCfg.value = false
  }
}

function toggleSite(key: string, on: boolean) {
  const set = new Set(enabledSites.value)
  if (on) set.add(key)
  else set.delete(key)
  enabledSites.value = SITES.map((s) => s.key).filter((k) => set.has(k))
}

function addChannel() {
  const ch = normalizeChannel(newChannel.value)
  if (!ch) return
  // 频道显示名（带空格 / emoji / 中文）不是 username，贴进来只会白打请求。
  if (/\s|[^\x00-\x7F]/.test(ch)) {
    toast.warning('要填频道 username（如 QukanMovie），不是频道显示名')
    return
  }
  // 以 bot 结尾的是机器人，没有 t.me/s 网页预览页，加进来永远 0 条结果。
  // 这类机器人（搜索结果转发号）能搜到内容的前提是有人把它的结果**转发进频道**，
  // 直接搜机器人本身是搜不到东西的。
  if (/bot$/i.test(ch)) {
    toast.warning(
      `@${ch} 是机器人，不是频道`,
      't.me 的网页预览只支持公开频道，机器人搜不到。请换成把结果转发出来的资源频道。',
    )
    return
  }
  if (channelList.value.some((x) => x.toLowerCase() === ch.toLowerCase())) {
    toast.warning('这个频道已经在列表里了')
    newChannel.value = ''
    return
  }
  channelList.value.push(ch)
  newChannel.value = ''
}

function removeChannel(ch: string) {
  channelList.value = channelList.value.filter((x) => x !== ch)
  delete tests.value[ch]
}

function addCustom() {
  const row = parseCustomLine(newCustom.value)
  if (!row) {
    toast.warning('地址不对', '应该像：我的站 | https://example.com/api?apikey=xxx&q={q}')
    return
  }
  if (customList.value.some((c) => c.url === row.url)) {
    toast.warning('这个地址已经加过了')
    return
  }
  customList.value.push(row)
  newCustom.value = ''
}

function removeCustom(row: CustomRow) {
  customList.value = customList.value.filter((c) => c.raw !== row.raw)
  delete tests.value[row.raw]
}

/**
 * 测一个源。测的是「输入框里的值」而不是「已保存的值」——
 * 用户填完地址就想知道能不能用，不该逼他先保存。
 */
async function testOne(kind: 'tg' | 'site' | 'custom', target: string) {
  if (!target || testing.value[target]) return
  testing.value[target] = true
  try {
    const r: any = await api.transfer.resTest({ kind, target })
    tests.value[target] = r
  } catch (e: any) {
    tests.value[target] = {
      kind,
      name: target,
      ok: false,
      count: 0,
      ms: 0,
      error: e?.message || '测试失败',
    }
  } finally {
    testing.value[target] = false
  }
}

/**
 * 分批跑：t.me 对同一 IP 的并发很敏感，三个频道一起打过去经常有一个卡到超时。
 * 3 个一批既不慢（每批 1~2 秒），也不会把自己测成红的。
 */
async function mapLimit<T>(items: T[], limit: number, fn: (x: T) => Promise<unknown>) {
  const queue = items.slice()
  const workers = Array.from({ length: Math.min(limit, queue.length) }, async () => {
    for (let next = queue.shift(); next !== undefined; next = queue.shift()) {
      await fn(next)
    }
  })
  await Promise.all(workers)
}

async function testAll() {
  testingAll.value = true
  const jobs: Array<['tg' | 'site' | 'custom', string]> = [
    ...channelList.value.map((c) => ['tg', c] as ['tg', string]),
    ...enabledSites.value.map((k) => ['site', k] as ['site', string]),
    ...customList.value.map((c) => ['custom', c.raw] as ['custom', string]),
  ]
  await mapLimit(jobs, 3, ([k, t]) => testOne(k, t))
  testingAll.value = false
  const bad = jobs.filter(([, t]) => tests.value[t] && !tests.value[t].ok).length
  if (!bad) toast.success('全部可用', `测了 ${jobs.length} 个源`)
  else toast.warning(`${bad} 个源有问题`, '看每个源后面的红字')
}

async function saveCfg() {
  savingCfg.value = true
  try {
    await api.config.saveSettings({
      tg_channels: channelList.value.join(','),
      res_sites: enabledSites.value.join(','),
      res_sites_custom: customList.value.map((c) => c.raw).join('\n'),
    })
    savedSig.value = signature()
    toast.success('来源配置已保存', '下一轮搜索立即生效')
  } catch (e: any) {
    toast.warning('保存失败', e?.message)
  } finally {
    savingCfg.value = false
  }
}

const dirty = () => signature() !== savedSig.value

/** 测试结果的一句话（失败时尽量把原因说全）。 */
function testText(key: string) {
  const r = tests.value[key]
  if (!r) return ''
  if (!r.ok) return r.error || '失败'
  // 标一下缓存：否则「秒回 0ms」会让人以为 t.me 比本地还快
  return `${r.count} 条 · ${r.ms}ms${r.cached ? ' · 缓存' : ''}`
}

function testTone(key: string) {
  const r = tests.value[key]
  if (!r) return ''
  return r.ok ? 'badge-success' : 'badge-danger'
}

function testTip(key: string) {
  const r = tests.value[key]
  if (!r) return ''
  if (!r.ok) return r.error || ''
  const parts = [`探活词：${r.query || '（不带关键词）'}`]
  if (r.notice) parts.push(r.notice)
  if (r.url) parts.push(r.url)
  if (r.samples?.length) parts.push(...r.samples.map((s) => '· ' + s))
  return parts.join('\n')
}
</script>

<template>
  <div class="col" style="gap: 16px">
    <!-- 搜索 -->
    <div class="panel search-hero">
      <div class="col" style="gap: 12px">
        <div>
          <h2 style="font-size: 18px">资源搜索</h2>
          <p class="muted small" style="margin-top: 4px">
            TG 公开频道 + 种子站并发检索。输入中文片名即可，会自动用 TMDB 换英文名再搜；
            一次搜全部来源，下面的切换只是筛选，不会重新联网。
          </p>
        </div>

        <div class="row wrap" style="gap: 8px">
          <div class="search" style="flex: 1; min-width: 260px; max-width: 560px">
            <AppIcon name="search" :size="16" />
            <input
              v-model="keyword"
              class="input"
              placeholder="片名，如：沙丘2 / 三体 / Dune"
              @keydown.enter="search"
            />
          </div>
          <button class="btn btn-primary" :disabled="res.loading.value" @click="search">
            {{ res.loading.value ? '搜索中…' : '搜索' }}
          </button>
          <button class="btn btn-tertiary" @click="showEn = !showEn">
            {{ showEn ? '收起英文名' : '指定英文名' }}
          </button>
        </div>

        <div v-if="showEn" class="row" style="gap: 8px; max-width: 560px">
          <input
            v-model="en"
            class="input mono"
            placeholder="英文原名（留空则由后端查 TMDB / IMDb 自动换）"
            @keydown.enter="search"
          />
        </div>

        <div class="row wrap" style="gap: 6px">
          <button
            v-for="k in KIND_FILTERS"
            :key="k.v"
            class="chip"
            :class="{ 'is-active': res.kind.value === k.v }"
            @click="pickKind(k.v)"
          >
            {{ k.l }}
          </button>
          <span class="grow" />
          <button
            v-for="s in SIZE_FILTERS"
            :key="s.v"
            class="chip"
            :class="{ 'is-active': res.minSize.value === s.v }"
            @click="res.minSize.value = s.v"
          >
            {{ s.l }}
          </button>
        </div>

        <!-- 候选面板：search() 打开，选中 / 跳过之后才真正搜资源 -->
        <TitlePicker
          v-if="pickerShown"
          ref="pickerRef"
          :initial="keyword"
          @pick="onPick"
          @skip="onSkip"
        />
      </div>
    </div>

    <!-- 来源状态 -->
    <div v-if="res.result.value?.sources?.length" class="row wrap" style="gap: 8px">
      <span
        v-for="s in res.result.value.sources"
        :key="s.key"
        class="badge"
        :class="s.ok ? 'badge-success' : 'badge-danger'"
        :title="s.error || ''"
      >
        <i class="dot" />{{ s.name }} · {{ s.ok ? `${s.count} 条 / ${s.ms}ms` : s.error || '失败' }}<template v-if="s.cached"> · 缓存</template>
      </span>
      <span v-if="res.lookupNote.value" class="badge badge-info">{{ res.lookupNote.value }}</span>
      <span v-if="res.cachedNote.value" class="badge">{{ res.cachedNote.value }}</span>
      <span class="muted small">耗时 {{ res.result.value.took_ms }}ms</span>
    </div>

    <!-- 结果 -->
    <HPanel
      title="搜索结果"
      :desc="
          res.result.value
            ? `「${res.result.value.query}」· ${res.items.value.length} 条` +
              (res.result.value.filtered ? `（已过滤 ${res.result.value.filtered} 条广告）` : '')
            : '输入片名后回车'
        "
    >
      <template #actions>
        <button v-if="res.result.value" class="btn btn-sm btn-tertiary" :disabled="res.loading.value" @click="rerun">
          <AppIcon name="refresh" :size="14" />重搜
        </button>
      </template>

      <!-- 当前锁定：明确告诉用户这一轮资源是按哪部片搜的 -->
      <div class="lock-bar" :class="{ locked: !!picked }">
        <template v-if="picked">
          <AppIcon name="check" :size="14" />
          <span class="small">
            当前锁定：<strong>《{{ picked.title }}》</strong>
            <span v-if="picked.original" class="muted">· {{ picked.original }}</span>
            <span v-if="picked.year" class="muted">· {{ picked.year }}</span>
            <span class="badge">{{ picked.kind === 'tv' ? '剧集' : '电影' }}</span>
          </span>
          <span class="grow" />
          <button class="btn btn-sm btn-tertiary" title="看海报、简介与入库状态" @click="openTitle">
            <AppIcon name="info" :size="14" />详情
          </button>
          <button class="btn btn-sm btn-tertiary" @click="changeTitle">换一部</button>
        </template>
        <span v-else class="muted small">未锁定影视 · 直接按关键词搜索</span>
      </div>

      <div v-if="res.loading.value" class="col" style="gap: 8px">
        <div v-for="i in 5" :key="i" class="skeleton" style="height: 58px; border-radius: var(--r-sm)" />
      </div>

      <div v-else class="col" style="gap: 8px">
        <div
          v-for="r in res.pagedItems.value"
          :key="r.source_key + r.title"
          class="res"
        >
          <!-- 整块主体（标题 + 元信息）都可点：以前只有标题那行文字能点，
               用户点行中间那一片毫无反应，看起来就像「搜索页根本没变」。 -->
          <div class="grow res-hit" title="在右侧查看这部片" @click="openRow(r)">
            <p class="res-title">{{ r.title }}</p>
            <div class="row wrap small muted" style="gap: 7px; margin-top: 3px">
              <span class="badge" :class="r.kind === 'tg' ? 'badge-info' : ''">{{ r.source }}</span>
              <!-- 认出来的 115 分享：码 + 提取码都亮出来，不用点开原帖核对 -->
              <span v-if="r.share_code" class="badge badge-success mono" :title="r.share_url">
                115 · {{ r.share_code }}<template v-if="r.share_pwd"> · {{ r.share_pwd }}</template>
              </span>
              <!-- 夸克分享：同样把码和提取码亮出来，方便核对 -->
              <span v-if="r.quark_share" class="badge mono" :title="r.quark_url">
                夸克 · {{ r.quark_share }}<template v-if="r.quark_pwd"> · {{ r.quark_pwd }}</template>
              </span>
              <span class="mono">{{ fmtSize(r.size) }}</span>
              <span v-if="r.seeders">· {{ r.seeders }} 做种</span>
              <span v-if="r.date">· {{ r.date }}</span>
              <span v-if="r.category" class="truncate" style="max-width: 220px">· {{ r.category }}</span>
            </div>
          </div>
          <div class="row" style="gap: 6px; flex: none">
            <!-- 显式的详情入口：整块主体也能点，但按钮更直观 -->
            <button class="btn btn-sm btn-ghost" title="在右侧查看详情" @click="openRow(r)">
              <AppIcon name="info" :size="14" />
            </button>
            <!-- 认出 115 分享 → 直接转存，不跳网页 -->
            <button
              v-if="r.share_code"
              class="btn btn-sm btn-primary"
              :disabled="!!busy115"
              :title="r.share_url"
              @click="saveTo115(r)"
            >
              <AppIcon name="download" :size="14" />{{ isBusy115(r) ? '转存中…' : '存到 115' }}
            </button>
            <!-- 夸克分享 → 存进夸克网盘（播放走夸克TV 直链，不经过 115） -->
            <button
              v-if="r.quark_share"
              class="btn btn-sm btn-tertiary"
              :disabled="!!quarkBusy"
              :title="r.quark_url"
              @click="saveToQuark(r)"
            >
              <AppIcon name="cloud" :size="14" />{{ quarkBusy === quarkKey(r) ? "转存中…" : "存到夸克" }}
            </button>
            <!-- 种子站结果 → 直接丢给 115 离线，不用先复制磁力 -->
            <button
              v-else-if="offlineURI(r)"
              class="btn btn-sm btn-primary"
              :disabled="!!busy115"
              :title="offlineURI(r)"
              @click="offlineTo115(r)"
            >
              <AppIcon name="download" :size="14" />{{ isBusy115(r) ? '提交中…' : '离线下载' }}
            </button>
            <a
              v-else-if="firstLink(r)"
              class="btn btn-sm btn-tertiary"
              :href="firstLink(r)"
              target="_blank"
              rel="noreferrer"
            >
              <AppIcon name="external" :size="14" />打开
            </a>

            <button
              v-if="r.magnet"
              class="btn btn-sm btn-ghost"
              title="复制磁力"
              @click="copy(r.magnet)"
            >
              <AppIcon name="link" :size="14" />
            </button>
            <button
              v-if="r.torrent"
              class="btn btn-sm btn-ghost"
              title="复制种子直链"
              @click="copy(r.torrent)"
            >
              <AppIcon name="link" :size="14" />
            </button>
            <button
              v-if="r.links?.length && !r.share_code && !r.quark_share"
              class="btn btn-sm btn-ghost"
              title="复制网盘链接"
              @click="copy(r.links?.[0])"
            >
              <AppIcon name="link" :size="14" />
            </button>
          </div>
        </div>

        <!-- 一次 5 条：下一页只翻页，不再打网络 -->
        <div v-if="res.totalPages.value > 1" class="pager">
          <button class="btn btn-sm btn-tertiary" :disabled="res.page.value <= 1" @click="res.prevPage()">
            上一页
          </button>
          <span class="small muted">第 {{ Math.min(res.page.value, res.totalPages.value) }} / {{ res.totalPages.value }} 页</span>
          <button
            class="btn btn-sm btn-primary"
            :disabled="res.page.value >= res.totalPages.value"
            @click="res.nextPage()"
          >
            下一页<AppIcon name="chevron-right" :size="14" />
          </button>
        </div>

        <HEmpty
          v-if="!res.items.value.length"
          :title="res.error.value || (res.result.value ? '这几个源都没搜到' : '还没开始搜')"
          :desc="
            res.result.value
              ? '换英文名再试，或放开体积筛选；也可以到下面开更多来源'
              : '中文名直接搜就行，后端会去 IMDb 换英文名'
          "
          icon="search"
        />
      </div>
    </HPanel>

    <!-- 来源配置 -->
    <HPanel
      title="资源来源"
      desc="TG 频道和种子站都可以自己加，每个源后面都能单独测一次"
    >
      <template #actions>
        <button class="btn btn-sm btn-tertiary" :disabled="testingAll" @click="testAll">
          {{ testingAll ? '测试中…' : '全部测试' }}
        </button>
        <button class="btn btn-sm btn-primary" :disabled="savingCfg || !dirty()" @click="saveCfg">
          {{ savingCfg ? '保存中…' : dirty() ? '保存' : '已保存' }}
        </button>
      </template>

      <!-- TG 频道 -->
      <div class="src-head">
        <strong class="small">TG 频道</strong>
        <span class="muted small">
          只留 115 / 夸克方向的频道 —— 别的网盘链接转不进 115，摆着只是噪音
        </span>
      </div>
      <div class="src-list">
        <div v-for="ch in channelList" :key="ch" class="src-row">
          <span class="avatar">TG</span>
          <div class="grow" style="min-width: 0">
            <div class="row wrap" style="gap: 8px">
              <strong class="small">@{{ ch }}</strong>
              <span
                v-if="tests[ch]"
                class="badge mono"
                :class="testTone(ch)"
                :title="testTip(ch)"
              >
                {{ testText(ch) }}
              </span>
            </div>
            <p class="muted small mono truncate">t.me/{{ ch }}</p>
          </div>
          <button
            class="btn btn-sm btn-tertiary"
            :disabled="!!testing[ch]"
            @click="testOne('tg', ch)"
          >
            {{ testing[ch] ? '测试中…' : '测试' }}
          </button>
          <button class="btn btn-sm btn-ghost" title="移除这个频道" @click="removeChannel(ch)">
            ×
          </button>
        </div>
        <div class="src-add">
          <input
            v-model="newChannel"
            class="input"
            placeholder="频道 username，如 @QukanMovie 或 t.me/s/QukanMovie"
            @keydown.enter="addChannel"
          />
          <button class="btn btn-tertiary" @click="addChannel">添加</button>
        </div>
        <p class="muted small" style="margin-top: 6px">
          要填 username，不是「Torrent Search Bot 🔎」这类频道显示名；显示名无法用 t.me/s/ 预览页检索。
        </p>
      </div>

      <!-- 内置种子站 -->
      <div class="src-head">
        <strong class="small">内置种子站</strong>
        <span class="muted small">打开开关才参与检索</span>
      </div>
      <div class="src-list">
        <div v-for="s in SITES" :key="s.key" class="src-row">
          <span class="avatar">{{ s.name.slice(0, 1) }}</span>
          <div class="grow" style="min-width: 0">
            <div class="row wrap" style="gap: 8px">
              <strong class="small">{{ s.name }}</strong>
              <span class="badge mono">{{ s.key }}</span>
              <span
                v-if="tests[s.key]"
                class="badge mono"
                :class="testTone(s.key)"
                :title="testTip(s.key)"
              >
                {{ testText(s.key) }}
              </span>
            </div>
            <p class="muted small truncate">{{ s.desc }}</p>
          </div>
          <button
            class="btn btn-sm btn-tertiary"
            :disabled="!!testing[s.key]"
            @click="testOne('site', s.key)"
          >
            {{ testing[s.key] ? '测试中…' : '测试' }}
          </button>
          <HSwitch
            :model-value="enabledSites.includes(s.key)"
            :label="s.name"
            @update:model-value="(v: boolean) => toggleSite(s.key, v)"
          />
        </div>
      </div>

      <!-- 自定义源 -->
      <div class="src-head">
        <strong class="small">自定义源</strong>
        <span class="muted small">
          填 RSS / Torznab 搜索地址，{q} 会被换成关键词；Jackett、Prowlarr 的地址直接粘
        </span>
      </div>
      <div class="src-list">
        <div v-for="c in customList" :key="c.raw" class="src-row">
          <span class="avatar">RSS</span>
          <div class="grow" style="min-width: 0">
            <div class="row wrap" style="gap: 8px">
              <strong class="small">{{ c.name }}</strong>
              <span
                v-if="tests[c.raw]"
                class="badge mono"
                :class="testTone(c.raw)"
                :title="testTip(c.raw)"
              >
                {{ testText(c.raw) }}
              </span>
            </div>
            <p class="muted small mono truncate" :title="c.url">{{ c.url }}</p>
          </div>
          <button
            class="btn btn-sm btn-tertiary"
            :disabled="!!testing[c.raw]"
            @click="testOne('custom', c.raw)"
          >
            {{ testing[c.raw] ? '测试中…' : '测试' }}
          </button>
          <button class="btn btn-sm btn-ghost" title="移除这个源" @click="removeCustom(c)">×</button>
        </div>
        <div class="src-add">
          <input
            v-model="newCustom"
            class="input mono"
            placeholder="我的站 | https://example.com/api?apikey=xxx&q={q}"
            @keydown.enter="addCustom"
          />
          <button class="btn btn-tertiary" @click="addCustom">添加</button>
        </div>
        <p v-if="!customList.length" class="muted small" style="padding: 0 2px">
          私有站、PT 站、小型资源站大多没有公开搜索页，但几乎都有 RSS（或 Jackett / Prowlarr
          聚合出来的 Torznab）。把地址填进来就能和内置站一起搜。
        </p>
      </div>

      <HField
        label="转存目录"
        tip="资源里认出 115 分享后，一键转存落到这里"
        hint="默认 115 根目录；海报墙详情里的「存到 115」也存这儿"
      >
        <div class="row" style="gap: 8px">
          <input :value="targetLabel()" class="input mono" readonly />
          <button class="btn btn-tertiary" @click="dirPicker = true">
            <AppIcon name="folder" :size="14" />选择
          </button>
        </div>
      </HField>

      <div class="grid-3" style="margin-top: 14px">
        <div class="tipbox">
          <strong>中文 → 英文</strong>
          <p class="muted small">命中中文片名时先查 IMDb 取官方英文名，再搜种子站。</p>
        </div>
        <div class="tipbox">
          <strong>相关性优先</strong>
          <p class="muted small">种子站返回热门无关项时按片名与年份重排，宁缺毋滥。</p>
        </div>
        <div class="tipbox">
          <strong>识别 115 分享</strong>
          <p class="muted small">
            TG 帖子的 115 链接 / 分享码会被正则认出来，点「存到 115」直接转存，不用打开网页。<br />
            夸克分享（yunpanquark / quarkshare 这类频道）会标出分享码，点「存到夸克」走网页
            Cookie 转存 —— 播放由夸克TV 的 302 直链接管，不经过 115。
          </p>
        </div>
      </div>
    </HPanel>

    <DirPicker v-model="dirPicker" @pick="onPickDir" />
  </div>
</template>

<style scoped>
.search-hero {
  padding: 20px 22px;
  background:
    radial-gradient(80% 140% at 10% 0%, var(--accent-soft), transparent 62%),
    var(--surface);
}
.lock-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 8px 10px;
  margin-bottom: 12px;
  border: 1px dashed var(--border);
  border-radius: var(--r-sm);
  background: var(--surface-secondary);
}
.lock-bar.locked {
  border-style: solid;
  border-color: var(--accent);
}
.res {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  border-radius: var(--r-sm);
  border: 1px solid var(--border);
  background: var(--surface);
}
.pager {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding-top: 10px;
}
.res-title {
  font-size: 13px;
  line-height: 1.5;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
/* 结果主体整块可点：手型 + hover 变色，让人看得出「这里能点」 */
.res-hit {
  cursor: pointer;
  border-radius: var(--r-sm);
  transition: color 0.12s ease;
}
.res-hit:hover .res-title {
  color: var(--accent);
  text-decoration: underline;
  text-underline-offset: 3px;
}
.res-hit:active {
  opacity: 0.75;
}
.src-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  margin: 4px 0 8px;
}
.src-head + .src-list + .src-head {
  margin-top: 20px;
}
.src-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.src-row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  border-radius: var(--r-sm);
  border: 1px solid var(--border);
  background: var(--surface-secondary);
}
/* 添加行：输入框吃掉剩余宽度，按钮不被挤扁 */
.src-add {
  display: flex;
  gap: 8px;
  margin-top: 2px;
}
.src-add .input {
  flex: 1;
  min-width: 0;
}
.avatar {
  flex: none;
  width: 30px;
  height: 30px;
  border-radius: 9px;
  display: grid;
  place-items: center;
  font-size: 10px;
  font-weight: 600;
  letter-spacing: 0.02em;
  color: var(--accent-foreground);
  background: var(--accent);
}
.tipbox {
  padding: 14px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.tipbox strong {
  font-size: 13px;
}
.tipbox p {
  margin-top: 4px;
  line-height: 1.6;
}
</style>
