<script setup lang="ts">
/**
 * Strm 管理：状态、配置、全量、增量、孤儿清理。
 *
 * 这一页之前是纯示例数据（定时器假装进度条）。现在每个数字都来自后端：
 *  - 状态：GET /api/strm/overview（库 + 台账条数 + 内存里的 Job + 账号节流计数）
 *  - 配置：play_domain / sync_min_size / sync_delete_stale / sync_dedupe，
 *          以及每个库的 keep_ext（PUT /api/libraries/{id}/keep-ext）
 *  - 全量：POST /api/libraries/{id}/sync {force:true}，逐个库轮询 job
 *  - 增量：同上但 force:false，差异只在「内容没变的 .strm 不重写」
 *  - 孤儿：GET /api/strm/orphans 走磁盘，POST /api/strm/orphans/clean 删
 *
 * 一个刻意的取舍：页面上不再出现「并发度 / 命名模板」这类后端没有的开关。
 * 能点的都真的会生效，不能点的就不放出来——假开关比没有开关更伤人。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HField from '@/components/HField.vue'
import HModal from '@/components/HModal.vue'
import HPanel from '@/components/HPanel.vue'
import HTabs from '@/components/HTabs.vue'
import HSwitch from '@/components/HSwitch.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'

interface Job {
  library_id: number
  phase: string
  dir: string
  dirs: number
  files: number
  written: number
  removed: number
  failed: number
  skipped: number
  running: boolean
  message: string
  err?: string
  /** 多来源进度：一个库同时扫 115 和夸克时，告诉用户现在扫到哪一家 */
  source_name?: string
  source_index?: number
  source_total?: number
}
interface LibStatus {
  id: number
  name: string
  account_id: number
  account_name: string
  remote_path: string
  remote_cid: string
  local_path: string
  local_root: string
  collection_type: string
  keep_ext: boolean
  files: number
  last_sync_at: string
  last_sync_files: number
  /** 这个库挂的网盘目录，115 和夸克各一条 */
  sources?: { id: number; account_id: number; kind: string; account: string; remote_path: string; remote_cid: string; files: number }[]
  job?: Job
}
interface Run {
  id: number
  library_id: number
  library_name: string
  trigger: string
  phase: string
  files: number
  written: number
  removed: number
  failed: number
  skipped: number
  message: string
  started_at: string
  finished_at: string
}
interface Overview {
  libraries: LibStatus[]
  accounts: any[]
  runs: Run[]
  totals: { libraries: number; files: number; running: number }
  last_sync_at: string
  last_full_sync: string
  domain: string
}
interface OrphanLib {
  library_id: number
  name: string
  root: string
  scanned: number
  extra: string[]
  missing: string[]
  err?: string
}

const tab = ref('status')
const tabs = [
  { value: 'status', label: '同步状态' },
  { value: 'config', label: 'STRM 配置' },
  { value: 'full', label: '全量同步' },
  { value: 'incr', label: '增量同步' },
  { value: 'orphans', label: '孤儿清理' },
]

/* ==================== 总览 ==================== */
const ov = ref<Overview | null>(null)
const loading = ref(true)
const loadErr = ref('')

async function load() {
  try {
    ov.value = (await api.strm.overview()) as Overview
    loadErr.value = ''
  } catch (e: any) {
    loadErr.value = e?.message || '读取同步状态失败'
  } finally {
    loading.value = false
  }
}

let poll: number | undefined
onMounted(async () => {
  await Promise.all([load(), loadSettings()])
  // 有任务在跑就刷得勤一点，闲着的时候 8 秒一次足够
  poll = window.setInterval(() => {
    if (document.hidden) return
    if (ov.value?.totals.running || busy.value) void load()
  }, 5000)
})
onBeforeUnmount(() => {
  if (poll) window.clearInterval(poll)
})

