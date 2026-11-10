<script setup lang="ts">
/**
 * 网盘目录选择器（115 / 夸克TV 通用）。
 *
 * 逻辑和「账号与媒体库」里建库时那个选择器一样：逐级 browse，面包屑可回退。
 * 做成组件是因为资源搜索的转存目录、夸克的分享转存落点都要用，
 * 而复用账号页那份会把几页耦在一起。
 * 两种网盘的根目录 cid 都是 "0"，区别只在默认找哪个账号和文案。
 */
import { computed, ref, watch } from 'vue'
import AppIcon from './AppIcon.vue'
import HModal from './HModal.vue'
import { api } from '@/api/endpoints'

interface DirEntry {
  id: string
  name: string
  is_dir: boolean
}

const props = withDefaults(
  defineProps<{
    modelValue: boolean
    accountId?: number
    /** 不指定 accountId 时按这个类型找第一个账号 */
    accountKind?: '115' | 'quarktv'
    title?: string
    desc?: string
  }>(),
  {
    title: '选择转存目录',
    desc: '转存进来的文件会落在这个目录里',
    accountKind: '115',
  },
)
const emit = defineEmits<{
  'update:modelValue': [boolean]
  pick: [v: { cid: string; path: string; name: string }]
}>()

const crumbs = ref<{ cid: string; name: string }[]>([])
const entries = ref<DirEntry[]>([])
const loading = ref(false)
const err = ref('')
const accId = ref(0)

const isQuark = computed(() => props.accountKind === 'quarktv')
const rootName = computed(() => (isQuark.value ? '夸克 根目录' : '115 根目录'))
const noAccountHint = computed(() =>
  isQuark.value
    ? '还没有夸克TV 账号，先去「账号与媒体库」登录'
    : '还没有 115 账号，先去「账号与媒体库」登录',
)

watch(
  () => props.modelValue,
  (open) => {
    if (open) void open_()
  },
)

/** 没指定账号时，按 accountKind 找第一个匹配的账号 */
async function resolveAccount(): Promise<number> {
  if (props.accountId) return props.accountId
  try {
    const list: any = await api.storage.accounts()
    const want = props.accountKind
    const hit = (list ?? []).find((a: any) => String(a.kind || '').toLowerCase() === want)
    return hit?.id ?? 0
  } catch {
    return 0
  }
}

async function open_() {
  accId.value = await resolveAccount()
  crumbs.value = []
  if (!accId.value) {
    err.value = noAccountHint.value
    entries.value = []
    return
  }
  await load('0')
}

async function load(cid: string) {
  loading.value = true
  err.value = ''
  try {
    const r: any = await api.storage.browse(accId.value, cid)
    entries.value = (r?.entries ?? []).filter((x: DirEntry) => x.is_dir)
  } catch (e: any) {
    entries.value = []
    err.value = e?.message || '读取目录失败'
  } finally {
    loading.value = false
  }
}

async function enter(d: DirEntry) {
  crumbs.value = [...crumbs.value, { cid: d.id, name: d.name }]
  await load(d.id)
}

async function jumpTo(i: number) {
  crumbs.value = crumbs.value.slice(0, i + 1)
  await load(crumbs.value[i].cid)
}

async function jumpRoot() {
  crumbs.value = []
  await load('0')
}

function pickRoot() {
  emit('pick', { cid: '0', path: rootName.value, name: '根目录' })
  emit('update:modelValue', false)
}

function pickCurrent() {
  const cur = crumbs.value[crumbs.value.length - 1]
  if (!cur) return
  emit('pick', { cid: cur.cid, path: '/' + crumbs.value.map((c) => c.name).join('/'), name: cur.name })
  emit('update:modelValue', false)
}
</script>

<template>
  <HModal
    :model-value="modelValue"
    :title="title"
    size="md"
    @update:model-value="(v: boolean) => emit('update:modelValue', v)"
  >
    <template #desc>{{ desc }}</template>

    <div class="row wrap small dir-crumbs">
      <button class="crumb" @click="jumpRoot">根目录</button>
      <template v-for="(p, i) in crumbs" :key="p.cid">
        <span class="muted">/</span>
        <button class="crumb" @click="jumpTo(i)">{{ p.name }}</button>
      </template>
    </div>

    <div class="dir-list">
      <div v-if="loading" class="muted small" style="padding: 10px">读取中…</div>
      <div v-else-if="err" class="small text-danger" style="padding: 10px">{{ err }}</div>
      <template v-else>
        <button v-for="d in entries" :key="d.id" class="dir-row" @click="enter(d)">
          <AppIcon name="folder" :size="15" />
          <span class="truncate">{{ d.name }}</span>
          <AppIcon name="play" :size="12" class="muted" />
        </button>
        <p v-if="!entries.length" class="muted small" style="padding: 10px">这个目录下没有子目录</p>
      </template>
    </div>

    <template #footer>
      <button class="btn btn-tertiary" @click="pickRoot">用根目录</button>
      <button class="btn btn-primary" :disabled="!crumbs.length" @click="pickCurrent">用当前目录</button>
    </template>
  </HModal>
</template>

<style scoped>
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
</style>
