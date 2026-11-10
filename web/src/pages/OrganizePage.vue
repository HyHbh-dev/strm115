<script setup lang="ts">
/**
 * 自动整理：把 115「待整理」目录里的片，按识别结果搬进对应的媒体库目录。
 *
 * 这一页之前七个页签全是写死的示例数据（开关不落库、规则是编的、
 * 「保存」只弹 toast）。现在只保留真能跑通的功能，其余一律不放：
 *  - 配置      GET|PUT /api/organize/config
 *  - 扫描预览  POST /api/organize/scan        只读，不动文件
 *  - 执行      POST /api/organize/run         真移动
 *  - 台账      GET  /api/organize/records     忽略 / 重试 / 清空
 *  - 试算      POST /api/organize/preview     走真实识别器
 *
 * 两个刻意的取舍：
 * 1. 没有「AI 识别」「洗版」「重命名模板」这些页签。它们需要的能力后端
 *    还没有，留一个点了没反应的开关比没有这个开关更浪费用户时间。
 * 2. 扫描和执行分成两步，且执行要在弹窗里二次确认。整理会改变网盘里的
 *    实际位置，第一次用不该是「点一下试试看」。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import TitleLink from '@/components/TitleLink.vue'
import AppIcon from '@/components/AppIcon.vue'
import HField from '@/components/HField.vue'
import HModal from '@/components/HModal.vue'
import HPanel from '@/components/HPanel.vue'
import HTabs from '@/components/HTabs.vue'
import HSwitch from '@/components/HSwitch.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'

/** 一条整理规则：一个网盘目录 + 它要搬进的库。 */
interface Rule {
  id: string
  name?: string
  enabled?: boolean
  account_id: number
  source_cid: string
  source_name: string
  movie_lib_id: number
  tv_lib_id: number
  /** 目标子目录；留空 = 库的根目录。混合库（根/影视，下分 电影/剧集）必须填 */
  target_cid?: string
  target_name?: string
}

/** 扫描接口按规则分组返回。 */
interface ScanRule {
  id: string
  name: string
  account_id: number
  account_name: string
  account_kind: string
  source_cid: string
  source_name: string
  items: Item[]
  stats: Record<string, number>
  error?: string
}

interface LibBrief {
  id: number
  name: string
  collection_type: string
  remote_cid: string
}
interface Item {
  file_id: string
  name: string
  is_dir: boolean
  size: number
  kind: string
  title: string
  year: string
  season: number
  episode: number
  confidence: number
  reason: string
  dir: string
  file: string
  target: string
  lib?: string
  skip?: string
}
interface OrgRecord {
  id: number
  file_id: string
  name: string
  kind: string
  title: string
  year: string
  target: string
  status: string
  message: string
  confidence: number
  updated_at: string
}

const tab = ref('basic')
const tabs = [
  { value: 'basic', label: '基础配置' },
  { value: 'scan', label: '扫描与执行' },
  { value: 'records', label: '整理记录' },
  { value: 'rules', label: '识别规则' },
]

/* ==================== 配置 ==================== */
const cfg = reactive({
  enabled: false,
  rules: [] as Rule[],
  account_id: 0,
  source_cid: '',
  source_name: '',
  movie_lib_id: 0,
  tv_lib_id: 0,
  batch: 30,
  min_size_mb: 300,
  sync_after: true,
  auto_ingest: true,
  verify_tmdb: true,
})
const accounts = ref<{ id: number; name: string; kind: string }[]>([])
const libs = ref<LibBrief[]>([])
const stats = ref<Record<string, number>>({})
const loading = ref(true)
const saving = ref(false)
const dirty = ref(false)

const movieLibs = computed(() => {
  const hit = libs.value.filter((l) => l.collection_type !== 'tvshows')
  return hit.length ? hit : libs.value
})
const tvLibs = computed(() => {
  const hit = libs.value.filter((l) => l.collection_type !== 'movies')
  return hit.length ? hit : libs.value
})
function ruleAccount(r: Rule) {
  return r.account_id || accounts.value[0]?.id || 0
}
function addRule() {
  cfg.rules.push({
    id: `r${Date.now()}`,
    account_id: accounts.value[0]?.id ?? 0,
    source_cid: '',
    source_name: '',
    movie_lib_id: 0,
    tv_lib_id: 0,
  })
  dirty.value = true
}
function removeRule(i: number) {
  cfg.rules.splice(i, 1)
  dirty.value = true
}
function onRuleAccount(r: Rule) {
  // 换账号后旧目录的 cid 属于上一个网盘，留着会算错目标也列不出来
  r.source_cid = ''
  r.source_name = ''
  dirty.value = true
}

