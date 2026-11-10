<script setup lang="ts">
/**
 * 网盘文件：直接读你登录的那个 115 账号，不是台账、不是示例数据。
 *
 * 三条与后端对齐的事实：
 *  1. 目录是**懒加载**的。列一页就是一次真实请求，115 对频率敏感，
 *     所以只在展开/进入时拉，绝不一进页面就把整棵树扫一遍。
 *  2. 每个文件会带一个「台账里对应的 .strm」标记。网盘里有、媒体库里没有，
 *     和网盘里有、媒体库里也有，是两种完全不同的状态，页面必须分开显示。
 *  3. 移动/删除只动网盘。本地 .strm 由下一轮同步跟上，两条链路各管各的。
 */
import { computed, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import DirTreeNode from '@/components/DirTreeNode.vue'
import HEmpty from '@/components/HEmpty.vue'
import HField from '@/components/HField.vue'
import HModal from '@/components/HModal.vue'
import HPanel from '@/components/HPanel.vue'
import { api } from '@/api/endpoints'
import type { DirNode } from '@/data/tree'
import { toast } from '@/stores/toast'
import { fmtSize } from '@/utils/format'

interface Acct {
  id: number
  kind: string
  name: string
}
interface Lib {
  id: number
  account_id: number
  name: string
  remote_path: string
  local_path: string
}
interface FileRow {
  id: string
  name: string
  is_dir: boolean
  size: number
  mtime: string
  strm?: string
  library?: string
}

const accounts = ref<Acct[]>([])
const accountId = ref(0)
const libs = ref<Lib[]>([])

const roots = ref<DirNode[]>([])
const rows = ref<FileRow[]>([])
const crumbs = ref<{ id: string; name: string }[]>([{ id: '0', name: '根目录' }])
const current = ref('0')
const loading = ref(false)
const err = ref('')

const keyword = ref('')
const onlySynced = ref(false)
const selected = ref<string[]>([])

/** 115 和夸克在同一个页面里浏览：标题、可做的操作都跟着当前账号走。 */
const currentAcct = computed(() => accounts.value.find((a) => a.id === accountId.value))
const panLabel = computed(() =>
  currentAcct.value?.kind === 'quarktv' ? '夸克网盘' : '115 网盘',
)
/** 夸克TV 开放接口只给 list / download，移动和删除会返回「不支持」。 */
const canManage = computed(() => currentAcct.value?.kind !== 'quarktv')

const myLibs = computed(() => libs.value.filter((l) => l.account_id === accountId.value))
const syncedCount = computed(() => rows.value.filter((r) => r.strm).length)
const filtered = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return rows.value.filter((r) => {
    if (onlySynced.value && !r.strm) return false
    if (kw && !r.name.toLowerCase().includes(kw)) return false
    return true
  })
})
const allChecked = computed(
  () => filtered.value.length > 0 && filtered.value.every((r) => selected.value.includes(r.id)),
)