/* ==================== 格式化 ==================== */
function fmtTime(s?: string) {
  if (!s) return '—'
  const d = new Date(s)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
function fmtDuration(a?: string, b?: string) {
  if (!a || !b) return '—'
  const ms = new Date(b).getTime() - new Date(a).getTime()
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  const s = Math.round(ms / 1000)
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60}s`
}
const triggerLabel = (t: string) =>
  ({ full: '全量', incremental: '增量', auto: '自动', manual: '手动' })[t] ?? t
const phaseLabel = (p: string) =>
  ({ done: '完成', error: '失败', canceled: '已取消', running: '进行中' })[p] ?? p

const lastFullRuns = computed(() => (ov.value?.runs ?? []).filter((r) => r.trigger === 'full').slice(0, 5))

/* ==================== 配置 ==================== */
const cfg = ref({ domain: '', minSize: 50, delStale: true, dedupe: true })
const cfgSaving = ref(false)
const rewriting = ref(false)

async function loadSettings() {
  try {
    const s = await api.config.settings()
    cfg.value.domain = s.play_domain || ''
    cfg.value.minSize = Number(s.sync_min_size || 0) || 0
    cfg.value.delStale = s.sync_delete_stale !== 'false'
    cfg.value.dedupe = s.sync_dedupe !== 'false'
  } catch (e: any) {
    toast.error('读取配置失败', e?.message)
  }
}

async function saveCfg() {
  cfgSaving.value = true
  try {
    await api.config.saveSettings({
      play_domain: cfg.value.domain.trim(),
      sync_min_size: String(cfg.value.minSize || 0),
      sync_delete_stale: cfg.value.delStale ? 'true' : 'false',
      sync_dedupe: cfg.value.dedupe ? 'true' : 'false',
    })
    toast.success('配置已保存', '下一轮同步生效')
    await Promise.all([load(), loadSettings()])
  } catch (e: any) {
    toast.error('保存失败', e?.message)
  } finally {
    cfgSaving.value = false
  }
}

async function useCurrentAddr() {
  try {
    const d = await api.config.netAddress()
    let host = d.host || ''
    if (!host || host === '0.0.0.0') host = location.hostname
    if (!host) return toast.warning('取不到当前地址，请手动填写')
    cfg.value.domain = `http://${host}:${d.play_port || 6086}`
    toast.info('已填入当前地址', '保存后新生成的 STRM 用这个地址')
  } catch (e: any) {
    toast.error('读取失败', e?.message)
  }
}

/** 改地址的同时把已有 .strm 全改一遍——不然库里一半新地址一半旧地址。 */
async function applyDomainEverywhere() {
  const domain = cfg.value.domain.trim()
  if (!domain) {
    toast.warning('先填播放地址')
    return
  }
  rewriting.value = true
  try {
    const r = await api.config.rewriteDomain(domain)
    toast.success(`已改写 ${r.changed}/${r.files} 个 STRM`, r.hint || '地址已保存')
    await Promise.all([load(), loadSettings()])
  } catch (e: any) {
    toast.error('改写失败', e?.message)
  } finally {
    rewriting.value = false
  }
}

async function toggleKeepExt(lib: LibStatus) {
  const next = !lib.keep_ext
  lib.keep_ext = next // 先动 UI，失败再回滚
  try {
    await api.libraries.setKeepExt(lib.id, next)
    toast.success(`「${lib.name}」${next ? '直链带扩展名' : '直链不带扩展名'}`, '下一轮同步生效')
  } catch (e: any) {
    lib.keep_ext = !next
    toast.error('修改失败', e?.message)
  }
}

/* ==================== 全量 / 增量 ==================== */
const picked = ref<number[]>([])
const delStale = ref(true)
const busy = ref(false)
const jobs = ref<Record<number, Job>>({})
const runKind = ref<'full' | 'incremental'>('full')

function toggleLib(id: number) {
  picked.value = picked.value.includes(id)
    ? picked.value.filter((x) => x !== id)
    : [...picked.value, id]
}
function pickAll() {
  const all = (ov.value?.libraries ?? []).map((l) => l.id)
  picked.value = picked.value.length === all.length ? [] : all
}

