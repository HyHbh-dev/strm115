<script setup lang="ts">
/**
 * 本地文件：直接看宿主 /media 里到底落了什么。
 *
 * 和「网盘文件」是两回事：那一页读 115，这一页读磁盘。
 *  - 网盘里有、本地没有 = 还没同步（去 Strm 管理点增量）
 *  - 本地有、台账里没有 = 广告壳 / 手放的海报 / 删了库条目留下的 .strm
 * 第二种是这一页存在的理由：以前只能 SSH 进去 ls。
 *
 * 三个刻意的取舍：
 * 1. 只做「看 + 删」，不做重命名/移动。本地镜像是同步生成的，
 *    手改名字下一轮同步就还原，等于骗自己。
 * 2. 删除不改台账。台账是「应该有什么」，本地是「实际有什么」；
 *    删一个本地文件不该悄悄改掉库内容。台账里还留着的，删除后会明确
 *    告诉用户「下一轮同步会写回来」。
 * 3. 体积 / 孤儿数要手动点「统计」才算。遍历开销随库大小线性增长，
 *    不该塞进每次进页面。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import HModal from '@/components/HModal.vue'
import HPanel from '@/components/HPanel.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { fmtSize } from '@/utils/format'

interface LibLocal {
  id: number
  name: string
  collection_type: string
  root: string
  exists: boolean
  ledger: number
}
interface Entry {
  name: string
  path: string
  is_dir: boolean
  size: number
  mtime: string
  ext?: string
  known: boolean
}
interface Crumb {
  name: string
  path: string
}
interface Browse {
  library: LibLocal
  path: string
  crumbs: Crumb[]
  entries: Entry[]
  truncated: boolean
  dirs: number
  files: number
  orphans: number
}
interface Stat {
  library_id: number
  root: string
  files: number
  dirs: number
  bytes: number
  orphans: number
  empty_dirs: number
  scanned_at: string
  truncated: boolean
  err?: string
}

const libs = ref<LibLocal[]>([])
const libId = ref(0)
const cur = ref<Browse | null>(null)
const stat = ref<Stat | null>(null)
const loading = ref(false)
const statting = ref(false)
const err = ref('')
const keyword = ref('')

/* ---------------- 删除弹窗 ---------------- */
const del = ref<{ open: boolean; entry: Entry | null; recursive: boolean; busy: boolean }>({
  open: false,
  entry: null,
  recursive: false,
  busy: false,
})

const lib = computed(() => libs.value.find((l) => l.id === libId.value) ?? null)
const rows = computed(() => {
  const list = cur.value?.entries ?? []
  const kw = keyword.value.trim().toLowerCase()
  if (!kw) return list
  return list.filter((e) => e.name.toLowerCase().includes(kw))
})
const emptyDir = computed(() => !!cur.value && cur.value.entries.length === 0)

