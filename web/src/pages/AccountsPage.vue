<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import DirPicker from '@/components/DirPicker.vue'
import HField from '@/components/HField.vue'
import HPanel from '@/components/HPanel.vue'
import HTabs from '@/components/HTabs.vue'
import HModal from '@/components/HModal.vue'
import HEmpty from '@/components/HEmpty.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { fmtDate } from '@/utils/format'

interface Acct {
  id: number
  kind: string
  name: string
  created_at: string
  /** 夸克专用：网页 Cookie 有没有配好（没配就只能播放、不能转存分享） */
  quark_cookie_set?: boolean
}
interface LibSource {
  id: number
  library_id: number
  account_id: number
  account_kind: string
  account_name: string
  remote_cid: string
  remote_path: string
  last_sync_at?: string
  last_sync_files?: number
}
interface Lib {
  id: number
  account_id: number
  name: string
  remote_cid: string
  remote_path: string
  local_path: string
  collection_type: string
  server_name?: string
  last_sync_at?: string
  last_sync_files?: number
  created_at?: string
  /** 这个库挂的全部网盘目录：115 一份、夸克一份，都写进同一个本地目录 */
  sources?: LibSource[]
}
interface DirEntry {
  id: string
  name: string
  is_dir: boolean
  size: number
}

type QRState = 'idle' | 'waiting' | 'scanned' | 'allowed' | 'expired' | 'canceled' | 'error'

const route = useRoute()
// 影视库页的「新建媒体库」直接跳到这里，带 ?tab=library&new=1 一步打开建库弹窗。
const tab = ref<'account' | 'library'>(route.query.tab === 'library' ? 'library' : 'account')
const tabs = [
  { value: 'account', label: '账号' },
  { value: 'library', label: '媒体库' },
]

const accounts = ref<Acct[]>([])
const libs = ref<Lib[]>([])
/** 每个账号今天的 115 API 调用量（含进程启动以来的总量，放 title 里）
 *  115 的风控额度按天算，用户想知道的是「今天还剩多少余量」，不是进程跑了多久。 */
const calls = ref<Record<number, { today: number; total: number }>>({})
const loading = ref(true)
const loadErr = ref('')

async function loadAccounts() {
  try {
    accounts.value = (await api.storage.accounts()) ?? []
    loadErr.value = ''
  } catch (e: any) {
    loadErr.value = e?.message || '读取账号失败'
  }
}
async function loadLibs() {
  try {
    libs.value = (await api.libraries.list()) ?? []
  } catch {
    /* 媒体库读不到不阻塞账号页 */
  }
}
async function reload() {
  loading.value = true
  await Promise.all([loadAccounts(), loadLibs(), loadDirSettings()])
  loading.value = false
  void loadCalls()
}

/* ---------------- 目录设置：转存目录 + 媒体库目录 ----------------
 * 这两处以前散在「资源搜索」页和「新建库」弹窗里，媒体库页完全看不到。
 * 现在集中到「媒体库」页签：转存目录可直接改（写同一批设置项），
 * 媒体库目录是容器里的 MEDIA_ROOT，只读展示并列每个库的子目录。 */
const dirCfg = ref({
  res_save_cid: '0',
  res_save_path: '',
  quark_save_fid: '',
  media_root: '/media',
})
const saveDirPicker = ref(false)
const saveDirKind = ref<'115' | 'quarktv'>('115')
const saveDirAccountId = ref(0)

async function loadDirSettings() {
  const [s, i] = await Promise.allSettled([api.config.settings(), api.dashboard.instance()])
  const cfg: any = s.status === 'fulfilled' ? s.value : {}
  const inst: any = i.status === 'fulfilled' ? i.value : {}
  dirCfg.value = {
    res_save_cid: String(cfg?.res_save_cid || '0'),
    res_save_path: String(cfg?.res_save_path || ''),
    quark_save_fid: String(cfg?.quark_save_fid || ''),
    media_root: String(inst?.media_root || '/media'),
  }
}

const resSaveLabel = computed(() => dirCfg.value.res_save_path || '115 根目录')
const quarkSaveLabel = computed(() =>
  dirCfg.value.quark_save_fid
    ? `目录 ${dirCfg.value.quark_save_fid.slice(0, 12)}…`
    : '夸克根目录',
)
const libDirs = computed(() =>
  libs.value.map((l) => ({
    name: l.name,
    path: `${dirCfg.value.media_root}/${l.local_path || l.name}`.replace(/\/+/g, '/'),
  })),
)

function openSaveDir(kind: '115' | 'quarktv') {
  const a = accounts.value.find((x) => x.kind === kind)
  if (!a) {
    toast.warning(`还没有登录 ${kind === '115' ? '115' : '夸克TV'} 账号`, '先到「账号」页签登录')
    return
  }
  saveDirKind.value = kind
  saveDirAccountId.value = a.id
  saveDirPicker.value = true
}

async function onPickSaveDir(v: { cid: string; path: string }) {
  saveDirPicker.value = false
  try {
    if (saveDirKind.value === 'quarktv') {
      // 根目录在设置项里用留空表示；normalizeQuarkFID 只认 32 位 fid。
      const fid = v.cid === '0' ? '' : v.cid
      await api.config.saveSettings({ quark_save_fid: fid })
      dirCfg.value.quark_save_fid = fid
      toast.success('夸克转存目录已保存', v.path)
    } else {
      await api.config.saveSettings({ res_save_cid: v.cid, res_save_path: v.path })
      dirCfg.value.res_save_cid = v.cid
      dirCfg.value.res_save_path = v.path
      toast.success('115 转存目录已保存', v.path)
    }
  } catch (e: any) {
    toast.warning('保存失败', e?.message || '')
  }
}

/** 拉每个账号的调用量。失败就留空，不阻塞账号列表 —— 这是个附带信息。 */
async function loadCalls() {
  await Promise.all(
    accounts.value.map(async (a) => {
      try {
        const r: any = await api.storage.accountStats(a.id)
        const st = r?.stats ?? {}
        calls.value = {
          ...calls.value,
          [a.id]: { today: Number(st.requests_today) || 0, total: Number(st.requests) || 0 },
        }
      } catch {
        /* 见上 */
      }
    }),
  )
}
onMounted(async () => {
  await reload()
  if (route.query.new === '1' && route.query.tab === 'library') openLib()
})

/* ---------------- 扫码登录 ---------------- */
const qr = ref<{ id: string; png: string } | null>(null)
const qrStatus = ref<QRState>('idle')
const qrMsg = ref('')
const qrBusy = ref(false)
let pollTimer: number | null = null
let qrDeadline = 0
let polling = false
/** 停表后再回来的响应不许再排下一轮（页面卸载时靠它收尾） */
let stopped = false

function stopPoll() {
  stopped = true
  if (pollTimer !== null) {
    window.clearTimeout(pollTimer)
    pollTimer = null
  }
  polling = false
}

function schedule() {
  if (stopped || !qr.value) return
  if (pollTimer !== null) window.clearTimeout(pollTimer)
  pollTimer = window.setTimeout(pollQR, 1200)
}

async function startQR() {
  qrBusy.value = true
  stopPoll()
  stopped = false
  qrStatus.value = 'idle'
  qrMsg.value = ''
  try {
    const r = await api.storage.qrStart()
    qr.value = { id: r.id, png: r.png }
    qrDeadline = Date.now() + (Number(r.expires_in) || 180) * 1000
    qrStatus.value = 'waiting'
    qrMsg.value = '等待扫码'
    schedule()
  } catch (e: any) {
    qrStatus.value = 'error'
    qrMsg.value = e?.message || '获取二维码失败，检查服务器到 115 的连通性'
  } finally {
    qrBusy.value = false
  }
}

/**
 * 串行轮询：等上一次响应回来（或超时）再发下一次。
 * 115 的状态接口是长轮询，用 setInterval 会同时压十几个在途请求。
 */