async function load() {
  try {
    const r = await api.organize.config()
    Object.assign(cfg, r?.config ?? {})
    accounts.value = r?.accounts ?? []
    libs.value = r?.libraries ?? []
    stats.value = r?.stats ?? {}
    if (!cfg.account_id && accounts.value.length) cfg.account_id = accounts.value[0].id
    if (!Array.isArray(cfg.rules)) cfg.rules = []
    if (!cfg.rules.length && cfg.source_cid) {
      cfg.rules = [{
        id: 'r1', account_id: cfg.account_id, source_cid: cfg.source_cid,
        source_name: cfg.source_name, movie_lib_id: cfg.movie_lib_id, tv_lib_id: cfg.tv_lib_id,
      }]
    }
    dirty.value = false
  } catch (e: any) {
    toast.error('读取整理配置失败', e?.message)
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    await api.organize.saveConfig({ ...cfg })
    dirty.value = false
    toast.success('配置已保存', cfg.enabled ? '可以到「扫描与执行」看一轮了' : '自动整理仍未启用')
    await load()
  } catch (e: any) {
    toast.error('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

/* ==================== 目录选择 ==================== */
const picker = reactive({
  open: false,
  ruleIndex: -1,
  // source = 待整理目录，target = 搬进库里的哪个目录
  kind: 'source' as 'source' | 'target',
  cid: '0',
  stack: [] as { id: string; name: string }[],
  entries: [] as { id: string; name: string; is_dir: boolean }[],
  busy: false,
  error: '',
})

async function openPicker(index: number, kind: 'source' | 'target' = 'source') {
  picker.ruleIndex = index
  picker.kind = kind
  picker.open = true
  picker.stack = [{ id: '0', name: '根目录' }]
  await browseDir('0')
}

async function browseDir(cid: string) {
  picker.busy = true
  picker.error = ''
  try {
    const rule = cfg.rules[picker.ruleIndex]
    const r = await api.files.browse(rule ? ruleAccount(rule) : 0, cid, { dirsOnly: true })
    picker.cid = r?.cid ?? cid
    picker.entries = (r?.entries ?? []).filter((e: any) => e.is_dir)
  } catch (e: any) {
    picker.entries = []
    picker.error = e?.message || '列目录失败'
  } finally {
    picker.busy = false
  }
}

async function enterDir(e: { id: string; name: string }) {
  picker.stack.push({ id: e.id, name: e.name })
  await browseDir(e.id)
}

async function upDir() {
  if (picker.stack.length <= 1) return
  picker.stack.pop()
  const top = picker.stack[picker.stack.length - 1]
  await browseDir(top.id)
}

function chooseDir() {
  const top = picker.stack[picker.stack.length - 1]
  if (top.id === '0') {
    toast.warning('别选根目录', '请进到具体的「待整理」子目录')
    return
  }
  const rule = cfg.rules[picker.ruleIndex]
  if (!rule) {
    picker.open = false
    return
  }
  const full = picker.stack.map((s) => s.name).slice(1).join('/')
  if (picker.kind === 'target') {
    rule.target_cid = top.id
    rule.target_name = full
    dirty.value = true
    picker.open = false
    toast.success('已选目标目录', full)
    return
  }
  rule.source_cid = top.id
  rule.source_name = full
  dirty.value = true
  picker.open = false
  toast.success('已选源目录', full)
}

/* ==================== 扫描 ==================== */
const scan = reactive({
  busy: false,
  done: false,
  ruleId: '',
  rules: [] as ScanRule[],
  items: [] as Item[],
  picked: [] as string[],
  stats: {} as Record<string, number>,
  error: '',
})

const currentScanRule = computed(() => scan.rules.find((r) => r.id === scan.ruleId))

// applyScanRule 把「当前选中规则」的扫描结果摊到表格上。
function applyScanRule() {
  const cur = currentScanRule.value
  scan.items = cur?.items ?? []
  scan.stats = cur?.stats ?? {}
  scan.error = cur?.error ?? ''
  // 默认只勾「把握够」的：没有年份的条目也能认出来，但错一次就是
  // 文件被搬进一个奇怪的目录。低把握的留给用户自己决定。
  scan.picked = scan.items.filter((i) => !i.skip && i.confidence >= CONF_LINE).map((i) => i.file_id)
  scan.done = true
}

function switchScanRule(id: string) {
  scan.ruleId = id
  applyScanRule()
}

const CONF_LINE = 70
const actionable = computed(() => scan.items.filter((i) => !i.skip))
const lowConf = computed(() => actionable.value.filter((i) => i.confidence < CONF_LINE).length)
const allPicked = computed(
  () => actionable.value.length > 0 && scan.picked.length === actionable.value.length,
)

async function runScan() {
  scan.busy = true
  scan.error = ''
  try {
    const r = await api.organize.scan()
    scan.rules = r?.rules ?? []
    // 优先停在「有东西可整理」的那条规则上：多条规则时多半只有一条有货。
    if (!scan.rules.some((x) => x.id === scan.ruleId)) {
      scan.ruleId = (scan.rules.find((x) => (x.items?.length ?? 0) > 0) ?? scan.rules[0])?.id ?? ''
    }
    applyScanRule()
    const total = scan.rules.reduce((n, x) => n + (x.items?.length ?? 0), 0)
    if (!total) toast.info('这些目录里没有待整理的条目')
    else toast.success(`扫到 ${total} 条`, `${actionable.value.length} 条可以整理`)
  } catch (e: any) {
    scan.error = e?.message || '扫描失败'
    toast.error('扫描失败', scan.error)
  } finally {
    scan.busy = false
  }
}

function togglePick(id: string) {
  const i = scan.picked.indexOf(id)
  if (i >= 0) scan.picked.splice(i, 1)
  else scan.picked.push(id)
}
function toggleAll() {
  scan.picked = allPicked.value ? [] : actionable.value.map((i) => i.file_id)
}

const confirmOpen = ref(false)
const running = ref(false)
const runResult = ref<{ moved: number; failed: number; skipped: number; synced: string[]; errors: string[] } | null>(null)

async function doRun() {
  running.value = true
  try {
    const r = await api.organize.run(scan.picked, scan.ruleId || undefined)
    runResult.value = r
    confirmOpen.value = false
    if (r?.moved) {
      toast.success(`已整理 ${r.moved} 项`, r.synced?.length ? `正在同步：${r.synced.join('、')}` : '')
    } else if (r?.failed) {
      toast.warning('一条都没整理成功', r.errors?.[0] ?? '')
    } else {
      toast.info('没有可整理的条目')
    }
    await Promise.all([runScan(), loadRecords(), load()])
  } catch (e: any) {
    toast.error('整理失败', e?.message)
  } finally {
    running.value = false
  }
}

/* ==================== 台账 ==================== */
const rec = reactive({
  status: '',
  limit: 50,
  offset: 0,
  total: 0,
  items: [] as OrgRecord[],
  busy: false,
})
const statusTabs = [
  { value: '', label: '全部' },
  { value: 'moved', label: '已整理' },
  { value: 'failed', label: '失败' },
  { value: 'skipped', label: '跳过' },
  { value: 'ignored', label: '已忽略' },
]
const toneOf: Record<string, string> = {
  moved: 'badge-success',
  failed: 'badge-danger',
  skipped: 'badge-warning',
  ignored: '',
}
const textOf: Record<string, string> = {
  moved: '已整理',
  failed: '失败',
  skipped: '跳过',
  ignored: '已忽略',
}

async function loadRecords() {
  rec.busy = true
  try {
    const r = await api.organize.records({ status: rec.status, limit: rec.limit, offset: rec.offset })
    rec.items = r?.items ?? []
    rec.total = r?.total ?? 0
    stats.value = r?.stats ?? stats.value
  } catch (e: any) {
    toast.error('读取整理记录失败', e?.message)
  } finally {
    rec.busy = false
  }
}

async function switchStatus(v: string) {
  rec.status = v
  rec.offset = 0
  await loadRecords()
}

async function ignoreOne(id: number) {
  try {
    await api.organize.ignore(id)
    toast.success('已忽略', '以后扫描不再出现这一条')
    await loadRecords()
  } catch (e: any) {
    toast.error('忽略失败', e?.message)
  }
}

async function retryOne(id: number) {
  try {
    const r = await api.organize.retry(id)
    if (r?.ok) toast.success('已重新整理', r?.message)
    else toast.warning('还是没成功', r?.message)
    await Promise.all([loadRecords(), load()])
  } catch (e: any) {
    toast.error('重试失败', e?.message)
  }
}

async function clearRecords() {
  try {
    const r = await api.organize.clearRecords(rec.status)
    toast.success(`已清空 ${r?.removed ?? 0} 条记录`, '只删台账，网盘里的文件没动')
    rec.offset = 0
    await Promise.all([loadRecords(), load()])
  } catch (e: any) {
    toast.error('清空失败', e?.message)
  }
}

/* ==================== 试算 ==================== */
const probe = reactive({ name: '', isDir: false, busy: false, item: null as Item | null, error: '' })

async function doPreview() {
  if (!probe.name.trim()) return
  probe.busy = true
  probe.error = ''
  try {
    const r = await api.organize.preview(probe.name.trim(), probe.isDir)
    probe.item = r?.item ?? null
  } catch (e: any) {
    probe.item = null
    probe.error = e?.message || '试算失败'
  } finally {
    probe.busy = false
  }
}

/* ==================== 杂项 ==================== */
const tabOf: Record<string, string> = { movie: '电影', series: '剧集', unknown: '未知' }
const kindOf = (k: string) => tabOf[k] ?? k
function sizeText(n: number): string {
  if (!n) return '—'
  const g = n / 1024 ** 3
  return g >= 1 ? `${g.toFixed(1)} GB` : `${(n / 1024 ** 2).toFixed(0)} MB`
}
function agoText(s: string): string {
  if (!s) return '—'
  const t = new Date(s).getTime()
  if (!t) return '—'
  const d = Math.max(0, Date.now() - t) / 1000
  if (d < 60) return '刚刚'
  if (d < 3600) return `${Math.floor(d / 60)} 分钟前`
  if (d < 86400) return `${Math.floor(d / 3600)} 小时前`
  return `${Math.floor(d / 86400)} 天前`
}

onMounted(async () => {
  await load()
  await loadRecords()
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <HTabs v-model="tab" :items="tabs" />

    <!-- ==================== 基础配置 ==================== -->
    <template v-if="tab === 'basic'">
      <HPanel title="基础配置" desc="选好「从哪来」和「到哪去」，再到下一个页签先看一遍再执行">
        <p v-if="!cfg.enabled" class="pending small">
          <AppIcon name="info" :size="14" />
          自动整理还没启用：下面的配置可以随便改，但「执行整理」会被拒绝。
        </p>

        <HField label="启用">
          <label class="checkline">
            <HSwitch v-model="cfg.enabled" label="启用自动整理" />
            <span class="small">允许执行整理（扫描预览任何时候都能跑）</span>
          </label>
        </HField>

        <HField
          label="整理规则"
          required
          tip="每条规则 = 一个网盘目录 + 它要搬进的库。115 和夸克各配一条就互不干扰；源目录会被搬空，别选库目录本身"
        >
          <div class="rule-list">
            <div v-for="(r, i) in cfg.rules" :key="r.id" class="rule-card">
              <div class="row-between" style="margin-bottom: 8px">
                <strong class="small">规则 {{ i + 1 }}</strong>
                <button
                  class="btn btn-sm btn-ghost"
                  style="color: var(--danger)"
                  title="删除这条规则"
                  @click="removeRule(i)"
                >
                  <AppIcon name="trash" :size="13" />删除
                </button>
              </div>
              <div class="rule-grid">
                <label class="rule-field">
                  <span class="muted small">网盘账号</span>
                  <select v-model.number="r.account_id" class="select-native" @change="onRuleAccount(r)">
                    <option v-for="a in accounts" :key="a.id" :value="a.id">
                      {{ a.kind === 'quarktv' ? '夸克' : '115' }} · {{ a.name || `账号 ${a.id}` }}
                    </option>
                  </select>
                </label>
                <label class="rule-field">
                  <span class="muted small">待整理目录</span>
                  <div class="row" style="gap: 6px">
                    <input
                      class="input mono"
                      :value="r.source_name || (r.source_cid ? `cid ${r.source_cid}` : '')"
                      placeholder="还没选"
                      readonly
                    />
                    <button class="btn btn-sm btn-tertiary" @click="openPicker(i)">
                      <AppIcon name="folder" :size="13" />选择
                    </button>
                  </div>
                </label>
                <label class="rule-field">
                  <span class="muted small">电影库</span>
                  <select v-model.number="r.movie_lib_id" class="select-native">
                    <option :value="0">不整理电影</option>
                    <option v-for="l in movieLibs" :key="l.id" :value="l.id">{{ l.name }}</option>
                  </select>
                </label>
                <label class="rule-field">
                  <span class="muted small">剧集库</span>
                  <select v-model.number="r.tv_lib_id" class="select-native">
                    <option :value="0">不整理剧集</option>
                    <option v-for="l in tvLibs" :key="l.id" :value="l.id">{{ l.name }}</option>
                  </select>
                </label>
                <label class="rule-field" style="grid-column: 1 / -1">
                  <span class="muted small">
                    目标目录
                    <span class="muted">（留空 = 库根目录；库根下面是「电影 / 剧集」两层时必填）</span>
                  </span>
                  <div class="row" style="gap: 6px">
                    <input
                      class="input mono"
                      :value="r.target_name || (r.target_cid ? `cid ${r.target_cid}` : '')"
                      placeholder="库根目录"
                      readonly
                    />
                    <button class="btn btn-sm btn-tertiary" @click="openPicker(i, 'target')">
                      <AppIcon name="folder" :size="13" />选择
                    </button>
                    <button
                      v-if="r.target_cid"
                      class="btn btn-sm btn-ghost"
                      title="清空，改回库根目录"
                      @click="r.target_cid = ''; r.target_name = ''; dirty = true"
                    >
                      清空
                    </button>
                  </div>
                </label>
              </div>
            </div>
            <button class="btn btn-sm btn-tertiary" style="align-self: flex-start" @click="addRule">
              <AppIcon name="plus" :size="13" />添加规则
            </button>
          </div>
        </HField>

        <HField label="单轮上限" tip="一次最多移动多少条。115 会按请求密度风控，别一次几百个" hint="建议 20～50">
          <input v-model.number="cfg.batch" class="input" type="number" min="1" max="200" style="max-width: 120px" />
        </HField>

        <HField label="最小体积" hint="单位 MB。小于这个的当作样片 / 花絮跳过">
          <input v-model.number="cfg.min_size_mb" class="input" type="number" min="0" style="max-width: 120px" />
        </HField>

        <HField label="整理后同步">
          <label class="checkline">
            <HSwitch v-model="cfg.sync_after" label="整理后同步" />
            <span class="small">移动完立刻给受影响的媒体库跑一轮增量，Emby 马上能看到</span>
          </label>
        </HField>

        <HField
          label="入库后整理"
          tip="TG 转存、机器人离线、订阅下载都会落在「资源保存目录」。打开后东西一到就自动整理进媒体库，再接着同步、刮削，不用再回这页点执行；手动在 115 里转存进去的也一样（每 5 分钟看一眼）"
        >
          <label class="checkline">
            <HSwitch v-model="cfg.auto_ingest" label="入库后自动整理" />
            <span class="small">盯的是「资源保存目录」，不是上面的待整理目录</span>
          </label>
        </HField>

        <HField
          label="刮得到才搬"
          tip="搬之前先问一次 TMDB：片名和年份对得上才移动。对不上的原样留在暂存目录（/StrmStation），不会变成媒体库里一张没海报的灰卡片。TMDB 网络错误不算「对不上」，照常搬"
        >
          <label class="checkline">
            <HSwitch v-model="cfg.verify_tmdb" label="TMDB 找得到才搬" />
            <span class="small">未刮削的留在暂存目录，人工处理完再整理</span>
          </label>
        </HField>

        <template #footer>
          <span v-if="dirty" class="small muted">有未保存的改动</span>
          <button class="btn btn-primary" :disabled="saving || loading" @click="save">
            {{ saving ? '保存中…' : '保存配置' }}
          </button>
        </template>
      </HPanel>
    </template>

    <!-- ==================== 扫描与执行 ==================== -->
    <template v-else-if="tab === 'scan'">
      <HPanel title="扫描预览" desc="只看不动：源目录里的每一条会被识别成什么、准备放进哪个目录">
        <template #actions>
          <button class="btn btn-sm btn-tertiary" :disabled="scan.busy || !cfg.rules.length" @click="runScan">
            <AppIcon name="search" :size="14" />
            {{ scan.busy ? '扫描中…' : '扫描源目录' }}
          </button>
        </template>

        <p v-if="!cfg.rules.length" class="pending small">
          <AppIcon name="info" :size="14" />
          还没有整理规则，去「基础配置」里加一条。
        </p>
        <p v-else-if="!scan.done" class="muted small">
          点右上角「扫描源目录」。这一步不会动任何文件，只是把结果列出来。
        </p>

        <div v-if="scan.rules.length > 1" class="chips" style="margin-bottom: 10px">
          <button
            v-for="r in scan.rules"
            :key="r.id"
            class="chip"
            :class="{ 'is-active': r.id === scan.ruleId }"
            @click="switchScanRule(r.id)"
          >
            {{ r.account_kind === 'quarktv' ? '夸克' : '115' }}:{{ r.source_name || r.source_cid }} ·
            {{ r.items?.length ?? 0 }} 条
          </button>
        </div>
        <p v-if="scan.error" class="field-error">{{ scan.error }}</p>

        <template v-if="scan.items.length">
          <div class="chips">
            <span class="chip">共 {{ scan.stats.total ?? scan.items.length }} 条</span>
            <span class="chip is-active">电影 {{ scan.stats.movie ?? 0 }}</span>
            <span class="chip is-active">剧集 {{ scan.stats.series ?? 0 }}</span>
            <span v-if="scan.stats.skip" class="chip">跳过 {{ scan.stats.skip }}</span>
            <span v-if="scan.stats.done" class="chip">已整理过 {{ scan.stats.done }}</span>
          </div>
        </template>

        <div v-if="scan.items.length" class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th style="width: 34px">
                  <input type="checkbox" :checked="allPicked" @change="toggleAll" />
                </th>
                <th style="width: 28%">源名称</th>
                <th style="width: 21%">识别为</th>
                <th style="width: 14%">依据</th>
                <th>目标</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="it in scan.items" :key="it.file_id" :class="{ 'is-off': !!it.skip }">
                <td>
                  <input
                    v-if="!it.skip"
                    type="checkbox"
                    :checked="scan.picked.includes(it.file_id)"
                    @change="togglePick(it.file_id)"
                  />
                  <span v-else class="muted">—</span>
                </td>
                <td class="small">
                  <span class="truncate" style="display: block" :title="it.name">{{ it.name }}</span>
                  <span class="muted mono" style="font-size: 11px">
                    {{ sizeText(it.size) }}<template v-if="it.is_dir"> · 目录</template>
                  </span>
                </td>
                <td class="small">
                  <div class="row" style="gap: 6px; align-items: center">
                    <span class="badge" :class="it.kind === 'series' ? 'badge-info' : 'badge-accent'">
                      {{ kindOf(it.kind) }}
                    </span>
                    <span class="mono small">{{ it.confidence }}%</span>
                    <span v-if="!it.skip && it.confidence < CONF_LINE" class="badge badge-warning">低把握</span>
                  </div>
                  <div class="truncate" style="margin-top: 4px" :title="it.title">
                    <TitleLink :type="it.kind" :title="it.title" :year="it.year">{{ it.title }}</TitleLink><template v-if="it.year"> ({{ it.year }})</template>
                  </div>
                </td>
                <td class="muted small">{{ it.reason }}</td>
                <td class="mono small">
                  <template v-if="it.skip">
                    <span class="warn">跳过：{{ it.skip }}</span>
                  </template>
                  <template v-else>
                    <div class="tgt">
                      <span class="tgtlib">{{ it.lib }}</span>
                      <span class="truncate" :title="`${it.lib} / ${it.dir}`">{{ it.dir }}</span>
                    </div>
                    <div v-if="it.file" class="truncate tgtfile" :title="it.file">{{ it.file }}</div>
                  </template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <template #footer>
          <template v-if="scan.items.length">
            <span class="small muted">
              已选 {{ scan.picked.length }} / 可整理 {{ actionable.length }}
              <template v-if="lowConf">（{{ lowConf }} 条低把握未勾选）</template>
            </span>
            <button class="btn btn-primary" :disabled="!scan.picked.length || running || !cfg.enabled" @click="confirmOpen = true">
              <AppIcon name="organize" :size="14" />
              执行整理（{{ scan.picked.length }}）
            </button>
          </template>
        </template>
      </HPanel>

      <HPanel v-if="runResult" title="上一次执行结果">
        <div class="chips">
          <span class="chip is-active">已整理 {{ runResult.moved }}</span>
          <span v-if="runResult.failed" class="chip">失败 {{ runResult.failed }}</span>
          <span v-if="runResult.skipped" class="chip">跳过 {{ runResult.skipped }}</span>
          <span v-if="runResult.synced?.length" class="chip">已触发同步：{{ runResult.synced.join('、') }}</span>
        </div>
        <p v-if="runResult.errors?.length" class="small warn" style="white-space: pre-wrap">
          {{ runResult.errors.join('\n') }}
        </p>
      </HPanel>
    </template>

    <!-- ==================== 整理记录 ==================== -->
    <template v-else-if="tab === 'records'">
      <HPanel title="整理记录" desc="每一条都记着识别依据和目标位置；清空只删台账，不动网盘文件" flush>
        <template #actions>
          <button class="btn btn-sm btn-ghost" style="color: var(--danger)" @click="clearRecords">
            <AppIcon name="trash" :size="14" />清空{{ rec.status ? '此类' : '全部' }}
          </button>
        </template>

        <div class="filterbar">
          <button
            v-for="t in statusTabs"
            :key="t.value"
            class="chip"
            :class="{ 'is-active': rec.status === t.value }"
            @click="switchStatus(t.value)"
          >
            {{ t.label }}
            <template v-if="t.value === ''">({{ rec.total }})</template>
          </button>
        </div>

        <div v-if="!rec.items.length" class="empty" style="padding: 28px 0">
          <p class="empty-title">{{ rec.busy ? '读取中…' : '还没有记录' }}</p>
          <p class="empty-desc">跑一轮整理之后，这里会留下每一部的识别依据和落位结果</p>
        </div>

        <!-- 台账按「一行一条」排，不再用表：六列在窄屏上会互相挤，
             而且手机里横向滚动表头还会浮在别的内容上面。 -->
        <div v-else class="reclist">
          <div v-for="r in rec.items" :key="r.id" class="rec">
            <div class="rec-main">
              <div class="row wrap" style="gap: 6px; align-items: center">
                <span class="badge" :class="toneOf[r.status]">{{ textOf[r.status] ?? r.status }}</span>
                <strong class="small truncate">
                  <TitleLink :type="r.kind" :title="r.title" :year="r.year">{{ r.title || '未识别' }}</TitleLink><template v-if="r.year"> ({{ r.year }})</template>
                </strong>
                <span class="muted mono small">{{ r.confidence }}%</span>
              </div>
              <p class="rec-name mono small muted truncate" :title="r.name">{{ r.name }}</p>
              <p class="rec-tgt small">
                <span v-if="r.target" class="truncate">{{ r.target }}</span>
                <span v-else class="muted">{{ r.message || '—' }}</span>
              </p>
            </div>
            <div class="rec-side">
              <span class="muted small">{{ agoText(r.updated_at) }}</span>
              <div class="row" style="gap: 2px">
                <button
                  v-if="r.status !== 'moved' && r.status !== 'ignored'"
                  class="btn btn-sm btn-ghost"
                  title="重试"
                  @click="retryOne(r.id)"
                >
                  <AppIcon name="refresh" :size="14" />
                </button>
                <button
                  v-if="r.status !== 'ignored'"
                  class="btn btn-sm btn-ghost"
                  title="忽略"
                  @click="ignoreOne(r.id)"
                >
                  <AppIcon name="close" :size="14" />
                </button>
              </div>
            </div>
          </div>
        </div>

        <template #footer>
          <span class="small muted">共 {{ rec.total }} 条</span>
          <div class="row" style="gap: 6px">
            <button class="btn btn-sm btn-tertiary" :disabled="rec.offset <= 0" @click="rec.offset = Math.max(0, rec.offset - rec.limit); loadRecords()">
              上一页
            </button>
            <button
              class="btn btn-sm btn-tertiary"
              :disabled="rec.offset + rec.limit >= rec.total"
              @click="rec.offset += rec.limit; loadRecords()"
            >
              下一页
            </button>
          </div>
        </template>
      </HPanel>
    </template>

    <!-- ==================== 识别规则 ==================== -->
    <template v-else>
      <HPanel title="试算" desc="用真实识别器跑一遍：贴一个文件名或目录名，看它会被放进哪个目录">
        <HField label="名称">
          <div class="row" style="gap: 8px">
            <input
              v-model="probe.name"
              class="input mono"
              placeholder="如 The.Last.of.Us.S01E03.2160p.WEB-DL.mkv"
              @keyup.enter="doPreview"
            />
            <button class="btn btn-primary" :disabled="probe.busy || !probe.name.trim()" @click="doPreview">
              {{ probe.busy ? '试算中…' : '试算' }}
            </button>
          </div>
        </HField>
        <HField label="当成目录">
          <label class="checkline">
            <HSwitch v-model="probe.isDir" label="当成目录" />
            <span class="small">整个目录会被搬到目标位置（不拆开）</span>
          </label>
        </HField>

        <p v-if="probe.error" class="field-error">{{ probe.error }}</p>
        <div v-if="probe.item" class="result">
          <div class="row" style="gap: 8px; align-items: center">
            <span class="badge" :class="probe.item.kind === 'series' ? 'badge-info' : 'badge-accent'">
              {{ kindOf(probe.item.kind) }}
            </span>
            <strong>
              <TitleLink :type="probe.item.kind" :title="probe.item.title">{{ probe.item.title || '没认出来' }}</TitleLink>
            </strong>
            <span v-if="probe.item.year" class="muted">({{ probe.item.year }})</span>
            <span class="mono small muted">{{ probe.item.confidence }}%</span>
          </div>
          <p class="mono small" style="margin-top: 8px">
            目标：{{ probe.item.skip ? '不会整理' : `${probe.item.dir}${probe.item.file ? '/' + probe.item.file : ''}` }}
          </p>
          <p class="muted small" style="margin-top: 4px">
            {{ probe.item.skip ? '跳过原因：' + probe.item.skip : probe.item.reason }}
          </p>
        </div>
      </HPanel>

      <HPanel title="识别规则" desc="当前实现的真实规则；想让某种命名更好认，把文件名发我加规则">
        <div class="rules">
          <div class="rule">
            <strong class="small">季集</strong>
            <p class="muted small">
              <code class="mono">S01E03</code>、<code class="mono">1x03</code>、
              <code class="mono">第 1 季 第 3 集</code>、<code class="mono">第03集</code>（季号可从目录补）；
              带集号的按剧集处理
            </p>
          </div>
          <div class="rule">
            <strong class="small">年份</strong>
            <p class="muted small">
              取 <code class="mono">1900–2099</code> 里最靠后且不像分辨率/码率的四位数字：
              <code class="mono">沙丘2.2024.2160p</code> → 2024
            </p>
          </div>
          <div class="rule">
            <strong class="small">画质与来源</strong>
            <p class="muted small">
              <code class="mono">2160p / 1080p / WEB-DL / BluRay / H265 / 10bit</code> 这类标记从片名里剥掉，但不会改文件名
            </p>
          </div>
          <div class="rule">
            <strong class="small">落位</strong>
            <p class="muted small">
              电影 → <code class="mono">电影库/片名 (年份)/</code>；剧集（文件）→
              <code class="mono">剧集库/剧名 (年份)/Season N/</code>；剧集（目录）→
              <code class="mono">剧集库/剧名 (年份)/</code>，目录整体搬过去，不再套一层 Season
            </p>
          </div>
          <div class="rule">
            <strong class="small">把握度</strong>
            <p class="muted small">
              片名 + 年份 + 季集都认出来约 85% 以上；只有片名（没年份）约 60%。
              扫描结果里低于 70% 的默认不勾选，需要你自己确认后再选
            </p>
          </div>
          <div class="rule">
            <strong class="small">跳过</strong>
            <p class="muted small">
              非视频扩展名、小于「最小体积」的文件、认不出片名的条目都不动，只在结果里标出来
            </p>
          </div>
        </div>
      </HPanel>
    </template>

    <!-- 目录选择弹窗 -->
    <HModal
      v-model="picker.open"
      :title="picker.kind === 'target' ? '选择目标目录（搬进库里哪个文件夹）' : '选择待整理目录'"
      size="lg"
    >
      <template #desc>逐级进入 115 的目录，选中「新片堆在这里」的那一层</template>
      <div class="row-between" style="margin-bottom: 10px">
        <div class="crumb mono small">
          <template v-for="(s, i) in picker.stack" :key="s.id">
            <span v-if="i" class="muted"> / </span>{{ s.name }}
          </template>
        </div>
        <button class="btn btn-sm btn-tertiary" :disabled="picker.stack.length <= 1 || picker.busy" @click="upDir">
          <AppIcon name="folder" :size="14" />上一级
        </button>
      </div>
      <p v-if="picker.error" class="field-error">{{ picker.error }}</p>
      <div class="diritems">
        <button
          v-for="e in picker.entries"
          :key="e.id"
          class="diritem"
          @click="enterDir(e)"
        >
          <AppIcon name="folder" :size="16" />
          <span class="truncate">{{ e.name }}</span>
        </button>
        <p v-if="!picker.entries.length && !picker.busy" class="muted small">这一层没有子目录</p>
      </div>
      <template #footer>
        <span class="small muted">当前：{{ picker.stack[picker.stack.length - 1]?.name }}</span>
        <button class="btn btn-tertiary" @click="picker.open = false">取消</button>
        <button class="btn btn-primary" @click="chooseDir">用这个目录</button>
      </template>
    </HModal>

    <!-- 执行确认 -->
    <HModal v-model="confirmOpen" title="确认执行整理？">
      <template #desc>会真的移动网盘里的文件，不是预览</template>
      <div class="confirmbox">
        <p class="small">
          即将把 <strong>{{ scan.picked.length }}</strong> 个条目从
          <code class="mono">{{ cfg.source_name || cfg.source_cid }}</code>
          移动到目标媒体库目录。
        </p>
        <ul class="small muted" style="margin: 10px 0 0 18px">
          <li>115 上的位置会变；源目录整理完会空出来</li>
          <li>不会重命名文件，只移动</li>
          <li v-if="cfg.sync_after">移动后会自动给受影响的媒体库跑一轮增量同步</li>
          <li v-if="cfg.auto_ingest">
            之后 TG / 订阅 / 手动转存到资源目录的东西都会自动整理，并接着同步、刮削
          </li>
        </ul>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="confirmOpen = false">再想想</button>
        <button class="btn btn-primary" :disabled="running" @click="doRun">
          {{ running ? '整理中…' : '确认执行' }}
        </button>
      </template>
    </HModal>
  </div>
</template>

<style scoped>
/* 整理规则卡片：一个库挂两个网盘之后，规则可能有好几条，
   排成卡片比塞进一行四个下拉框清楚得多。 */
.rule-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.rule-card {
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  padding: 10px 12px;
  background: var(--surface-secondary);
}
.rule-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px 14px;
}
.rule-field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
@media (max-width: 720px) {
  .rule-grid {
    grid-template-columns: 1fr;
  }
}

.checkline {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
.pending {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 14px;
  padding: 8px 10px;
  border-radius: var(--r-sm);
  background: var(--warning-soft);
  color: var(--warning);
}
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.filterbar {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  padding: 12px 16px 0;
}
.warn {
  color: var(--warning);
}
/* 台账行：桌面一行装完，窄屏自动变成上下两段。
   以前这里是 table，六列在手机上互相挤，表头还会粘在上方盖住筛选条。 */
.reclist {
  display: flex;
  flex-direction: column;
  padding: 4px 8px;
}
.rec {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 8px;
}
.rec + .rec {
  border-top: 1px solid var(--separator);
}
.rec-main {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.rec-name {
  margin: 0;
}
.rec-tgt {
  margin: 0;
  color: var(--muted);
}
.rec-side {
  flex: none;
  display: flex;
  align-items: center;
  gap: 6px;
}
@media (max-width: 720px) {
  .rec {
    flex-direction: column;
    align-items: stretch;
    gap: 6px;
  }
  .rec-side {
    width: 100%;
    justify-content: space-between;
  }
}
/* 目标列拆两行：第一行「库名徽章 + 目录」，第二行灰掉的原始文件名。
   原来的写法把「目录/文件」拼成一行，省略号恰好落在文件名上，
   看不出来它到底要落到哪个目录 —— 而目录才是这一列的答案。 */
.tgt {
  display: flex;
  align-items: center;
  gap: 6px;
}
.tgt > .truncate {
  min-width: 0;
}
.tgtlib {
  flex: none;
  padding: 1px 7px;
  border-radius: var(--r-full);
  background: var(--accent-soft);
  color: var(--accent);
  font-family: var(--font-sans);
  font-size: 11px;
  line-height: 1.6;
}
.tgtfile {
  margin-top: 3px;
  font-size: 11px;
  color: var(--muted);
}
.is-off {
  opacity: 0.62;
}
.crumb {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.diritems {
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-height: 320px;
  overflow: auto;
}
.diritem {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
  color: inherit;
  cursor: pointer;
  text-align: left;
}
.diritem:hover {
  border-color: var(--border-secondary);
  background: var(--surface-secondary);
}
.result {
  margin-top: 6px;
  padding: 12px;
  border: 1px dashed var(--border-secondary);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.rules {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.rule p {
  margin-top: 4px;
  line-height: 1.7;
}
.confirmbox {
  padding: 12px;
  border-radius: var(--r-md);
  background: var(--warning-soft);
  color: var(--foreground);
}
</style>