async function pollJobs() {
  for (;;) {
    let alive = false
    for (const id of picked.value) {
      try {
        const j = (await api.sync.job(id)) as Job
        jobs.value = { ...jobs.value, [id]: j }
        if (j.running) alive = true
      } catch {
        /* 单个库查不到不影响其它库 */
      }
    }
    await load()
    if (!alive) return
    await new Promise((r) => setTimeout(r, 1500))
  }
}

async function runSync(force: boolean) {
  if (!picked.value.length) {
    toast.warning('先选至少一个库')
    return
  }
  if (!cfg.value.domain) {
    toast.warning('还没配播放地址', '先到「STRM 配置」里填一个能被播放器访问的地址')
    tab.value = 'config'
    return
  }
  busy.value = true
  runKind.value = force ? 'full' : 'incremental'
  jobs.value = {}
  let ok = 0
  try {
    for (const id of picked.value) {
      try {
        const j = (await api.sync.start(id, { force, delete_stale: delStale.value })) as Job
        jobs.value = { ...jobs.value, [id]: j }
        ok++
      } catch (e: any) {
        toast.error('启动失败', e?.message)
      }
    }
    if (ok) {
      toast.success(force ? '全量同步已开始' : '增量同步已开始', `${ok} 个库，进度见下方`)
      await pollJobs()
      const res = Object.values(jobs.value)
      const written = res.reduce((n, j) => n + (j.written || 0), 0)
      const removed = res.reduce((n, j) => n + (j.removed || 0), 0)
      const failed = res.reduce((n, j) => n + (j.failed || 0), 0)
      toast.success('同步结束', `写入 ${written} · 清理 ${removed} · 失败 ${failed}`)
    }
  } finally {
    busy.value = false
  }
}

async function cancelLib(id: number) {
  try {
    await api.sync.cancel(id)
    toast.info('已请求停止')
  } catch (e: any) {
    toast.error('取消失败', e?.message)
  }
}

/* ==================== 孤儿清理 ==================== */
const orphans = ref<OrphanLib[] | null>(null)
const orphBusy = ref(false)
const orphErr = ref('')
const cleanOpen = ref(false)
const cleanLib = ref<OrphanLib | null>(null)
const cleaning = ref(false)

async function scanOrphans() {
  orphBusy.value = true
  orphErr.value = ''
  try {
    const r = await api.strm.orphans()
    orphans.value = (r.libraries ?? []) as OrphanLib[]
  } catch (e: any) {
    orphErr.value = e?.message || '扫描失败'
  } finally {
    orphBusy.value = false
  }
}

function askClean(lib: OrphanLib) {
  cleanLib.value = lib
  cleanOpen.value = true
}

async function doClean() {
  if (!cleanLib.value) return
  cleaning.value = true
  try {
    // all: true —— 扫描接口每类只回前 200 条，全清必须让服务端现场重扫
    const r = await api.strm.cleanOrphans({ library_id: cleanLib.value.library_id, all: true })
    toast.success(
      '清理完成',
      `删除 ${r.removed} 个 STRM · 清掉 ${r.rows} 条台账 · 跳过 ${r.skipped} · 失败 ${r.failed}`,
    )
    cleanOpen.value = false
    await Promise.all([scanOrphans(), load()])
  } catch (e: any) {
    toast.error('清理失败', e?.message)
  } finally {
    cleaning.value = false
  }
}