async function pollQR() {
  if (!qr.value || polling) return
  if (Date.now() > qrDeadline) {
    stopPoll()
    qrStatus.value = 'expired'
    qrMsg.value = '二维码已过期，点「刷新二维码」重来'
    return
  }
  polling = true
  try {
    const r = await api.storage.qrPoll(qr.value.id)
    qrStatus.value = (r?.status || 'waiting') as QRState
    qrMsg.value = r?.msg || ''
    if (r?.status === 'allowed') {
      stopPoll()
      qr.value = null
      qrStatus.value = 'idle'
      toast.success('115 账号已添加', r?.account?.name ? `账号：${r.account.name}` : '')
      await reload()
      return
    }
    if (r?.status === 'expired' || r?.status === 'canceled') {
      stopPoll()
      return
    }
  } catch (e: any) {
    // 客户端超时（115 那边还没结果）不算失败，接着轮询就行
    const msg = String(e?.message || '')
    if (!/abort|timeout|超时/i.test(msg)) {
      qrStatus.value = 'error'
      qrMsg.value = msg || '查询扫码状态失败'
      stopPoll()
      return
    }
  } finally {
    polling = false
  }
  if (qr.value) schedule()
}

onBeforeUnmount(() => {
  stopPoll()
  stopQkPoll()
  if (keepTimer !== null) window.clearInterval(keepTimer)
  stopWebQR()
})
/* ---------------- 夸克TV：扫码登录（TV 接口，负责播放） ---------------- */
/**
 * 夸克登录和 115 完全不是一套：这里扫的是夸克TV 的开放接口，
 * 换回来的是 refresh_token（长期有效），只用来列目录和取 302 直链。
 * 分享转存需要另配一份网页 Cookie，见下面的「转存 Cookie」。
 */
const addKind = ref<'115' | 'quarktv'>('115')
const qkQR = ref<{ id: string; png: string } | null>(null)
const qkStatus = ref<'idle' | 'waiting' | 'ok' | 'expired' | 'error'>('idle')
const qkMsg = ref('')
const qkBusy = ref(false)
let qkTimer: number | null = null
let qkPolling = false
let qkDeadline = 0
let qkStopped = false

function stopQkPoll() {
  qkStopped = true
  if (qkTimer !== null) {
    window.clearTimeout(qkTimer)
    qkTimer = null
  }
  qkPolling = false
}

function scheduleQk() {
  if (qkStopped || !qkQR.value) return
  if (qkTimer !== null) window.clearTimeout(qkTimer)
  qkTimer = window.setTimeout(pollQkQR, 1500)
}

async function startQkQR() {
  qkBusy.value = true
  stopQkPoll()
  qkStopped = false
  qkStatus.value = 'idle'
  qkMsg.value = ''
  try {
    const r = await api.quark.qrStart()
    qkQR.value = { id: r.id, png: r.png }
    qkDeadline = Date.now() + (Number(r.expires_in) || 180) * 1000
    qkStatus.value = 'waiting'
    qkMsg.value = '等待扫码'
    scheduleQk()
  } catch (e: any) {
    qkStatus.value = 'error'
    qkMsg.value = e?.message || '获取二维码失败，检查服务器到夸克的连通性'
  } finally {
    qkBusy.value = false
  }
}

async function pollQkQR() {
  if (!qkQR.value || qkPolling) return
  if (Date.now() > qkDeadline) {
    stopQkPoll()
    qkStatus.value = 'expired'
    qkMsg.value = '二维码已过期，点「刷新二维码」重来'
    return
  }
  qkPolling = true
  try {
    const r: any = await api.quark.qrPoll(qkQR.value.id)
    if (r?.status === 'ok') {
      stopQkPoll()
      qkQR.value = null
      qkStatus.value = 'idle'
      toast.success('夸克账号已添加', r?.account?.name ? `账号：${r.account.name}` : '')
      await reload()
      return
    }
    if (r?.status === 'expired') {
      stopQkPoll()
      qkStatus.value = 'expired'
      qkMsg.value = r?.msg || '二维码已过期'
      return
    }
    qkStatus.value = 'waiting'
  } catch (e: any) {
    const msg = String(e?.message || '')
    if (!/abort|timeout|超时/i.test(msg)) {
      qkStatus.value = 'error'
      qkMsg.value = msg || '查询扫码状态失败'
      stopQkPoll()
      return
    }
  } finally {
    qkPolling = false
  }
  if (qkQR.value) scheduleQk()
}

/* ---------------- 夸克TV：网页 Cookie（负责转存分享） ---------------- */
const qkCookieFor = ref<Acct | null>(null)
const qkCookie = ref('')
const qkCookieSaving = ref(false)
/** 转存落点：夸克目录 fid，留空 = 根目录（设置项 quark_save_fid） */
const qkSaveFid = ref('')
/** 选中目录的可读名字，只在本次弹窗里显示，不落库 */
const qkSaveDirName = ref('')
const qkDirPicker = ref(false)
const qkDirLabel = computed(() => {
  if (qkSaveDirName.value) return qkSaveDirName.value
  if (qkSaveFid.value) return `已保存的目录 ${qkSaveFid.value.slice(0, 8)}…`
  return '根目录（默认）'
})

function onPickQkDir(v: { cid: string; path: string; name: string }) {
  // 根目录在设置项里用留空表示；normalizeQuarkFID 只认 32 位 fid。
  qkSaveFid.value = v.cid === '0' ? '' : v.cid
  qkSaveDirName.value = v.cid === '0' ? '根目录' : v.path
}

function clearQkDir() {
  qkSaveFid.value = ''
  qkSaveDirName.value = '根目录'
}
const qkTestMsg = ref<Record<number, string>>({})

/** Cookie 自动保活状态：后端每 ~100 分钟续一次 __puus，这里只读来显示。 */
interface KeepState {
  account_id: number
  cookie_set: boolean
  ok: boolean
  last_at?: string
  last_error?: string
  next_at: string
  refreshes: number
}
const keep = ref<Record<number, KeepState>>({})
let keepTimer: number | null = null

async function loadKeep() {
  try {
    const list: any = await api.quark.keepalive()
    const next: Record<number, KeepState> = {}
    for (const st of Array.isArray(list) ? list : []) next[st.account_id] = st
    keep.value = next
  } catch {
    /* 保活状态读不到不影响账号页其它功能 */
  }
}