/* ---------------- 格式化 ---------------- */
function fmtTime(s: string) {
  if (!s) return '—'
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/* ---------------- 目录树 ---------------- */
async function loadKids(node: DirNode) {
  if (node.loading) return
  node.loading = true
  try {
    const r = await api.files.browse(accountId.value, node.id, { dirsOnly: true })
    node.kids = (r.entries as FileRow[]).map((e) => ({
      id: e.id,
      name: e.name,
      trail: [...node.trail, { id: e.id, name: e.name }],
    }))
    node.loaded = true
  } catch (e: any) {
    toast.error('读取目录失败', e?.message)
  } finally {
    node.loading = false
  }
}

async function toggle(node: DirNode) {
  if (!node.loaded) await loadKids(node)
  node.open = !node.open
}

async function initTree() {
  roots.value = [
    {
      id: '0',
      name: '根目录',
      trail: [{ id: '0', name: '根目录' }],
      open: true,
      loaded: false,
    },
  ]
  // 必须用数组里的那个代理对象去加载：直接改外面那个原始对象，
  // Vue 收不到通知，树会一直停在「根目录」一行。
  await loadKids(roots.value[0])
}

/* ---------------- 列表 ---------------- */
async function loadEntries() {
  if (!accountId.value) return
  loading.value = true
  err.value = ''
  try {
    const r = await api.files.browse(accountId.value, current.value)
    rows.value = (r.entries as FileRow[]) ?? []
  } catch (e: any) {
    err.value = e?.message || '读取失败'
    rows.value = []
  } finally {
    loading.value = false
  }
}

async function open(node: DirNode) {
  current.value = node.id
  crumbs.value = node.trail
  selected.value = []
  keyword.value = ''
  if (!node.loaded) void loadKids(node)
  await loadEntries()
}

/** 从列表里进入子目录：拼一条 trail，再顺手让左侧树跟上。 */
async function enter(row: FileRow) {
  const trail = [...crumbs.value, { id: row.id, name: row.name }]
  current.value = row.id
  crumbs.value = trail
  selected.value = []
  keyword.value = ''
  void reveal(trail)
  await loadEntries()
}

/** 让左侧树展开到指定路径（树没加载到的层级就停在能到的地方）。 */
async function reveal(trail: { id: string; name: string }[]) {
  let list = roots.value
  for (let i = 1; i < trail.length; i++) {
    const n = list.find((x) => x.id === trail[i].id)
    if (!n) return
    n.open = true
    if (!n.loaded) await loadKids(n)
    list = n.kids ?? []
  }
}

async function jump(index: number) {
  current.value = crumbs.value[index].id
  crumbs.value = crumbs.value.slice(0, index + 1)
  selected.value = []
  await loadEntries()
}

async function switchAccount() {
  current.value = '0'
  crumbs.value = [{ id: '0', name: '根目录' }]
  rows.value = []
  selected.value = []
  await initTree()
  await loadEntries()
}

/* ---------------- 选择 ---------------- */
function toggleSelect(id: string) {
  selected.value = selected.value.includes(id)
    ? selected.value.filter((x) => x !== id)
    : [...selected.value, id]
}
function checkAll() {
  selected.value = allChecked.value ? [] : filtered.value.map((r) => r.id)
}
const selectedDirs = computed(() => rows.value.filter((r) => selected.value.includes(r.id) && r.is_dir))

/* ---------------- 移动 ---------------- */
const moveOpen = ref(false)
const moveRoots = ref<DirNode[]>([])
const moveTarget = ref<DirNode | null>(null)
const moving = ref(false)

async function openMove() {
  if (!selected.value.length) return
  moveOpen.value = true
  moveTarget.value = null
  moveRoots.value = [
    {
      id: '0',
      name: '根目录',
      trail: [{ id: '0', name: '根目录' }],
      open: true,
      loaded: false,
    },
  ]
  await loadKids(moveRoots.value[0])
}

async function doMove() {
  if (!moveTarget.value) {
    toast.warning('先选目标目录')
    return
  }
  if (selected.value.includes(moveTarget.value.id)) {
    toast.warning('不能移动到选中的目录里')
    return
  }
  moving.value = true
  try {
    const r = await api.files.move({
      account_id: accountId.value,
      ids: selected.value,
      to_cid: moveTarget.value.id,
    })
    toast.success(`已移动 ${r.moved} 项`, '网盘已生效；本地 STRM 会在下一轮同步里跟上')
    moveOpen.value = false
    selected.value = []
    await loadEntries()
  } catch (e: any) {
    toast.error('移动失败', e?.message)
  } finally {
    moving.value = false
  }
}

/* ---------------- 删除 ---------------- */
const delOpen = ref(false)
const deleting = ref(false)

async function doDelete() {
  deleting.value = true
  try {
    const r = await api.files.remove({ account_id: accountId.value, ids: selected.value })
    toast.warning(`已删除 ${r.removed} 项`, r.hint)
    delOpen.value = false
    selected.value = []
    await loadEntries()
  } catch (e: any) {
    toast.error('删除失败', e?.message)
  } finally {
    deleting.value = false
  }
}

/* ---------------- 加入媒体库 ---------------- */
const addOpen = ref(false)
const addLib = ref(0)
const addScan = ref<any>(null)
const addBusy = ref(false)

function remotePath() {
  const parts = crumbs.value.slice(1).map((c) => c.name)
  return parts.length ? '/' + parts.join('/') : '/'
}

async function openAdd() {
  if (!myLibs.value.length) {
    toast.warning('这个账号下还没有媒体库', '先到「账号与媒体库」建一个')
    return
  }
  addLib.value = myLibs.value[0].id
  addScan.value = null
  addOpen.value = true
  await runScan()
}

async function runScan() {
  if (!addLib.value) return
  addBusy.value = true
  addScan.value = null
  try {
    // 落盘目录由服务端从 115 的 ParentID 往上走算出来，不靠面包屑拼，
    // 否则同名目录（电影/电影/…）会被拼成两层。
    const rel = await api.libraries.relDir(addLib.value, current.value)
    addScan.value = await api.catalog.scan({
      library_id: addLib.value,
      cid: current.value,
      rel_dir: rel.rel_dir || '',
    })
  } catch (e: any) {
    toast.error('扫描失败', e?.message)
  } finally {
    addBusy.value = false
  }
}

async function doAdd() {
  if (!addLib.value) return
  addBusy.value = true
  try {
    const r = await api.catalog.add({
      library_id: addLib.value,
      cid: current.value,
      rel_dir: addScan.value?.rel_dir || '',
    })
    toast.success(`已加入 ${r.added} 个片目`, r.note)
    addOpen.value = false
    await loadEntries()
  } catch (e: any) {
    toast.error('加入失败', e?.message)
  } finally {
    addBusy.value = false
  }
}

/* ---------------- 初始化 ---------------- */
onMounted(async () => {
  try {
    accounts.value = (await api.storage.accounts()) ?? []
  } catch (e: any) {
    err.value = e?.message || '读取账号失败'
  }
  try {
    libs.value = (await api.libraries.list()) ?? []
  } catch {
    /* 库读不到不影响浏览网盘 */
  }
  if (!accounts.value.length) return
  accountId.value = accounts.value[0].id
  await initTree()
  await loadEntries()
})
</script>

<template>
  <div class="files">
    <!-- 目录树 -->
    <HPanel
      :title="panLabel"
      desc="115 / 夸克都在这里浏览，点目录切换右侧列表"
      flush
      class="tree-panel"
    >
      <template v-if="accounts.length > 1" #actions>
        <select v-model.number="accountId" class="input input-sm" @change="switchAccount">
          <option v-for="a in accounts" :key="a.id" :value="a.id">
            {{ a.kind === 'quarktv' ? '夸克TV' : '115' }} · {{ a.name }}
          </option>
        </select>
      </template>
      <div class="tree">
        <DirTreeNode
          v-for="n in roots"
          :key="n.id"
          :node="n"
          :current="current"
          @pick="open"
          @toggle="toggle"
        />
        <p v-if="!accounts.length" class="muted small pad">
          还没有登录网盘账号。先到「账号与媒体库」登录 115 或夸克TV，这里才有东西可看。
        </p>
      </div>
    </HPanel>

    <!-- 文件列表 -->
    <HPanel flush>
      <template #actions>
        <div class="row" style="gap: 6px">
          <button class="btn btn-sm btn-tertiary" :disabled="loading" @click="loadEntries">
            <AppIcon name="refresh" :size="14" />刷新
          </button>
          <button class="btn btn-sm btn-primary" :disabled="!accountId" @click="openAdd">
            <AppIcon name="plus" :size="14" />当前目录加入媒体库
          </button>
        </div>
      </template>

      <div class="crumb">
        <template v-for="(c, i) in crumbs" :key="c.id">
          <AppIcon v-if="i" name="chevronRight" :size="13" />
          <button class="cbtn" :class="{ 'is-last': i === crumbs.length - 1 }" @click="jump(i)">
            {{ c.name }}
          </button>
        </template>
        <span class="muted small" style="margin-left: 8px">
          {{ rows.length }} 项 · 已入库 {{ syncedCount }}
        </span>
      </div>

      <div class="toolbar">
        <input v-model="keyword" class="input" placeholder="筛选名称…" style="max-width: 260px" />
        <label class="checkline"><input v-model="onlySynced" type="checkbox" />只看已入库</label>
        <span v-if="err" class="err small">{{ err }}</span>
      </div>

      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th style="width: 36px">
                <input type="checkbox" :checked="allChecked" @change="checkAll" />
              </th>
              <th>名称</th>
              <th style="width: 96px">类型</th>
              <th class="num" style="width: 96px">大小</th>
              <th style="width: 140px">修改时间</th>
              <th style="width: 170px">媒体库</th>
              <th style="width: 84px" />
            </tr>
          </thead>
          <tbody>
            <tr v-for="f in filtered" :key="f.id" :class="{ 'is-picked': selected.includes(f.id) }">
              <td>
                <input type="checkbox" :checked="selected.includes(f.id)" @change="toggleSelect(f.id)" />
              </td>
              <td class="mono small">
                <button v-if="f.is_dir" class="linkbtn" @click="enter(f)">{{ f.name }}</button>
                <span v-else>{{ f.name }}</span>
              </td>
              <td class="muted small">{{ f.is_dir ? '目录' : '视频/文件' }}</td>
              <td class="num mono small">{{ f.is_dir ? '—' : fmtSize(f.size) }}</td>
              <td class="muted small">{{ fmtTime(f.mtime) }}</td>
              <td>
                <span v-if="f.strm" class="badge badge-success" :title="f.strm">
                  已入库 · {{ f.library }}
                </span>
                <span v-else-if="!f.is_dir" class="badge">未入库</span>
                <span v-else class="muted small">—</span>
              </td>
              <td>
                <button v-if="f.is_dir" class="btn btn-sm btn-ghost" @click="enter(f)">
                  <AppIcon name="chevronRight" :size="14" />
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <HEmpty
        v-if="!loading && !filtered.length && !err"
        :title="keyword || onlySynced ? '没有符合条件的文件' : '这个目录是空的'"
        desc="换一个目录，或者清掉筛选条件"
      />

      <div class="batch">
        <span class="muted small">已选 {{ selected.length }} 项</span>
        <div class="row" style="gap: 6px">
          <button
            class="btn btn-sm btn-tertiary"
            :disabled="!selected.length || !canManage"
            :title="canManage ? '' : '夸克TV 开放接口不支持移动，请到夸克客户端操作'"
            @click="openMove"
          >
            <AppIcon name="folder" :size="14" />移动到…
          </button>
          <button
            class="btn btn-sm btn-ghost"
            :disabled="!selected.length || !canManage"
            :title="canManage ? '' : '夸克TV 开放接口不支持删除，请到夸克客户端操作'"
            style="color: var(--danger)"
            @click="delOpen = true"
          >
            <AppIcon name="trash" :size="14" />删除
          </button>
        </div>
      </div>
    </HPanel>

    <!-- 移动 -->
    <HModal v-model="moveOpen" title="移动到" size="md">
      <template #desc>选一个目标目录；只动网盘，本地 STRM 下一轮同步自动跟上。</template>
      <div class="picktree">
        <DirTreeNode
          v-for="n in moveRoots"
          :key="n.id"
          :node="n"
          :current="moveTarget?.id ?? ''"
          @pick="moveTarget = $event"
          @toggle="toggle"
        />
      </div>
      <p class="muted small" style="margin-top: 10px">
        目标：<strong>{{ moveTarget?.name ?? '未选择' }}</strong> · 共 {{ selected.length }} 项
        <span v-if="selectedDirs.length" class="muted">（含 {{ selectedDirs.length }} 个目录）</span>
      </p>
      <template #footer>
        <button class="btn btn-tertiary" @click="moveOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="moving || !moveTarget" @click="doMove">
          {{ moving ? '移动中…' : '确认移动' }}
        </button>
      </template>
    </HModal>

    <!-- 删除 -->
    <HModal v-model="delOpen" title="删除网盘文件" size="md">
      <template #desc>会调用网盘的删除接口，文件进回收站（可以恢复）。</template>
      <div class="danger-box">
        <AppIcon name="alert" :size="18" />
        <div>
          <strong>将删除 {{ selected.length }} 项</strong>
          <p class="muted small">
            本地 .strm / NFO 不会跟着删：清理本地残留请到「Strm 管理 → 孤儿清理」。
            删除后想更新媒体库，跑一轮勾选了「删除失效」的增量同步即可。
          </p>
        </div>
      </div>
      <template #footer>
        <button class="btn btn-tertiary" @click="delOpen = false">取消</button>
        <button class="btn btn-danger" :disabled="deleting" @click="doDelete">
          {{ deleting ? '删除中…' : '确认删除' }}
        </button>
      </template>
    </HModal>

    <!-- 加入媒体库 -->
    <HModal v-model="addOpen" title="加入媒体库" size="md">
      <template #desc>扫描当前目录里的视频，写 .strm 并登记台账，随后自动刮削。</template>
      <HField label="目标媒体库">
        <select v-model.number="addLib" class="input" @change="runScan">
          <option v-for="l in myLibs" :key="l.id" :value="l.id">
            {{ l.name }}（{{ l.local_path }}）
          </option>
        </select>
      </HField>
      <p class="muted small" style="margin-top: 8px">
        目录：<span class="mono">{{ remotePath() }}</span>
      </p>

      <div v-if="addBusy" class="muted small" style="margin-top: 12px">
        扫描中…（目录大时可能要十几秒）
      </div>
      <div v-else-if="addScan" class="scanbox">
        <div class="row-between small">
          <span>识别到 <strong>{{ addScan.videos }}</strong> 个视频</span>
          <span class="mono">新增 {{ addScan.will_add }} · 已存在 {{ addScan.will_skip }}</span>
        </div>
        <p class="muted small" style="margin-top: 6px">
          落到本地目录：<span class="mono">{{ addScan.rel_dir || '库根目录' }}</span>
        </p>
      </div>

      <template #footer>
        <button class="btn btn-tertiary" @click="addOpen = false">取消</button>
        <button
          class="btn btn-primary"
          :disabled="addBusy || !addScan || !addScan.will_add"
          @click="doAdd"
        >
          加入 {{ addScan?.will_add ?? 0 }} 个
        </button>
      </template>
    </HModal>
  </div>
</template>

<style scoped>
.files {
  display: grid;
  grid-template-columns: minmax(220px, 268px) minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}
.tree-panel {
  position: sticky;
  top: calc(var(--topbar-h) + 16px);
}
.tree {
  padding: 8px;
  max-height: calc(100dvh - 180px);
  overflow: auto;
}
.tree .pad {
  padding: 10px 12px;
  line-height: 1.7;
}
.crumb {
  display: flex;
  align-items: center;
  gap: 3px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--separator);
  flex-wrap: wrap;
}
.cbtn {
  border: 0;
  background: transparent;
  color: var(--muted);
  font-size: 12.5px;
  padding: 2px 4px;
  border-radius: 6px;
}
.cbtn:hover {
  background: var(--surface-secondary);
  color: var(--foreground);
}
.cbtn.is-last {
  color: var(--foreground);
  font-weight: 600;
}
.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 16px;
  border-bottom: 1px solid var(--separator);
}
.checkline {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12.5px;
  color: var(--muted);
  white-space: nowrap;
}
.checkline input {
  accent-color: var(--accent);
}
.linkbtn {
  border: 0;
  background: transparent;
  color: var(--accent);
  font: inherit;
  padding: 0;
  text-align: left;
}
.linkbtn:hover {
  text-decoration: underline;
}
tr.is-picked {
  background: var(--accent-soft);
}
.batch {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 16px;
  border-top: 1px solid var(--separator);
  background: color-mix(in oklab, var(--surface-secondary) 60%, transparent);
}
.picktree {
  max-height: 300px;
  overflow: auto;
  padding: 6px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
}
.scanbox {
  margin-top: 12px;
  padding: 12px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
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
  .files {
    grid-template-columns: minmax(0, 1fr);
  }
  .tree-panel {
    position: static;
  }
}
</style>