/* ---------------- 格式化 ---------------- */
function fmtTime(s: string) {
  if (!s) return '—'
  const t = new Date(s)
  if (Number.isNaN(t.getTime()) || t.getFullYear() < 1971) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${t.getFullYear()}-${p(t.getMonth() + 1)}-${p(t.getDate())} ${p(t.getHours())}:${p(t.getMinutes())}`
}
function kindText(e: Entry) {
  if (e.is_dir) return '目录'
  const ext = (e.ext || '').toLowerCase()
  if (ext === 'strm') return 'STRM'
  if (['jpg', 'jpeg', 'png', 'webp'].includes(ext)) return '图片'
  if (ext === 'nfo') return 'NFO'
  if (['srt', 'ass', 'ssa', 'vtt'].includes(ext)) return '字幕'
  return ext ? ext.toUpperCase() : '文件'
}

/* ---------------- 数据 ---------------- */
async function loadLibs() {
  try {
    const r = await api.local.overview()
    libs.value = r?.libraries ?? []
    if (!libs.value.length) return
    // 默认选第一个存在的库：目录不在的库点进去只会得到一句报错
    const first = libs.value.find((l) => l.exists) ?? libs.value[0]
    if (!libId.value) libId.value = first.id
  } catch (e: any) {
    toast.error('读取媒体库失败', e?.message)
  }
}

async function browse(path = '') {
  if (!libId.value) return
  loading.value = true
  err.value = ''
  try {
    cur.value = (await api.local.browse(libId.value, path)) as Browse
  } catch (e: any) {
    err.value = e?.message || '读取目录失败'
    cur.value = null
  } finally {
    loading.value = false
  }
}

async function runStat() {
  if (!libId.value) return
  statting.value = true
  try {
    const r = (await api.local.stat(libId.value)) as Stat
    stat.value = r
    if (r?.err) toast.error('统计没跑完', r.err)
  } catch (e: any) {
    toast.error('统计失败', e?.message)
  } finally {
    statting.value = false
  }
}

function switchLib(id: number) {
  if (id === libId.value) return
  libId.value = id
  keyword.value = ''
}

watch(libId, () => {
  stat.value = null
  void browse('')
})

function enter(e: Entry) {
  if (!e.is_dir) return
  keyword.value = ''
  void browse(e.path)
}
function goCrumb(c: Crumb) {
  keyword.value = ''
  void browse(c.path)
}
function upOne() {
  const crumbs = cur.value?.crumbs ?? []
  if (crumbs.length <= 1) return
  goCrumb(crumbs[crumbs.length - 2])
}

/* ---------------- 删除 ---------------- */
function askDelete(e: Entry) {
  del.value = { open: true, entry: e, recursive: false, busy: false }
}

async function confirmDelete() {
  const e = del.value.entry
  if (!e) return
  del.value.busy = true
  try {
    const r = await api.local.remove({
      library_id: libId.value,
      path: e.path,
      recursive: del.value.recursive,
    })
    toast.success(`已删除 ${e.name}`, r?.hint || '')
    del.value.open = false
    stat.value = null
    await browse(cur.value?.path ?? '')
  } catch (e2: any) {
    toast.error('删除失败', e2?.message)
  } finally {
    del.value.busy = false
  }
}

onMounted(async () => {
  await loadLibs()
  if (libId.value) await browse('')
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <HPanel
      title="本地文件"
      desc="宿主 /media 里真实落了什么。删除只动本地，不改同步台账"
    >
      <template #actions>
        <button class="btn btn-sm btn-tertiary" :disabled="statting || !libId" @click="runStat">
          <AppIcon name="refresh" :size="14" />
          {{ statting ? '统计中…' : '统计体积' }}
        </button>
      </template>

      <div v-if="!libs.length" class="empty" style="padding: 28px 0">
        <p class="empty-title">还没有媒体库</p>
        <p class="empty-desc">
          先去 <RouterLink class="link" to="/accounts">账号与媒体库</RouterLink> 建一个，本地目录才会出现
        </p>
      </div>

      <template v-else>
        <div class="libbar">
          <button
            v-for="l in libs"
            :key="l.id"
            class="libcard"
            :class="{ 'is-active': l.id === libId, 'is-missing': !l.exists }"
            @click="switchLib(l.id)"
          >
            <div class="libname">
              <AppIcon :name="l.collection_type === 'tvshows' ? 'local' : 'folder'" :size="14" />
              <span class="truncate">{{ l.name }}</span>
            </div>
            <div class="libpath mono truncate">{{ l.root || '未配置本地目录' }}</div>
            <div class="libmeta small">
              台账 {{ l.ledger }} 条
              <span v-if="!l.exists" class="warn">· 目录不存在</span>
            </div>
          </button>
        </div>

        <div v-if="stat && stat.library_id === libId" class="stats">
          <div class="stat"><span class="num">{{ stat.files }}</span><span class="lbl">文件</span></div>
          <div class="stat"><span class="num">{{ stat.dirs }}</span><span class="lbl">目录</span></div>
          <div class="stat"><span class="num">{{ fmtSize(stat.bytes) }}</span><span class="lbl">总体积</span></div>
          <div class="stat" :class="{ 'is-warn': stat.orphans > 0 }">
            <span class="num">{{ stat.orphans }}</span><span class="lbl">不在台账</span>
          </div>
          <div class="stat" :class="{ 'is-warn': stat.empty_dirs > 0 }">
            <span class="num">{{ stat.empty_dirs }}</span><span class="lbl">空目录</span>
          </div>
          <span class="small muted" style="margin-left: auto">
            统计于 {{ fmtTime(stat.scanned_at) }}<template v-if="stat.truncated">（文件太多，只算了前 5 万个）</template>
          </span>
        </div>
      </template>
    </HPanel>

    <HPanel v-if="libId" title="目录" :desc="cur?.library.root || lib?.root || ''" flush>
      <template #actions>
        <div class="row" style="gap: 6px">
          <input v-model="keyword" class="input input-sm" placeholder="筛当前层" style="width: 150px" />
          <button class="btn btn-sm btn-tertiary" :disabled="!cur || cur.crumbs.length <= 1" @click="upOne">
            <AppIcon name="folder" :size="14" />上一级
          </button>
          <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="browse(cur?.path ?? '')">
            <AppIcon name="refresh" :size="14" />刷新
          </button>
        </div>
      </template>

      <div class="crumb mono small">
        <template v-for="(c, i) in cur?.crumbs ?? []" :key="c.path || 'root'">
          <span v-if="i" class="muted"> / </span>
          <button v-if="i < (cur?.crumbs.length ?? 1) - 1" class="crumb-link" @click="goCrumb(c)">
            {{ c.name }}
          </button>
          <span v-else>{{ c.name }}</span>
        </template>
      </div>

      <p v-if="err" class="field-error" style="padding: 0 16px">{{ err }}</p>

      <div v-else-if="loading" class="empty" style="padding: 28px 0">
        <p class="empty-title">读取中…</p>
      </div>

      <div v-else-if="emptyDir" class="empty" style="padding: 28px 0">
        <p class="empty-title">这一层是空的</p>
        <p class="empty-desc">如果这里本该有内容，去「Strm 管理」跑一轮增量同步</p>
      </div>

      <div v-else-if="!rows.length" class="empty" style="padding: 28px 0">
        <p class="empty-title">没有匹配的条目</p>
        <p class="empty-desc">筛选词：{{ keyword }}</p>
      </div>

      <div v-else class="rows">
        <div v-for="e in rows" :key="e.path" class="rowitem" :class="{ 'is-dir': e.is_dir }">
          <button v-if="e.is_dir" class="main" @click="enter(e)">
            <AppIcon name="folder" :size="16" class="ico" />
            <span class="txt">
              <span class="name truncate">{{ e.name }}</span>
              <span class="meta small muted">{{ fmtTime(e.mtime) }} · 目录</span>
            </span>
            <span class="badge badge-info">目录</span>
          </button>
          <div v-else class="main">
            <AppIcon name="file" :size="16" class="ico" />
            <span class="txt">
              <span class="name truncate" :title="e.name">{{ e.name }}</span>
              <span class="meta small muted">{{ kindText(e) }} · {{ fmtSize(e.size) }} · {{ fmtTime(e.mtime) }}</span>
            </span>
            <span class="badge" :class="e.known ? 'badge-success' : 'badge-warning'">
              {{ e.known ? '台账内' : '不在台账' }}
            </span>
          </div>
          <button class="btn btn-sm btn-ghost del" :title="`删除 ${e.name}`" @click="askDelete(e)">
            <AppIcon name="trash" :size="14" />
          </button>
        </div>
      </div>

      <template #footer>
        <span class="small muted">
          {{ cur?.path || '库根' }} · 目录 {{ cur?.dirs ?? 0 }} · 文件 {{ cur?.files ?? 0 }}
          <template v-if="cur?.orphans">· <span class="warn">不在台账 {{ cur.orphans }}</span></template>
          <template v-if="cur?.truncated">· 条目太多，只显示前 1000 条</template>
        </span>
      </template>
    </HPanel>

    <HModal v-model="del.open" title="删除本地文件？">
      <template #desc>只动磁盘，不改同步台账</template>
      <div class="confirmbox" v-if="del.entry">
        <p class="small">
          <strong>{{ del.entry.name }}</strong>
          <template v-if="!del.entry.is_dir"> · {{ fmtSize(del.entry.size) }}</template>
        </p>
        <p class="small muted mono" style="margin-top: 6px">{{ del.entry.path }}</p>
        <label v-if="del.entry.is_dir" class="checkline">
          <input v-model="del.recursive" type="checkbox" />
          <span class="small">连同目录里的所有文件一起删</span>
        </label>
        <p v-if="del.entry.is_dir && !del.recursive" class="small muted" style="margin-top: 8px">
          空目录可以直接删；非空目录需要勾上上面那一项
        </p>
        <p v-if="del.entry.known" class="small warn" style="margin-top: 8px">
          这条在同步台账里：删掉之后，下一轮同步会把它重新写回来。要彻底不要，请先删 115 上的文件。
        </p>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="del.open = false">取消</button>
        <button class="btn btn-danger" :disabled="del.busy" @click="confirmDelete">
          {{ del.busy ? '删除中…' : '删除' }}
        </button>
      </template>
    </HModal>
  </div>
</template>

<style scoped>
.link {
  color: var(--accent);
  text-decoration: underline;
  text-underline-offset: 2px;
}
.libbar {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 10px;
  margin-bottom: 14px;
}
.libcard {
  display: grid;
  gap: 4px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  color: inherit;
  text-align: left;
  cursor: pointer;
  transition: border-color 0.16s, background 0.16s;
  min-width: 0;
}
.libcard:hover {
  border-color: var(--border-secondary);
}
.libcard.is-active {
  border-color: color-mix(in oklab, var(--accent) 45%, transparent);
  background: var(--accent-soft);
}
.libcard.is-missing {
  opacity: 0.66;
}
.libname {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
  font-weight: 600;
  min-width: 0;
}
.libpath {
  font-size: 11.5px;
  color: var(--muted);
  min-width: 0;
}
.libmeta {
  color: var(--muted);
}
.stats {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 20px;
  padding: 12px 14px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border-secondary);
}
.stat {
  display: flex;
  align-items: baseline;
  gap: 6px;
}
.stat .num {
  font-size: 18px;
  font-weight: 650;
  letter-spacing: -0.01em;
}
.stat .lbl {
  font-size: 12px;
  color: var(--muted);
}
.stat.is-warn .num {
  color: var(--warning);
}
.crumb {
  padding: 12px 16px 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.crumb-link {
  background: none;
  border: 0;
  padding: 0;
  color: var(--accent);
  cursor: pointer;
  font: inherit;
}
.crumb-link:hover {
  text-decoration: underline;
  text-underline-offset: 2px;
}
.rows {
  display: flex;
  flex-direction: column;
  padding: 8px 8px 4px;
}
.rowitem {
  display: flex;
  align-items: center;
  gap: 6px;
  border-radius: var(--r-sm);
  padding-right: 4px;
  min-width: 0;
}
.rowitem:hover {
  background: var(--surface-secondary);
}
.rowitem + .rowitem {
  border-top: 1px solid var(--separator);
}
.main {
  display: flex;
  align-items: center;
  gap: 10px;
  flex: 1;
  min-width: 0;
  padding: 9px 10px;
  background: none;
  border: 0;
  color: inherit;
  text-align: left;
  cursor: default;
  font: inherit;
}
.is-dir .main {
  cursor: pointer;
}
.ico {
  flex: none;
  color: var(--muted);
}
.is-dir .ico {
  color: var(--accent);
}
.txt {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  flex: 1;
}
.name {
  font-size: 13px;
}
.meta {
  font-size: 11.5px;
}
.del {
  flex: none;
  color: var(--muted);
}
.del:hover {
  color: var(--danger);
}
.checkline {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 10px;
}
.confirmbox {
  padding: 12px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border-secondary);
}
@media (max-width: 640px) {
  .libbar {
    grid-template-columns: minmax(0, 1fr);
  }
  .stats {
    gap: 14px;
  }
}
</style>