function keepText(id: number): string {
  const st = keep.value[id]
  if (!st) return '等待首次检查'
  if (st.last_error) return '续期失败：' + st.last_error
  const next = st.next_at
    ? new Date(st.next_at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
    : ''
  return `自动续期${st.refreshes ? ` · 已续 ${st.refreshes} 次` : ''}${next ? ` · 下次 ${next}` : ''}`
}

async function renewNow() {
  try {
    await api.quark.keepaliveNow()
    await loadKeep()
    toast.success('已检查 Cookie', '有效就续期，没配或已失效会在这里写明')
  } catch (e: any) {
    toast.warning('续期失败', e?.message || '')
  }
}

/* ---------------- 网页 Cookie：扫码登录（不用 F12） ----------------
   夸克服务端给二维码约 120 秒窗口，过期后手机会直接报「登录请求过期」。
   所以这里做三件事：按后端给的 expires_in 倒计时、到点自动换新码、
   上游明确说过期时立刻换新码，用户不用守着一张废码反复点刷新。 */
const webQR = ref<{ id: string; png: string; qr_url: string } | null>(null)
const webQRStatus = ref<'idle' | 'waiting' | 'ok' | 'expired' | 'error'>('idle')
const webQRMsg = ref('')
const webQRBusy = ref(false)
const webQRLeft = ref(0)
let webQRTimer: number | null = null
let webQRCountTimer: number | null = null
let webQRPolling = false
let webQRStopped = false
let webQRAutoRefresh = 0

function stopWebQRCountdown() {
  if (webQRCountTimer !== null) {
    window.clearInterval(webQRCountTimer)
    webQRCountTimer = null
  }
}

function stopWebQR() {
  webQRStopped = true
  if (webQRTimer !== null) {
    window.clearTimeout(webQRTimer)
    webQRTimer = null
  }
  stopWebQRCountdown()
  webQRPolling = false
}

function scheduleWebQR() {
  if (webQRStopped || !webQR.value) return
  if (webQRTimer !== null) window.clearTimeout(webQRTimer)
  webQRTimer = window.setTimeout(() => void pollWebQR(), 1800)
}

function startWebQRCountdown(seconds: number) {
  stopWebQRCountdown()
  const deadline = Date.now() + Math.max(5, seconds || 110) * 1000
  const tick = () => {
    webQRLeft.value = Math.max(0, Math.ceil((deadline - Date.now()) / 1000))
    if (webQRLeft.value === 0) {
      stopWebQRCountdown()
      void refreshWebQRAuto()
    }
  }
  tick()
  webQRCountTimer = window.setInterval(tick, 1000)
}

/** 过期自动换码；连着换两次还没扫上就停下，别在后台无限刷夸克接口。 */
async function refreshWebQRAuto() {
  if (webQRAutoRefresh >= 2) {
    stopWebQR()
    webQRStatus.value = 'expired'
    webQRMsg.value = '二维码反复过期，请检查网络后点「刷新二维码」；也可以直接手动粘贴 Cookie'
    return
  }
  webQRAutoRefresh++
  webQRMsg.value = '二维码已过期，正在自动换一张…'
  await startWebQR(false)
}

/** manual=true 是用户点按钮，重置自动刷新计数；false 是过期自动换码。 */
async function startWebQR(manual = true) {
  const acct = qkCookieFor.value
  if (!acct) return
  if (manual) webQRAutoRefresh = 0
  const wasAuto = !manual
  webQRBusy.value = true
  stopWebQR()
  webQRStopped = false
  webQRStatus.value = 'idle'
  webQRMsg.value = ''
  try {
    const r: any = await api.quark.webQrStart(acct.id)
    webQR.value = { id: r.id, png: r.png, qr_url: r.qr_url || '' }
    webQRStatus.value = 'waiting'
    webQRMsg.value = wasAuto
      ? '已换一张新二维码，请用手机夸克 App 重新扫'
      : '用手机夸克 App 扫一扫 → 确认登录'
    startWebQRCountdown(Number(r?.expires_in) || 110)
    scheduleWebQR()
  } catch (e: any) {
    webQRStatus.value = 'error'
    webQRMsg.value = e?.message || '生成二维码失败'
  } finally {
    webQRBusy.value = false
  }
}

async function pollWebQR() {
  if (!webQR.value || webQRPolling) return
  webQRPolling = true
  try {
    const r: any = await api.quark.webQrPoll(webQR.value.id)
    if (r?.status === 'ok') {
      stopWebQR()
      webQR.value = null
      webQRStatus.value = 'ok'
      qkCookie.value = ''
      qkCookieFor.value = null
      toast.success('夸克 Cookie 已扫码获取', '系统会自动续期，不用再开 F12')
      await reload()
      await loadKeep()
      return
    }
    if (r?.status === 'expired') {
      // 上游 50004002 / Token Not Found：立即换新码，不再盯着废码轮询。
      stopWebQR()
      webQRMsg.value = r?.msg || '夸克提示登录请求已过期，正在换一张新的'
      void refreshWebQRAuto()
      return
    }
    webQRStatus.value = 'waiting'
  } catch (e: any) {
    const msg = String(e?.message || '')
    if (!/abort|timeout|超时/i.test(msg)) {
      webQRStatus.value = 'error'
      webQRMsg.value = msg || '查询扫码状态失败'
      stopWebQR()
      return
    }
  } finally {
    webQRPolling = false
  }
  if (webQR.value) scheduleWebQR()
}

async function openQkCookie(a: Acct) {
  qkCookieFor.value = a
  qkCookie.value = ''
  qkSaveFid.value = ''
  qkSaveDirName.value = ''
  qkDirPicker.value = false
  webQRAutoRefresh = 0
  webQRLeft.value = 0
  stopWebQR()
  webQR.value = null
  webQRStatus.value = 'idle'
  webQRMsg.value = ''
  try {
    const cfg: any = await api.config.settings()
    qkSaveFid.value = String(cfg?.quark_save_fid || '')
  } catch {
    /* 设置读不到不影响配 Cookie */
  }
}

async function saveQkCookie() {
  const a = qkCookieFor.value
  if (!a) return
  const raw = qkCookie.value.trim()
  if (!raw) {
    toast.warning('先粘贴夸克网页 Cookie')
    return
  }
  qkCookieSaving.value = true
  try {
    await api.quark.setCookie(a.id, raw)
    // 落点目录只改设置项：每次转存都从设置里读，机器人那边也用同一份。
    await api.config.saveSettings({ quark_save_fid: qkSaveFid.value.trim() })
    qkCookie.value = ''
    qkCookieFor.value = null
    toast.success('夸克 Cookie 已保存', '现在可以把 TG 里的夸克分享一键存进来了')
    await loadAccounts()
  } catch (e: any) {
    toast.warning('保存失败', e?.message || '')
  } finally {
    qkCookieSaving.value = false
  }
}

function fmtTB(n: number): string {
  return n ? (n / 1024 ** 4).toFixed(2) + ' TB' : '—'
}

/** 测试按钮要把「能不能真的拿到夸克数据」说清楚，光 Ping 通不算数。 */
async function testQuark(a: Acct) {
  qkTestMsg.value = { ...qkTestMsg.value, [a.id]: '测试中…' }
  try {
    const r: any = await api.quark.test(a.id)
    const parts: string[] = []
    parts.push(r?.tv_ok ? '播放 ✓' : '播放 ✗ ' + (r?.tv_error || ''))
    if (r?.tv_list_ok) parts.push(`列目录 ✓ ${r.tv_files} 项`)
    else if (r?.tv_list_error) parts.push('列目录 ✗ ' + r.tv_list_error)
    parts.push(r?.web_ok ? '转存 ✓' : '转存 ✗ ' + (r?.web_error || ''))
    if (r?.quota_ok) parts.push(`容量 ${fmtTB(r.quota_used)} / ${fmtTB(r.quota_total)}`)
    else if (r?.quota_error) parts.push(r.quota_error)
    if (r?.save_fid_fixed) parts.push(`转存目录已自动修正为 ${r.save_fid_fixed}`)
    qkTestMsg.value = { ...qkTestMsg.value, [a.id]: parts.join(' · ') }
  } catch (e: any) {
    qkTestMsg.value = { ...qkTestMsg.value, [a.id]: e?.message || '测试失败' }
  }
}






const qrHint = computed(() => {
  switch (qrStatus.value) {
    case 'waiting':
      return '打开 115 App → 扫一扫 → 确认登录'
    case 'scanned':
      return '已扫码，请在手机上点确认'
    case 'allowed':
      return '登录成功'
    case 'expired':
      return '二维码已过期'
    case 'canceled':
      return '已取消登录'
    case 'error':
      return '出错了'
    default:
      return '点下面的按钮生成二维码，2 分钟内有效'
  }
})

/* ---------------- Cookie 登录 ---------------- */
const addMode = ref<'qr' | 'cookie'>('qr')
const cookie = ref('')
const cookieName = ref('')
const saving = ref(false)

async function addByCookie() {
  const raw = cookie.value.trim()
  if (!raw) {
    toast.warning('先粘贴 115 Cookie')
    return
  }
  saving.value = true
  try {
    const a = await api.storage.cookieLogin(raw, cookieName.value.trim() || undefined)
    cookie.value = ''
    cookieName.value = ''
    toast.success('账号已添加', a?.name ? `账号：${a.name}` : '')
    await reload()
  } catch (e: any) {
    toast.warning('Cookie 登录失败', e?.message)
  } finally {
    saving.value = false
  }
}

/* ---------------- 探测凭据 ---------------- */
const probe = ref<Record<number, { state: 'ok' | 'fail' | 'busy'; msg: string }>>({})

async function check(a: Acct) {
  probe.value = { ...probe.value, [a.id]: { state: 'busy', msg: '探测中…' } }
  try {
    const r = await api.storage.browse(a.id, '0')
    const n = r?.entries?.length ?? 0
    probe.value = { ...probe.value, [a.id]: { state: 'ok', msg: `有效 · 根目录 ${n} 项` } }
  } catch (e: any) {
    probe.value = { ...probe.value, [a.id]: { state: 'fail', msg: e?.message || '探测失败' } }
  }
}

/**
 * 「凭据」这一行只放短状态，完整的后端报错放 title。
 * 之前把 "/file: page_size 范围: 1-100" 这类列目录错误整句塞进 flex 右侧，
 * 长文本会把左右两列撑歪（截图里的「错位」）。
 */
function probeText(id: number): string {
  const msg = probe.value[id]?.msg || '未探测'
  return msg.length > 20 ? msg.slice(0, 20) + '…' : msg
}

/* ---------------- 删除 ---------------- */
const delTarget = ref<{ kind: 'account' | 'library'; id: number; name: string } | null>(null)
const deleting = ref(false)

function askDelete(kind: 'account' | 'library', id: number, name: string) {
  delTarget.value = { kind, id, name }
}

async function doDelete() {
  const t = delTarget.value
  if (!t) return
  deleting.value = true
  try {
    if (t.kind === 'account') await api.storage.removeAccount(t.id)
    else await api.libraries.remove(t.id)
    toast.success(t.kind === 'account' ? '账号已删除' : '媒体库已删除')
    delTarget.value = null
    await reload()
  } catch (e: any) {
    toast.warning('删除失败', e?.message)
  } finally {
    deleting.value = false
  }
}

/* ---------------- 媒体库 ---------------- */
const libCount = (accountId: number) => libs.value.filter((l) => l.account_id === accountId).length
const accountName = (id: number) => accounts.value.find((a) => a.id === id)?.name || `#${id}`

const libModal = ref(false)
const creating = ref(false)
const form = ref({
  name: '',
  account_id: 0,
  remote_cid: '',
  remote_path: '',
  local_path: '',
  collection_type: 'movies',
})
const formErr = ref<Record<string, string>>({})

function openLib() {
  if (!accounts.value.length) {
    toast.warning('还没有网盘账号', '先在「账号」页签里登录 115 或夸克TV')
    return
  }
  form.value = {
    name: '',
    // 默认选最近添加的那个：刚扫码登录完夸克就点「新建媒体库」，
    // 不用在下拉里再翻一次（以前写死第一个，永远是那个 115 账号）。
    account_id: accounts.value[accounts.value.length - 1].id,
    remote_cid: '',
    remote_path: '',
    local_path: '',
    collection_type: 'movies',
  }
  formErr.value = {}
  libModal.value = true
}

async function submitLib() {
  const e: Record<string, string> = {}
  if (!form.value.name.trim()) e.name = '库名不能为空'
  if (!form.value.remote_cid) e.remote = '先选一个网盘目录'
  formErr.value = e
  if (Object.keys(e).length) return

  creating.value = true
  try {
    await api.libraries.create({
      account_id: form.value.account_id,
      name: form.value.name.trim(),
      remote_cid: form.value.remote_cid,
      remote_path: form.value.remote_path,
      local_path: form.value.local_path.trim(),
      collection_type: form.value.collection_type,
    })
    libModal.value = false
    toast.success('媒体库已创建', '去 Strm 管理跑一次全量同步')
    await reload()
  } catch (err: any) {
    toast.warning('创建失败', err?.message)
  } finally {
    creating.value = false
  }
}

/* ---------------- 网盘目录选择 ---------------- */
// 同一个目录弹窗现在有两个用途：新建库时选主目录、给已有库挂第二个网盘目录。
// pickerTarget 决定「选完写回哪个表单」，免得再复制一套目录浏览逻辑。
const pickerTarget = ref<'lib' | 'src'>('lib')
const dirPicker = ref(false)
const dirPath = ref<{ cid: string; name: string }[]>([])
const dirEntries = ref<DirEntry[]>([])
const dirLoading = ref(false)
const dirErr = ref('')

async function loadDir(cid: string) {
  dirLoading.value = true
  dirErr.value = ''
  try {
    const accountID =
      pickerTarget.value === 'src' ? srcForm.value.account_id : form.value.account_id
    const r = await api.storage.browse(accountID, cid)
    dirEntries.value = (r?.entries ?? []).filter((x: DirEntry) => x.is_dir)
  } catch (e: any) {
    dirEntries.value = []
    dirErr.value = e?.message || '读取目录失败'
  } finally {
    dirLoading.value = false
  }
}

async function openDirPicker(target: 'lib' | 'src' = 'lib') {
  pickerTarget.value = target
  dirPicker.value = true
  dirPath.value = []
  await loadDir('0')
}

async function enterDir(d: DirEntry) {
  dirPath.value = [...dirPath.value, { cid: d.id, name: d.name }]
  await loadDir(d.id)
}

async function jumpTo(i: number) {
  dirPath.value = dirPath.value.slice(0, i + 1)
  await loadDir(dirPath.value[i].cid)
}

async function jumpRoot() {
  dirPath.value = []
  await loadDir('0')
}

/** 选中的账号是哪种网盘：目录弹窗要写对名字，夸克列的是夸克目录。 */
const formAccountKind = computed(
  () => accounts.value.find((a) => a.id === form.value.account_id)?.kind || '115',
)
const formPanLabel = computed(() => (formAccountKind.value === 'quarktv' ? '夸克' : '115'))

// 换了账号，之前选的目录 id 属于另一个网盘，留着会建出一个空库。
watch(
  () => form.value.account_id,
  () => {
    form.value.remote_cid = ''
    form.value.remote_path = ''
    dirPath.value = []
    dirEntries.value = []
  },
)

function pickCurrent() {
  const cur = dirPath.value[dirPath.value.length - 1]
  const cid = cur?.cid || '0'
  const path = cur ? '/' + dirPath.value.map((p) => p.name).join('/') : '/'
  if (pickerTarget.value === 'src') {
    srcForm.value.remote_cid = cid
    srcForm.value.remote_path = path
    dirPicker.value = false
    return
  }
  // 根目录也可以直接选：夸克的根目录 fid 就是 "0"，不少人就想整盘扫。
  form.value.remote_cid = cid
  form.value.remote_path = path
  if (!form.value.name.trim()) form.value.name = cur?.name || formPanLabel.value + ' 媒体库'
  dirPicker.value = false
}

/* ---------------- 给已有库再挂一个网盘目录 ----------------
 * 「电影一半在 115、一半在夸克」不需要建两个库：挂到同一个库上，两边各写
 * 各的 .strm 到同一个本地目录，Emby 里只有一个分类。 */
const srcModal = ref(false)
const srcLib = ref<Lib | null>(null)
const srcSaving = ref(false)
const srcForm = ref({ account_id: 0, remote_cid: '', remote_path: '' })

const srcAccountKind = computed(
  () => accounts.value.find((a) => a.id === srcForm.value.account_id)?.kind || '115',
)

function openSrc(lib: Lib) {
  srcLib.value = lib
  const used = new Set((lib.sources ?? []).map((s) => s.account_id))
  // 默认挑一个还没挂过的账号：通常是刚登录的夸克。
  const candidate = accounts.value.find((a) => !used.has(a.id)) || accounts.value[0]
  srcForm.value = { account_id: candidate?.id ?? 0, remote_cid: '', remote_path: '' }
  srcModal.value = true
}

watch(
  () => srcForm.value.account_id,
  () => {
    srcForm.value.remote_cid = ''
    srcForm.value.remote_path = ''
  },
)

async function submitSrc() {
  if (!srcLib.value || !srcForm.value.remote_cid) {
    toast.warning('先选一个网盘目录')
    return
  }
  srcSaving.value = true
  try {
    await api.libraries.sources.add(srcLib.value.id, {
      account_id: srcForm.value.account_id,
      remote_cid: srcForm.value.remote_cid,
      remote_path: srcForm.value.remote_path,
    })
    srcModal.value = false
    toast.success('已挂上这个目录', '跑一次同步，两边的片就会出现在同一个库里')
    await reload()
  } catch (e: any) {
    toast.warning('挂载失败', e?.message)
  } finally {
    srcSaving.value = false
  }
}

async function removeSource(lib: Lib, src: LibSource) {
  if ((lib.sources?.length ?? 0) <= 1) {
    toast.warning('至少要留一个网盘目录', '库没有来源就没法同步；要换目录请先加新的')
    return
  }
  try {
    await api.libraries.sources.remove(lib.id, src.id)
    toast.success('已摘掉这个目录', '已生成的 .strm 还在，下一轮同步才会清理')
    await reload()
  } catch (e: any) {
    toast.warning('摘除失败', e?.message)
  }
}

const srcLabel = (s: LibSource) => (s.account_kind === 'quarktv' ? '夸克' : '115')
// 保活状态：进页面读一次，之后每分钟跟一次（后端 100 分钟才续一次，这里只是显示）
void loadKeep()
keepTimer = window.setInterval(() => void loadKeep(), 60_000)

</script>

<template>
  <div class="col" style="gap: 16px">
    <HTabs v-model="tab" :items="tabs" />

    <!-- ============ 网盘账号（115 / 夸克TV） ============ -->
    <template v-if="tab === 'account'">
      <div v-if="loadErr" class="panel panel-pad err-bar">
        <AppIcon name="alert" :size="16" />
        <span class="small">{{ loadErr }}</span>
        <button class="btn btn-sm btn-tertiary" style="margin-left: auto" @click="reload">重试</button>
      </div>

      <div v-if="loading && !accounts.length" class="panel panel-pad muted small">正在读取账号…</div>

      <div v-else-if="accounts.length" class="acc-grid">
        <div v-for="a in accounts" :key="a.id" class="panel panel-pad acc">
          <div class="row-between">
            <div class="row">
              <span class="avatar avatar-lg">{{ a.name.slice(0, 1) }}</span>
              <div class="col" style="gap: 2px">
                <strong>{{ a.name }}</strong>
                <span class="muted small mono">#{{ a.id }} · {{ (a.kind || '115').toUpperCase() }}</span>
              </div>
            </div>
            <span class="badge badge-success">已登录</span>
          </div>

          <div class="col" style="gap: 6px">
            <div class="row-between small">
              <span class="muted">添加时间</span>
              <span class="mono">{{ fmtDate(a.created_at) }}</span>
            </div>
            <div class="row-between small">
              <span class="muted">关联媒体库</span>
              <span class="mono">{{ libCount(a.id) }} 个</span>
            </div>
            <div class="row-between small">
              <span class="muted">今日请求</span>
              <span
                class="mono"
                :title="`本次运行以来共 ${calls[a.id]?.total ?? 0} 次（115 风控额度按天算，看的是今天）`"
              >
                {{ calls[a.id] ? `${calls[a.id].today} 次` : '—' }}
              </span>
            </div>
            <div class="row-between small probe-row">
              <span class="muted">凭据</span>
              <span
                class="mono truncate probe-msg"
                :title="probe[a.id]?.msg || '点「校验凭据」检查'"
                :style="{
                  color:
                    probe[a.id]?.state === 'ok'
                      ? 'var(--success)'
                      : probe[a.id]?.state === 'fail'
                        ? 'var(--danger)'
                        : 'var(--muted)',
                }"
              >
                {{ probeText(a.id) }}
              </span>
            </div>
            <!-- 夸克：播放靠 TV 接口，转存靠网页 Cookie，两件事要分开显示 -->
            <template v-if="a.kind === 'quarktv'">
              <div class="row-between small">
                <span class="muted">转存 Cookie</span>
                <span
                  class="mono"
                  :style="{ color: a.quark_cookie_set ? 'var(--success)' : 'var(--danger)' }"
                >
                  {{ a.quark_cookie_set ? '已配置' : '未配置 · 只能播放' }}
                </span>
              </div>
              <div v-if="a.quark_cookie_set" class="row-between small">
                <span class="muted">Cookie 保活</span>
                <span
                  class="mono"
                  :style="{ color: keep[a.id]?.last_error ? 'var(--danger)' : 'var(--success)' }"
                >{{ keepText(a.id) }}</span>
              </div>
              <div v-if="qkTestMsg[a.id]" class="small muted" style="line-height: 1.5">
                {{ qkTestMsg[a.id] }}
              </div>
            </template>
          </div>

          <div class="row" style="gap: 8px">
            <button
              class="btn btn-sm btn-tertiary"
              :disabled="probe[a.id]?.state === 'busy'"
              @click="check(a)"
            >
              <AppIcon name="shield" :size="14" />校验凭据
            </button>
            <template v-if="a.kind === 'quarktv'">
              <button class="btn btn-sm btn-tertiary" @click="openQkCookie(a)">
                <AppIcon name="key" :size="14" />配 Cookie
              </button>
              <button class="btn btn-sm btn-ghost" @click="testQuark(a)">
                <AppIcon name="shield" :size="14" />测试
              </button>
              <button
                v-if="a.quark_cookie_set"
                class="btn btn-sm btn-ghost"
                title="立刻向夸克续一次 __puus"
                @click="renewNow"
              >
                <AppIcon name="refresh" :size="14" />立即续期
              </button>
            </template>
            <button
              class="btn btn-sm btn-ghost"
              style="margin-left: auto; color: var(--danger)"
              @click="askDelete('account', a.id, a.name)"
            >
              <AppIcon name="trash" :size="14" />删除
            </button>
          </div>
        </div>
      </div>

      <!-- 添加账号 -->
      <HPanel
        :title="accounts.length ? '再添加一个账号' : '添加账号'"
        desc="115 扫码或 Cookie 登录；夸克TV 先扫码，再补一份网页 Cookie 用于转存"
      >
        <div class="segmented" style="margin-bottom: 14px">
          <button :class="{ 'is-active': addKind === '115' }" @click="addKind = '115'">115 网盘</button>
          <button :class="{ 'is-active': addKind === 'quarktv' }" @click="addKind = 'quarktv'">
            夸克TV
          </button>
        </div>

        <template v-if="addKind === '115'">
        <div class="row" style="gap: 8px; margin-bottom: 14px">
          <div class="segmented">
            <button :class="{ 'is-active': addMode === 'qr' }" @click="addMode = 'qr'">扫码登录</button>
            <button :class="{ 'is-active': addMode === 'cookie' }" @click="addMode = 'cookie'">
              Cookie 登录
            </button>
          </div>
        </div>

        <div v-if="addMode === 'qr'" class="qr-box">
          <div class="qr">
            <img v-if="qr" :src="qr.png" alt="115 登录二维码" class="qr-img" />
            <template v-else>
              <AppIcon name="grid" :size="44" />
              <span class="muted small">未生成</span>
            </template>
          </div>
          <div class="col" style="gap: 6px">
            <strong>{{ qrHint }}</strong>
            <p v-if="qrMsg" class="small" :class="qrStatus === 'error' ? 'text-danger' : 'muted'">
              {{ qrMsg }}
            </p>
            <p v-else class="muted small">
              二维码 2 分钟内有效；手机确认后自动写入凭据，不用再填任何东西。
            </p>
            <div class="row" style="gap: 8px; margin-top: 4px">
              <button class="btn btn-sm btn-primary" :disabled="qrBusy" @click="startQR">
                <AppIcon :name="qr ? 'refresh' : 'plus'" :size="14" />
                {{ qrBusy ? '生成中…' : qr ? '刷新二维码' : '生成二维码' }}
              </button>
              <span v-if="qrStatus === 'waiting' || qrStatus === 'scanned'" class="muted small dot-wait">
                轮询中…
              </span>
            </div>
          </div>
        </div>

        <div v-else class="col" style="gap: 10px">
          <HField label="115 Cookie" required tip="浏览器开发者工具里复制整段 Cookie，含 UID / CID / SEID / KID">
            <textarea v-model="cookie" class="textarea" placeholder="UID=...; CID=...; SEID=...; KID=..." />
          </HField>
          <HField label="账号备注" hint="留空就用 115 返回的昵称">
            <input v-model="cookieName" class="input" placeholder="如 主号（可留空）" />
          </HField>
          <div class="row" style="justify-content: flex-end">
            <button class="btn btn-primary" :disabled="saving" @click="addByCookie">
              {{ saving ? '登录中…' : '保存并登录' }}
            </button>
          </div>
        </div>
        </template>

        <!-- ===== 夸克TV：扫码登录 ===== -->
        <template v-else>
          <div class="qr-box">
            <div class="qr">
              <img v-if="qkQR" :src="qkQR.png" alt="夸克TV 登录二维码" class="qr-img" />
              <template v-else>
                <AppIcon name="cloud" :size="44" />
                <span class="muted small">未生成</span>
              </template>
            </div>
            <div class="col" style="gap: 6px">
              <strong>
                {{
                  qkStatus === 'waiting'
                    ? '打开夸克 App → 扫一扫 → 确认登录'
                    : qkStatus === 'expired'
                      ? '二维码已过期'
                      : qkStatus === 'error'
                        ? '出错了'
                        : '点下面的按钮生成二维码，2 分钟内有效'
                }}
              </strong>
              <p v-if="qkMsg" class="small" :class="qkStatus === 'error' ? 'text-danger' : 'muted'">
                {{ qkMsg }}
              </p>
              <p v-else class="muted small">
                这是夸克TV（电视端）的登录：换回的 refresh_token 长期有效，只用来列目录和取
                302 直链 —— 播放器直连夸克 CDN，服务端不中转。分享转存要另配网页 Cookie。
              </p>
              <div class="row" style="gap: 8px; margin-top: 4px">
                <button class="btn btn-sm btn-primary" :disabled="qkBusy" @click="startQkQR">
                  <AppIcon :name="qkQR ? 'refresh' : 'plus'" :size="14" />
                  {{ qkBusy ? '生成中…' : qkQR ? '刷新二维码' : '生成二维码' }}
                </button>
                <span v-if="qkStatus === 'waiting'" class="muted small dot-wait">轮询中…</span>
              </div>
            </div>
          </div>
          <p class="muted small" style="margin-top: 12px; line-height: 1.6">
            登录成功后，在下面的账号卡片上点「配 Cookie」贴上夸克网页 Cookie（F12 → Network →
            任意请求的 Cookie），才能把 TG 频道里的夸克分享一键存进来。
          </p>
        </template>
      </HPanel>
    </template>

    <!-- ============ 媒体库 ============ -->
    <template v-else>
      <HPanel title="媒体库" desc="一个库可以同时挂 115 和夸克的目录，都写进同一个本地目录；Emby 只认一个库" flush>
        <template #actions>
          <button class="btn btn-sm btn-primary" @click="openLib">
            <AppIcon name="plus" :size="14" />新建库
          </button>
        </template>

        <div v-if="libs.length" class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>库名</th>
                <th>网盘来源（可多个）</th>
                <th>本地目录</th>
                <th class="num">片目</th>
                <th>最近同步</th>
                <th style="width: 90px">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="l in libs" :key="l.id">
                <td><strong>{{ l.name }}</strong></td>
                <td>
                  <div class="col" style="gap: 4px">
                    <span
                      v-for="src in l.sources || []"
                      :key="src.id"
                      class="src-chip"
                      :title="`${src.account_name || accountName(src.account_id)} · ${src.remote_path || src.remote_cid}`"
                    >
                      <b>{{ srcLabel(src) }}</b>
                      <span class="mono truncate">{{ src.remote_path || src.remote_cid }}</span>
                      <button class="chip-x" title="摘掉这个目录" @click="removeSource(l, src)">
                        ×
                      </button>
                    </span>
                    <button class="btn btn-sm btn-ghost" style="align-self: flex-start" @click="openSrc(l)">
                      <AppIcon name="plus" :size="12" />挂网盘目录
                    </button>
                  </div>
                </td>
                <td class="mono small muted">{{ l.local_path }}</td>
                <td class="num">{{ (l.last_sync_files ?? 0).toLocaleString() }}</td>
                <td class="small muted">{{ fmtDate(l.last_sync_at) }}</td>
                <td>
                  <button
                    class="btn btn-sm btn-ghost"
                    title="删除"
                    style="color: var(--danger)"
                    @click="askDelete('library', l.id, l.name)"
                  >
                    <AppIcon name="trash" :size="14" />
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <HEmpty
          v-else
          title="还没有媒体库"
          :desc="
            accounts.length
              ? '选一个 115 或夸克TV 目录建库，再把 STRM 生成到本地'
              : '先在「账号」页签里登录 115 或夸克TV'
          "
        />
      </HPanel>

      <!-- 两个目录以前分散在「资源搜索」和「新建库」弹窗里，媒体库页看不到；
           配完转存还得跑回搜索页确认，所以集中到这里。 -->
      <HPanel title="目录设置" desc="转存目录 = 资源落进网盘的位置；媒体库目录 = 本地 .strm 的位置">
        <div class="grid-2">
          <div class="col dir-cfg-col">
            <span class="small strong">转存目录（网盘）</span>
            <div class="row dir-cfg-row">
              <span class="badge">115</span>
              <code class="mono mute-box grow truncate" :title="dirCfg.res_save_path">
                {{ resSaveLabel }}
              </code>
              <button class="btn btn-sm btn-tertiary" @click="openSaveDir('115')">
                <AppIcon name="folder" :size="13" />选择
              </button>
            </div>
            <div class="row dir-cfg-row">
              <span class="badge">夸克TV</span>
              <code class="mono mute-box grow truncate" :title="dirCfg.quark_save_fid">
                {{ quarkSaveLabel }}
              </code>
              <button class="btn btn-sm btn-tertiary" @click="openSaveDir('quarktv')">
                <AppIcon name="folder" :size="13" />选择
              </button>
            </div>
            <span class="muted small">
              「存到 115 / 存到夸克」、TG 分享转存、自动整理的源目录都用这两份设置。
            </span>
          </div>
          <div class="col dir-cfg-col">
            <span class="small strong">媒体库目录（本地）</span>
            <code class="mono mute-box">{{ dirCfg.media_root }}</code>
            <span class="muted small">
              STRM 写在 <span class="mono">{{ dirCfg.media_root }}/{{ '库名' }}</span> 下，
              Emby 添加这个根即可；媒体根由容器的 MEDIA_ROOT 决定。
            </span>
            <div v-if="libDirs.length" class="col" style="gap: 4px; margin-top: 2px">
              <div v-for="d in libDirs" :key="d.name" class="row-between small dir-cfg-row">
                <span class="muted" style="flex: none">{{ d.name }}</span>
                <span class="mono truncate" :title="d.path">{{ d.path }}</span>
              </div>
            </div>
            <span v-else class="muted small">还没有媒体库，新建后这里会列出各自的本地目录。</span>
            <button class="btn btn-sm btn-tertiary" style="align-self: flex-start" @click="reload">
              <AppIcon name="refresh" :size="14" />刷新目录信息
            </button>
          </div>
        </div>
      </HPanel>
    </template>

    <!-- 新建库 -->
    <HModal v-model="libModal" title="新建媒体库" size="md">
      <template #desc>库名会作为本地子目录名，建完别轻易改，改了要重扫</template>
      <HField label="库名" required :error="formErr.name">
        <input v-model="form.name" class="input" placeholder="如 电影" />
      </HField>
      <HField label="网盘账号" tip="115 和夸克TV 都能建库；夸克列目录走 TV 接口，播放走 302 直链">
        <select v-model.number="form.account_id" class="select-native">
          <option v-for="a in accounts" :key="a.id" :value="a.id">
            {{ a.kind === 'quarktv' ? '夸克TV' : '115' }} · {{ a.name }} · #{{ a.id }}
          </option>
        </select>
      </HField>
      <HField
        :label="`${formPanLabel} 网盘目录`"
        required
        :error="formErr.remote"
        tip="同步这个目录生成 STRM；选根目录就整盘扫"
      >
        <div class="row" style="gap: 8px">
          <input
            :value="form.remote_path"
            class="input mono"
            placeholder="点右边按钮选一个目录"
            readonly
          />
          <button class="btn btn-tertiary" @click="openDirPicker('lib')">
            <AppIcon name="folder" :size="14" />选择
          </button>
        </div>
      </HField>
      <HField label="本地目录" hint="在媒体根下的位置；留空按库名自动生成">
        <input v-model="form.local_path" class="input mono" placeholder="如 movies/电影（可留空）" />
      </HField>
      <HField label="内容类型">
        <div class="segmented">
          <button
            :class="{ 'is-active': form.collection_type === 'movies' }"
            @click="form.collection_type = 'movies'"
          >
            电影
          </button>
          <button
            :class="{ 'is-active': form.collection_type === 'tvshows' }"
            @click="form.collection_type = 'tvshows'"
          >
            剧集
          </button>
          <button
            :class="{ 'is-active': form.collection_type === 'mixed' }"
            @click="form.collection_type = 'mixed'"
          >
            混合
          </button>
        </div>
      </HField>
      <template #footer>
        <button class="btn btn-tertiary" @click="libModal = false">取消</button>
        <button class="btn btn-primary" :disabled="creating" @click="submitLib">
          {{ creating ? '创建中…' : '创建媒体库' }}
        </button>
      </template>
    </HModal>

    <!-- 给已有库再挂一个网盘目录 -->
    <HModal v-model="srcModal" title="挂一个网盘目录" size="md">
      <template #desc>
        两个网盘的片会写进同一个本地目录，Emby 里只有一个分类；差别只在 .strm 里的播放地址
      </template>
      <HField label="网盘账号" tip="115 和夸克都能挂；夸克列目录走 TV 接口，播放走 302 直链">
        <select v-model.number="srcForm.account_id" class="select-native">
          <option v-for="a in accounts" :key="a.id" :value="a.id">
            {{ a.kind === 'quarktv' ? '夸克TV' : '115' }} · {{ a.name }} · #{{ a.id }}
          </option>
        </select>
      </HField>
      <HField :label="`${srcAccountKind === 'quarktv' ? '夸克' : '115'} 网盘目录`" required
        tip="这个目录里的视频会和库里已有的片一起出现在 Emby 里">
        <div class="row" style="gap: 8px">
          <input :value="srcForm.remote_path" class="input mono" placeholder="点右边按钮选一个目录" readonly />
          <button class="btn btn-tertiary" @click="openDirPicker('src')">
            <AppIcon name="folder" :size="14" />选择
          </button>
        </div>
      </HField>
      <template #footer>
        <button class="btn btn-tertiary" @click="srcModal = false">取消</button>
        <button class="btn btn-primary" :disabled="srcSaving" @click="submitSrc">
          {{ srcSaving ? '挂载中…' : '挂上这个目录' }}
        </button>
      </template>
    </HModal>

    <!-- 网盘目录选择（115 / 夸克共用 /api/accounts/{id}/browse） -->
    <HModal
      v-model="dirPicker"
      :title="`选择 ${pickerTarget === 'src' ? (srcAccountKind === 'quarktv' ? '夸克' : '115') : formPanLabel} 目录`"
      size="md"
    >
      <template #desc>
        {{ pickerTarget === 'src' ? srcForm.remote_path || '根目录' : form.remote_path || '根目录' }}
      </template>
      <div class="row wrap small dir-crumbs">
        <button class="crumb" @click="jumpRoot">根目录</button>
        <template v-for="(p, i) in dirPath" :key="p.cid">
          <span class="muted">/</span>
          <button class="crumb" @click="jumpTo(i)">{{ p.name }}</button>
        </template>
      </div>
      <div class="dir-list">
        <div v-if="dirLoading" class="muted small" style="padding: 10px">读取中…</div>
        <div v-else-if="dirErr" class="small text-danger" style="padding: 10px">{{ dirErr }}</div>
        <template v-else>
          <button v-for="d in dirEntries" :key="d.id" class="dir-row" @click="enterDir(d)">
            <AppIcon name="folder" :size="15" />
            <span class="truncate">{{ d.name }}</span>
            <AppIcon name="play" :size="12" class="muted" />
          </button>
          <p v-if="!dirEntries.length" class="muted small" style="padding: 10px">
            这个目录下没有子目录
          </p>
        </template>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="dirPicker = false">取消</button>
        <button class="btn btn-primary" @click="pickCurrent">
          用当前目录
        </button>
      </template>
    </HModal>

    <!-- 删除确认 -->
    <HModal
      :model-value="!!delTarget"
      title="确认删除"
      size="md"
      @update:model-value="delTarget = null"
    >
      <p class="small">
        确定删除 <strong>{{ delTarget?.name }}</strong> 吗？
        <template v-if="delTarget?.kind === 'account'">
          账号下的媒体库不会自动删掉，但会失去对应的网盘凭据。
        </template>
        <template v-else>库记录会删掉，已生成的本地文件不会自动清理。</template>
      </p>
      <template #footer>
        <button class="btn btn-tertiary" @click="delTarget = null">取消</button>
        <button class="btn btn-danger" :disabled="deleting" @click="doDelete">
          {{ deleting ? '删除中…' : '删除' }}
        </button>
      </template>
    </HModal>

    <!-- 夸克网页 Cookie：只给「转存分享」用，和扫码登录的 refresh_token 是两回事 -->
    <HModal
      :model-value="!!qkCookieFor"
      title="夸克转存设置"
      size="md"
      @update:model-value="qkCookieFor = null"
    >
      <div class="col" style="gap: 10px">
        <p class="muted small" style="margin: 0; line-height: 1.65">
          夸克TV 的接口只能列目录和取直链，<strong>不能转存别人的分享</strong>，
          所以这一步单独要一份网页 Cookie。<strong>只需贴这一次</strong>：系统会每约 100 分钟
          自动续期会话字段并写回账号，只要服务不长期停机就不用重贴；万一失效，转存会明确报错，
          回来重贴即可。
        </p>
        <div class="qr-box" style="margin-top: 4px">
          <div class="qr">
            <img v-if="webQR" :src="webQR.png" alt="夸克网页登录二维码" class="qr-img" />
            <template v-else>
              <AppIcon name="key" :size="44" />
              <span class="muted small">未生成</span>
            </template>
          </div>
          <div class="col" style="gap: 6px">
            <strong>
              {{ webQRBusy && webQR ? '正在换一张新二维码…' :
                 webQRStatus === 'waiting' ? '用手机夸克 App 扫一扫 → 确认登录' :
                 webQRStatus === 'expired' ? '二维码已过期' :
                 webQRStatus === 'error' ? '出错了' : '推荐：扫码获取，不用开 F12' }}
            </strong>
            <p v-if="webQRMsg" class="small" :class="webQRStatus === 'error' ? 'text-danger' : 'muted'" style="margin: 0">
              {{ webQRMsg }}
            </p>
            <p v-else class="muted small" style="margin: 0; line-height: 1.6">
              扫完直接把网页 Cookie 写进这个账号，之后系统每约 100 分钟自动续期。
              这一步只影响「转存分享」，播放用的 TV 登录已经扫过了。
              二维码约 2 分钟有效，过期会自动换一张，不用手动刷新。
            </p>
            <div class="row" style="gap: 8px; margin-top: 4px">
              <button class="btn btn-sm btn-primary" :disabled="webQRBusy" @click="startWebQR()">
                <AppIcon :name="webQR ? 'refresh' : 'plus'" :size="14" />
                {{ webQRBusy ? '生成中…' : webQR ? '刷新二维码' : '生成二维码' }}
              </button>
              <span v-if="webQRStatus === 'waiting'" class="muted small dot-wait">
                轮询中…<template v-if="webQRLeft > 0"> · {{ webQRLeft }}s 后自动换码</template>
              </span>
            </div>
          </div>
        </div>
        <p class="muted small" style="margin: 0; line-height: 1.65">
          也可以手动粘贴：Chrome 打开 pan.quark.cn 并登录 → F12 → Network → 随便点一个请求 →
          复制请求头里整段 <span class="mono">Cookie</span>（含 __pus / __puus / __uid）。
        </p>
        <HField label="夸克 Cookie" required>
          <textarea
            v-model="qkCookie"
            class="textarea"
            placeholder="__pus=...; __puus=...; __uid=...; __kp=..."
          />
        </HField>
        <HField
          label="保存到夸克目录"
          hint="直接从夸克网盘里挑目录，不用再去地址栏复制 fid"
          tip="转存只改设置项，下次「存到夸克」直接用它"
        >
          <div class="row qk-dir-row" style="gap: 8px">
            <button class="btn btn-sm btn-tertiary" type="button" @click="qkDirPicker = true">
              <AppIcon name="folder" :size="14" />选择目录
            </button>
            <span class="small qk-dir-label truncate" :title="qkSaveFid || '根目录'">
              {{ qkDirLabel }}
            </span>
            <button
              v-if="qkSaveFid"
              class="btn btn-sm btn-ghost"
              type="button"
              @click="clearQkDir"
            >
              用根目录
            </button>
          </div>
        </HField>
        <p v-if="qkCookieFor?.quark_cookie_set" class="muted small" style="margin: 0">
          这个账号当前已有一份 Cookie，保存会直接覆盖（旧的通常已经过期了）。
        </p>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="qkCookieFor = null">取消</button>
        <button class="btn btn-primary" :disabled="qkCookieSaving" @click="saveQkCookie">
          {{ qkCookieSaving ? '校验并保存…' : '保存' }}
        </button>
      </template>
    </HModal>

    <DirPicker
      v-model="qkDirPicker"
      :account-id="qkCookieFor?.id"
      account-kind="quarktv"
      title="选择夸克转存目录"
      desc="分享链接转存进来的文件会落在这个目录里，选完自动记住"
      @pick="onPickQkDir"
    />

    <!-- 媒体库页「目录设置」里的转存目录，和上面 Cookie 弹窗是同一批设置项 -->
    <DirPicker
      v-model="saveDirPicker"
      :account-id="saveDirAccountId"
      :account-kind="saveDirKind"
      title="选择转存目录"
      :desc="`资源与分享转存进来的文件会落在这里（${saveDirKind === '115' ? '115' : '夸克TV'}）`"
      @pick="onPickSaveDir"
    />
  </div>