const orphanTotals = computed(() => {
  const list = orphans.value ?? []
  return list.reduce(
    (acc, l) => {
      acc.extra += l.extra.length
      acc.missing += l.missing.length
      return acc
    },
    { extra: 0, missing: 0 },
  )
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <HTabs v-model="tab" :items="tabs" />

    <p v-if="loadErr" class="err small">{{ loadErr }}</p>

    <!-- ==================== 同步状态 ==================== -->
    <template v-if="tab === 'status'">
      <div class="grid-4">
        <div class="stat">
          <span class="stat-label"><AppIcon name="local" :size="14" />媒体库</span>
          <span class="stat-value">{{ ov?.totals.libraries ?? 0 }}<small>个</small></span>
        </div>
        <div class="stat">
          <span class="stat-label"><AppIcon name="file" :size="14" />台账片目</span>
          <span class="stat-value">{{ (ov?.totals.files ?? 0).toLocaleString() }}</span>
        </div>
        <div class="stat">
          <span class="stat-label"><AppIcon name="sync" :size="14" />运行中</span>
          <span class="stat-value">{{ ov?.totals.running ?? 0 }}<small>个库</small></span>
        </div>
        <div class="stat">
          <span class="stat-label"><AppIcon name="clock" :size="14" />上次同步</span>
          <span class="stat-value" style="font-size: 16px">{{ fmtTime(ov?.last_sync_at) }}</span>
        </div>
      </div>

      <HPanel title="各库同步状态" desc="台账条数就是应有的 STRM 数；进度条是内存里正在跑的那一轮" flush>
        <div v-if="!(ov?.libraries ?? []).length" class="pad muted small">
          还没有媒体库。先到「账号与媒体库」选一个 115 / 夸克TV 目录建库。
        </div>
        <div class="list">
          <div v-for="l in ov?.libraries ?? []" :key="l.id" class="item">
            <span class="avatar">{{ l.name.slice(0, 1) }}</span>
            <div class="grow">
              <div class="row-between">
                <div class="row" style="gap: 8px">
                  <strong class="small">{{ l.name }}</strong>
                  <span
                    class="badge"
                    :class="
                      l.job?.running
                        ? 'badge-accent'
                        : l.job?.phase === 'error'
                          ? 'badge-warning'
                          : l.last_sync_at
                            ? 'badge-success'
                            : ''
                    "
                  >
                    {{
                      l.job?.running
                        ? `同步中 · ${l.job.phase}`
                        : l.job?.phase === 'error'
                          ? '上轮失败'
                          : l.last_sync_at
                            ? '已同步'
                            : '未同步'
                    }}
                  </span>
                  <span class="muted small mono">{{ l.files.toLocaleString() }} 个 STRM</span>
                </div>
                <span class="muted small">{{ fmtTime(l.last_sync_at) }}</span>
              </div>
              <div class="row small muted" style="gap: 8px; margin-top: 3px; flex-wrap: wrap">
                <template v-if="(l.sources?.length ?? 0) > 1">
                  <span v-for="(src, i) in l.sources" :key="src.id" class="mono">
                    {{ src.kind === 'quarktv' ? '夸克' : '115' }}:{{ src.remote_path || src.remote_cid }}<span
                      v-if="i < (l.sources?.length ?? 0) - 1"
                    >、</span>
                  </span>
                </template>
                <span v-else class="mono">{{ l.remote_path || l.remote_cid }}</span>
                <span>→</span>
                <span class="mono">{{ l.local_root }}</span>
                <span v-if="l.account_name">· {{ l.account_name }}</span>
              </div>
              <div v-if="l.job?.running" class="col" style="gap: 4px; margin-top: 8px">
                <div class="bar"><i :style="{ width: l.job.phase === 'clean' ? '88%' : '55%' }" /></div>
                <span class="muted small">
                  {{ l.job.message || l.job.phase }}
                  <template v-if="(l.job.source_total ?? 0) > 1">
                    · 来源 {{ l.job.source_index }}/{{ l.job.source_total }} {{ l.job.source_name }}
                  </template>
                  <template v-if="l.job.dir"> · {{ l.job.dir }}</template>
                  · 已写 {{ l.job.written }} · 清理 {{ l.job.removed }}
                </span>
              </div>
            </div>
            <div class="row" style="gap: 4px">
              <button
                v-if="l.job?.running"
                class="btn btn-sm btn-ghost"
                title="停止"
                style="color: var(--danger)"
                @click="cancelLib(l.id)"
              >
                <AppIcon name="close" :size="14" />
              </button>
              <template v-else>
                <button
                  class="btn btn-sm btn-ghost"
                  title="增量同步"
                  @click="picked = [l.id]; delStale = cfg.delStale; runSync(false)"
                >
                  <AppIcon name="refresh" :size="14" />
                </button>
                <button
                  class="btn btn-sm btn-ghost"
                  title="全量同步"
                  @click="picked = [l.id]; delStale = cfg.delStale; runSync(true)"
                >
                  <AppIcon name="sync" :size="14" />
                </button>
              </template>
            </div>
          </div>
        </div>
      </HPanel>

      <HPanel title="115 通道状态" desc="请求数、重试、风控命中都来自这个进程内的真实计数">
        <div class="grid-3">
          <div v-for="a in ov?.accounts ?? []" :key="a.id" class="tipbox">
            <div class="row" style="gap: 8px">
              <strong class="small">{{ a.name }}</strong>
              <span class="badge" :class="a.live ? 'badge-success' : ''">
                {{ a.live ? '已连接' : '未打开' }}
              </span>
            </div>
            <p class="muted small mono" style="margin-top: 6px">
              请求 {{ a.requests }} · 重试 {{ a.retries }} · 风控 {{ a.risk_hits }}
            </p>
            <p v-if="a.cooldown_until" class="small" style="color: var(--warning); margin-top: 4px">
              冷却到 {{ fmtTime(a.cooldown_until) }}
            </p>
            <p v-if="a.last_error" class="small muted" style="margin-top: 4px">
              最近错误：{{ a.last_error }}
            </p>
          </div>
          <div v-if="!(ov?.accounts ?? []).length" class="tipbox muted small">
            还没有 115 账号。
          </div>
        </div>
      </HPanel>
    </template>

    <!-- ==================== STRM 配置 ==================== -->
    <template v-else-if="tab === 'config'">
      <HPanel title="播放地址" desc="每个 .strm 里写的就是这个地址，必须能被 Emby 和播放器访问到">
        <HField
          label="播放域名"
          tip="形如 http://192.168.1.10:6086；不要填 127.0.0.1 —— Emby 在另一个容器里时它指向自己"
        >
          <div class="row" style="gap: 8px">
            <input v-model="cfg.domain" class="input mono" placeholder="http://192.168.1.10:6086" />
            <button class="btn btn-tertiary" @click="useCurrentAddr">
              <AppIcon name="link" :size="14" />用当前地址
            </button>
          </div>
        </HField>
        <p class="muted small" style="margin-top: -4px">
          改完点「保存并改写已有 STRM」：只保存不会动老文件，库里会一半新一半旧。
        </p>
        <div class="tipbox muted small">
          这里只管「Emby 能不能取到地址」。Emby 自己不做 302：它取到直链后会把视频
          拉下来再转发给播放器，客户端看到的永远是 Emby 的地址。想让播放器直连 CDN，
          去「设置 → EMBY 管理 → 播放直连（302 网关）」开启网关（默认端口 18096），
          播放请求会被直接 302 到网盘直链，视频不再经过这台机器。
        </div>
        <template #footer>
          <button class="btn btn-tertiary" :disabled="cfgSaving" @click="saveCfg">
            {{ cfgSaving ? '保存中…' : '仅保存' }}
          </button>
          <button class="btn btn-primary" :disabled="rewriting" @click="applyDomainEverywhere">
            {{ rewriting ? '改写中…' : '保存并改写已有 STRM' }}
          </button>
        </template>
      </HPanel>

      <HPanel title="同步规则" desc="这几个开关直接决定下一轮同步怎么写盘">
        <HField label="最小体积" tip="小于这个体积的视频不写 .strm，用来挡样片和预告">
          <div class="row" style="gap: 8px">
            <input v-model.number="cfg.minSize" type="number" min="0" class="input" style="max-width: 140px" />
            <span class="muted small">MB（0 = 不过滤）</span>
          </div>
        </HField>
        <HField label="删除失效" hint="网盘里已经不见的文件，本地 .strm 跟着删掉">
          <label class="checkline"><HSwitch v-model="cfg.delStale" />同步时清理失效条目</label>
        </HField>
        <HField label="同片去重" hint="同一部作品在网盘里有多个副本时，只保留体积最大的那条">
          <label class="checkline"><HSwitch v-model="cfg.dedupe" />一个作品只留一条 STRM</label>
        </HField>
        <template #footer>
          <button class="btn btn-primary" :disabled="cfgSaving" @click="saveCfg">
            {{ cfgSaving ? '保存中…' : '保存规则' }}
          </button>
        </template>
      </HPanel>

      <HPanel title="各库直链格式" desc="有的播放器靠 URL 后缀判断容器格式，播放失败可以先试这里" flush>
        <div class="list">
          <div v-for="l in ov?.libraries ?? []" :key="l.id" class="item">
            <div class="grow">
              <strong class="small">{{ l.name }}</strong>
              <div class="muted small mono" style="margin-top: 2px">{{ l.local_root }}</div>
            </div>
            <label class="checkline">
              <HSwitch :model-value="l.keep_ext" @update:model-value="toggleKeepExt(l)" />
              直链带扩展名
            </label>
          </div>
          <div v-if="!(ov?.libraries ?? []).length" class="pad muted small">还没有媒体库。</div>
        </div>
      </HPanel>
    </template>

    <!-- ==================== 全量 ==================== -->
    <template v-else-if="tab === 'full'">
      <div class="grid-2">
        <HPanel title="全量同步" desc="内容没变也重写，把每个 STRM 的 mtime 刷新，让 Emby 整库重扫">
          <p class="section-title">选择库</p>
          <label
            v-for="l in ov?.libraries ?? []"
            :key="l.id"
            class="lib-pick"
            :class="{ 'is-on': picked.includes(l.id) }"
          >
            <input type="checkbox" :checked="picked.includes(l.id)" @change="toggleLib(l.id)" />
            <span class="grow">
              <strong class="small">{{ l.name }}</strong>
              <span class="muted small"> · {{ l.files.toLocaleString() }} 个 STRM</span>
            </span>
            <span class="mono small muted">
              {{ (l.sources?.length ?? 0) > 1 ? `${l.sources!.length} 个网盘目录` : l.remote_path || l.remote_cid }}
            </span>
          </label>
          <div class="row" style="gap: 8px; margin-top: 10px">
            <button class="btn btn-sm btn-tertiary" @click="pickAll">
              {{ picked.length === (ov?.libraries ?? []).length ? '全不选' : '全选' }}
            </button>
            <span class="muted small">已选 {{ picked.length }} 个</span>
          </div>

          <div class="col" style="gap: 10px; margin-top: 16px">
            <p class="section-title">选项</p>
            <label class="checkline"><HSwitch v-model="delStale" />顺带清理台账里失效的 STRM</label>
          </div>

          <div v-if="busy" class="col" style="gap: 8px; margin-top: 16px">
            <div v-for="l in ov?.libraries ?? []" :key="l.id">
              <template v-if="jobs[l.id]">
                <div class="row-between small">
                  <span>{{ l.name }} · {{ jobs[l.id].phase }}</span>
                  <span class="mono">{{ jobs[l.id].written }} 写入</span>
                </div>
                <div class="bar" style="margin-top: 4px">
                  <i :style="{ width: jobs[l.id].running ? '65%' : '100%' }" />
                </div>
              </template>
            </div>
          </div>

          <template #footer>
            <button class="btn btn-primary" :disabled="busy" @click="runSync(true)">
              <AppIcon name="play" :size="14" />
              {{ busy ? '同步中…' : '开始全量同步' }}
            </button>
          </template>
        </HPanel>

        <HPanel title="上次全量" :desc="`最近一次成功：${fmtTime(ov?.last_full_sync)}`" flush>
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>时间</th>
                  <th>库</th>
                  <th class="num">写入</th>
                  <th class="num">清理</th>
                  <th class="num">耗时</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="r in lastFullRuns" :key="r.id">
                  <td class="mono small">{{ fmtTime(r.started_at) }}</td>
                  <td class="small">{{ r.library_name }}</td>
                  <td class="num mono small">{{ r.written }}</td>
                  <td class="num mono small">{{ r.removed }}</td>
                  <td class="num mono small">{{ fmtDuration(r.started_at, r.finished_at) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-if="!lastFullRuns.length" class="pad muted small">
            还没有跑过全量。全量要逐个目录列文件，万级库很慢；日常用增量就够了。
          </p>
        </HPanel>
      </div>
    </template>

    <!-- ==================== 增量 ==================== -->
    <template v-else-if="tab === 'incr'">
      <div class="grid-2">
        <HPanel title="增量同步" desc="只补缺失与变化的文件，内容没变的 STRM 一个字节都不写">
          <p class="section-title">选择库</p>
          <label
            v-for="l in ov?.libraries ?? []"
            :key="l.id"
            class="lib-pick"
            :class="{ 'is-on': picked.includes(l.id) }"
          >
            <input type="checkbox" :checked="picked.includes(l.id)" @change="toggleLib(l.id)" />
            <span class="grow">
              <strong class="small">{{ l.name }}</strong>
              <span class="muted small"> · 上次 {{ fmtTime(l.last_sync_at) }}</span>
            </span>
          </label>
          <div class="row" style="gap: 8px; margin-top: 10px">
            <button class="btn btn-sm btn-tertiary" @click="pickAll">
              {{ picked.length === (ov?.libraries ?? []).length ? '全不选' : '全选' }}
            </button>
            <span class="muted small">已选 {{ picked.length }} 个</span>
          </div>

          <div class="col" style="gap: 10px; margin-top: 16px">
            <label class="checkline"><HSwitch v-model="delStale" />顺带清理失效条目</label>
            <p class="muted small">
              增量与全量用的是同一套同步逻辑，区别只有一处：内容没变的 .strm 不重写。
              网盘里改名/移动的文件，增量也能认出来（台账记的是 file_id）。
            </p>
          </div>

          <template #footer>
            <button class="btn btn-primary" :disabled="busy" @click="runSync(false)">
              <AppIcon name="play" :size="14" />
              {{ busy ? '同步中…' : '立即跑一轮' }}
            </button>
          </template>
        </HPanel>

        <HPanel title="最近轮次" desc="每轮都落库，重启也不会丢" flush>
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>时间</th>
                  <th>库</th>
                  <th>触发</th>
                  <th>STRM</th>
                  <th>结果</th>
                  <th class="num">耗时</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="r in (ov?.runs ?? []).slice(0, 12)" :key="r.id">
                  <td class="mono small">{{ fmtTime(r.started_at) }}</td>
                  <td class="small">{{ r.library_name }}</td>
                  <td class="small muted">{{ triggerLabel(r.trigger) }}</td>
                  <td class="mono small">+{{ r.written }} / -{{ r.removed }}</td>
                  <td>
                    <span
                      class="badge"
                      :class="
                        r.phase === 'done'
                          ? 'badge-success'
                          : r.phase === 'error'
                            ? 'badge-warning'
                            : ''
                      "
                    >
                      {{ phaseLabel(r.phase) }}
                    </span>
                  </td>
                  <td class="num mono small">{{ fmtDuration(r.started_at, r.finished_at) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-if="!(ov?.runs ?? []).length" class="pad muted small">还没有同步记录。</p>
        </HPanel>
      </div>
    </template>

    <!-- ==================== 孤儿清理 ==================== -->
    <template v-else>
      <HPanel title="本地孤儿" desc="盘上有、台账没有的 STRM，以及台账有、盘上没了的条目">
        <div class="row" style="gap: 10px">
          <button class="btn btn-tertiary" :disabled="orphBusy" @click="scanOrphans">
            <AppIcon name="search" :size="14" />{{ orphBusy ? '扫描中…' : '扫描全部库' }}
          </button>
          <span v-if="orphans" class="muted small">
            孤儿 {{ orphanTotals.extra }} 个 · 失效 {{ orphanTotals.missing }} 条
          </span>
          <span v-if="orphErr" class="err small">{{ orphErr }}</span>
        </div>
        <p class="muted small" style="margin-top: 10px; line-height: 1.7">
          扫描是走磁盘的，库大时十几秒，所以放在按钮后面而不是自动跑。
          「孤儿」多半是同步中断留下的残骸；「失效」是台账里有记录但 .strm 已经被人删了。
          清理只删 .strm 与台账行，不动 NFO 和图片。
        </p>
      </HPanel>

      <template v-if="orphans">
        <HPanel
          v-for="l in orphans"
          :key="l.library_id"
          :title="l.name"
          :desc="`${l.root} · 盘上 ${l.scanned} 个 STRM`"
          flush
        >
          <template #actions>
            <button
              class="btn btn-sm btn-danger"
              :disabled="!l.extra.length && !l.missing.length"
              @click="askClean(l)"
            >
              <AppIcon name="trash" :size="14" />清理这个库
            </button>
          </template>
          <p v-if="l.err" class="pad err small">{{ l.err }}</p>
          <div v-else class="orphan-grid">
            <div>
              <p class="section-title">盘上有、台账没有（{{ l.extra.length }}）</p>
              <ul class="paths">
                <li v-for="p in l.extra" :key="p" class="mono small">{{ p }}</li>
                <li v-if="!l.extra.length" class="muted small">没有</li>
              </ul>
            </div>
            <div>
              <p class="section-title">台账有、盘上没有（{{ l.missing.length }}）</p>
              <ul class="paths">
                <li v-for="p in l.missing" :key="p" class="mono small">{{ p }}</li>
                <li v-if="!l.missing.length" class="muted small">没有</li>
              </ul>
            </div>
          </div>
        </HPanel>
      </template>

      <HModal v-model="cleanOpen" title="清理孤儿" size="md">
        <template #desc>会删除该库里所有不在台账中的 .strm，并清掉指向已消失文件的台账行。</template>
        <div class="danger-box">
          <AppIcon name="alert" :size="18" />
          <div>
            <strong>{{ cleanLib?.name }}</strong>
            <p class="muted small">
              清理前会重新扫一遍磁盘（不用上次的结果）。只删 .strm 和台账行，
              NFO、海报、字幕都保留；误删的 STRM 跑一次增量同步就能补回来。
            </p>
          </div>
        </div>
        <template #footer>
          <button class="btn btn-tertiary" @click="cleanOpen = false">取消</button>
          <button class="btn btn-danger" :disabled="cleaning" @click="doClean">
            {{ cleaning ? '清理中…' : '确认清理' }}
          </button>
        </template>
      </HModal>
    </template>
  </div>
</template>

<style scoped>
.list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 16px;
}
.pad {
  padding: 14px 16px;
}
.checkline {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  white-space: nowrap;
}
.lib-pick {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border-radius: var(--r-sm);
  border: 1px solid var(--border);
  cursor: pointer;
  transition: all 0.16s;
  margin-bottom: 8px;
}
.lib-pick.is-on {
  border-color: color-mix(in oklab, var(--accent) 42%, transparent);
  background: var(--accent-soft);
}
.lib-pick input {
  accent-color: var(--accent);
}
.tipbox {
  padding: 14px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.tipbox p {
  margin-top: 6px;
  line-height: 1.6;
}
.orphan-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
  padding: 12px 16px 16px;
}
.paths {
  margin: 6px 0 0;
  padding: 0;
  list-style: none;
  max-height: 220px;
  overflow: auto;
}
.paths li {
  padding: 3px 0;
  border-bottom: 1px dashed var(--separator);
  word-break: break-all;
}
.danger-box {
  display: flex;
  gap: 12px;
  padding: 14px 16px;
  border-radius: var(--r-md);
  border: 1px solid color-mix(in oklab, var(--danger) 28%, transparent);
  background: var(--danger-soft);
}
.danger-box svg {
  color: var(--danger);
  flex: none;
  margin-top: 1px;
}
.danger-box p {
  margin-top: 4px;
  line-height: 1.65;
}
@media (max-width: 860px) {
  .orphan-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