</template>

<style scoped>
.acc-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 16px;
}
@media (max-width: 1180px) {
  .acc-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 720px) {
  .acc-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
.acc {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.dir-cfg-col {
  gap: 8px;
  min-width: 0;
}
.dir-cfg-row {
  gap: 8px;
  min-width: 0;
}
.dir-cfg-row > .badge,
.dir-cfg-row > .btn {
  flex: none;
}
.dir-cfg-row > .mute-box {
  min-width: 0;
  padding: 6px 9px;
}
.qk-dir-row {
  min-width: 0;
}
.qk-dir-row > .qk-dir-label {
  flex: 1;
  min-width: 0;
  color: var(--muted);
}
/* 凭据状态可能是很长的后端报错：锁住左右两列的宽度，别把卡片撑歪 */
.probe-row > .muted {
  flex: none;
}
.probe-row > .probe-msg {
  min-width: 0;
  max-width: 62%;
  text-align: right;
}
.err-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  color: var(--danger);
}
.qr-box {
  display: grid;
  grid-template-columns: 168px minmax(0, 1fr);
  gap: 18px;
  align-items: center;
}
.qr {
  display: grid;
  place-items: center;
  gap: 4px;
  aspect-ratio: 1;
  padding: 6px;
  border-radius: var(--r-md);
  border: 1px dashed var(--border-secondary);
  background: var(--surface-secondary);
  color: var(--muted);
  overflow: hidden;
}
.qr-img {
  width: 100%;
  height: 100%;
  object-fit: contain;
  border-radius: var(--r-sm);
  background: #fff;
}
.mute-box {
  padding: 7px 10px;
  border-radius: var(--r-sm);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
  color: var(--foreground);
}
.text-danger {
  color: var(--danger);
}
textarea.textarea {
  height: auto;
  min-height: 96px;
  padding: 8px 11px;
  line-height: 1.5;
  resize: vertical;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 12px;
}
.dot-wait::before {
  content: '';
  display: inline-block;
  width: 6px;
  height: 6px;
  margin-right: 6px;
  border-radius: 50%;
  background: var(--accent);
  animation: pulse 1.1s ease-in-out infinite;
}
@keyframes pulse {
  50% {
    opacity: 0.25;
  }
}
.dir-crumbs {
  gap: 4px;
  padding: 8px 10px;
  margin-bottom: 10px;
  border-radius: var(--r-sm);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.crumb {
  border: 0;
  background: transparent;
  padding: 2px 4px;
  border-radius: 4px;
  font-size: 12px;
  color: var(--accent);
}
.crumb:hover {
  background: var(--accent-soft);
}
.dir-list {
  max-height: 46vh;
  overflow: auto;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
}
.dir-row {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  padding: 9px 12px;
  border: 0;
  border-bottom: 1px solid var(--separator);
  background: transparent;
  text-align: left;
  font-size: 13px;
}
.dir-row:last-child {
  border-bottom: 0;
}
.dir-row:hover {
  background: var(--surface-secondary);
}
.dir-row .truncate {
  flex: 1;
  min-width: 0;
}

/* 网盘来源小标签：一个库挂两家时，表格里一眼看出哪几条来自 115、哪几条来自夸克 */
.src-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 100%;
  padding: 3px 6px 3px 8px;
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 12px;
  background: var(--surface-secondary);
}
.src-chip b {
  font-weight: 500;
  color: var(--text-secondary);
}
.src-chip .truncate {
  min-width: 0;
  max-width: 220px;
}
.chip-x {
  border: 0;
  background: transparent;
  color: var(--text-tertiary, var(--text-secondary));
  cursor: pointer;
  font-size: 14px;
  line-height: 1;
  padding: 0 2px;
}
.chip-x:hover {
  color: var(--danger);
}
@media (max-width: 720px) {
  .qr-box {
    grid-template-columns: minmax(0, 1fr);
  }
  .qr {
    width: 168px;
  }
}
</style>
