<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '@/components/AppIcon.vue'
import HField from '@/components/HField.vue'
import HPanel from '@/components/HPanel.vue'
import HTabs from '@/components/HTabs.vue'
import HSwitch from '@/components/HSwitch.vue'
import HEmpty from '@/components/HEmpty.vue'
import WallBrowser from '@/components/WallBrowser.vue'
import PlatformWall from '@/components/PlatformWall.vue'
import MyCalendar from '@/components/MyCalendar.vue'
import TitlePicker from '@/components/TitlePicker.vue'
import type { TitleHit } from '@/components/TitlePicker.vue'
import { api } from '@/api/endpoints'
import { rememberOrigin } from '@/stores/navMemory'
import { toast } from '@/stores/toast'
import TitleLink from '@/components/TitleLink.vue'
import { forgetSub, subsVersion } from '@/stores/subscriptions'
import { openDetail } from '@/stores/detail'
import { pad2 } from '@/utils/format'
import SubLineSlider from '@/components/SubLineSlider.vue'

/* 标签页现在承载「找资源 / 订阅 / 我的追更 / 链接转存 / 资源搜索 / 订阅设置」。
   日历原本是独立的一页，两边都在看同一批片子却各刷各的（日历里订完回订阅页
   还看不到），合并之后共用一份订阅索引，切换标签不用重新认片。
   初始值认 ?tab=：顶栏搜索、外部链接都靠它落到指定标签。 */
const route = useRoute()
const router = useRouter()
/** 老书签 /media-transfer?tab=calendar 落到新的「我的追更」，别停在一个已不存在的标签上 */
function normalizeTab(v: unknown): string {
  const s = typeof v === 'string' && v ? v : 'search'
  return s === 'calendar' ? 'mycalendar' : s
}
const tab = ref(normalizeTab(route.query.tab))
// 「找资源」里的认片输入框：从详情页返回时会用它把上一次的词重新搜一遍。
const finderRef = ref<{ setQuery: (q: string) => void } | null>(null)
/** 「查看全部」聚焦到某个平台时，主海报墙让位给带筛选/分页的完整榜单。 */
const focusProvider = ref<{ code: string; name: string; type: 'movie' | 'series' } | null>(null)
// 路由里的 ?tab= 变了也要切过去：路由变化不一定重新挂载组件，
// 只靠初始值会停在原来的标签上。
watch(
  () => route.query.tab,
  (v) => {
    if (typeof v !== 'string' || !v) return
    const next = normalizeTab(v)
    if (next !== tab.value) tab.value = next
  },
)

// 「资源搜索」已经是独立页面了，标签还留着是为了不让老习惯失效：
// 点到它就跳过去，并把 ?q= 一起带上。
watch(tab, (v) => {
  if (v !== 'res') return
  const q = String(route.query.q || '').trim()
  const en = String(route.query.en || '').trim()
  tab.value = 'search'
  void router.push({ path: '/search', query: en ? { q, en } : q ? { q } : {} })
})

/* ---------------- 顶部搜索：先认片，再搜资源 ---------------- */

/**
 * 「发现与订阅」顶部这个搜索框是整页的入口：输入片名先出 TMDB + 豆瓣候选，
 * 点准了才去搜资源。以前资源搜索藏在第四个标签里，用户得先想到「去搜资源」；
 * 现在入口在顶上，选中候选后自动切到资源标签并开搜，不用再敲一遍片名。
 *
 * 传 runId 而不是直接调子组件的 search()：子组件可能还没挂载（标签没渲染
 * 过），watch 一个自增的数字最稳，重复选同一部也能重新搜。
 */
function searchResources(q: string, en = '') {
  const v = q.trim()
  if (!v) return
  // 资源搜索已经是独立页面：这里只负责把关键词带过去，结果留在那一页
  // （它被 KeepAlive 保活，点进详情返回后还在）。
  void router.push({ path: '/search', query: en ? { q: v, en } : { q: v } })
}

/**
 * 认片之后先进「影视详情页」，不再一点就跳到资源搜索。
 * 详情页里看完海报、简介、季集和本地入库状态，再由用户决定要不要找资源。
 * 豆瓣条目没有 TMDB id，只带片名 + 年份，让详情页按名字问 TMDB。
 */
function onFinderPick(hit: TitleHit) {
  const q = hit.title || hit.original
  // 记下这一站：详情页返回时回到「找资源」，并把这个词带回去 ——
  // 页面会重新挂载，候选列表靠 q 自动重搜一遍，用户看到的还是刚才那批结果。
  const back = new URLSearchParams({ tab: 'search', q })
  if (hit.original) back.set('en', hit.original)
  rememberOrigin(`/media-transfer?${back.toString()}`, '返回搜索结果')
  router.push({
    name: 'title',
    query: {
      type: hit.kind === 'tv' ? 'tv' : 'movie',
      tmdb_id: hit.source === 'tmdb' ? String(hit.id || '') : '',
      q,
      year: hit.year || '',
      // 详情页「找资源」按钮会把它带回来，避免国产片被拿去搜英文演员名。
      en: hit.source === 'tmdb' ? hit.original || '' : '',
    },
  })
}

function onFinderSkip(q: string) {
  searchResources(q)
}

/**
 * 日历里国产剧卡片上的「找资源」：直接切到资源搜索标签开搜。
 * 走同一个 searchResources，和顶部选择器选完片之后的行为完全一致。
 */
function onCalendarFind(q: string, en?: string) {
  searchResources(q, en || '')
}

/* ---------------- 订阅 ---------------- */

/**
 * 后端 /api/subscriptions 的一条订阅。
 *
 * watermark_* 是「已经提交到第几集」，不是「已经入库到第几集」——
 * 提交离线到真正落盘之间有几小时，页面上要显示的是「我帮你交到哪了」。
 */
interface SubRow {
  id: number
  tmdb_id: number
  media_type: 'tv' | 'movie'
  title: string
  original_title?: string
  year: number
  quality: string
  state: 'watching' | 'waiting' | 'done' | 'paused'
  watermark_season: number
  watermark_episode: number
  total_episodes: number
  last_result: string
  next_check_at: string
  fail_count: number
}

const subs = ref<SubRow[]>([])
const subCounts = ref<Record<string, number>>({})
const builtinTMDB = ref(false)
const subsLoading = ref(false)

const tabs = computed(() => [
  { value: 'search', label: '找资源' },
  { value: 'mycalendar', label: '我的追更' },
  { value: 'subs', label: '订阅', count: subs.value.length },
  { value: 'link', label: '链接转存' },
  { value: 'res', label: '资源搜索' },
  { value: 'subset', label: '订阅设置' },
])

const subFilter = ref('all')
const subFilters = [
  { value: 'all', label: '全部' },
  { value: 'watching', label: '追更中' },
  { value: 'waiting', label: '等待新集' },
  { value: 'done', label: '已入库' },
  { value: 'paused', label: '已暂停' },
]
const subStateText: Record<string, string> = {
  watching: '追更中',
  waiting: '等待新集',
  done: '已入库',
  paused: '已暂停',
}
const subStateTone: Record<string, string> = {
  watching: 'badge-accent',
  waiting: 'badge-warning',
  done: 'badge-success',
  paused: 'badge',
}
const subRows = computed(() =>
  subFilter.value === 'all' ? subs.value : subs.value.filter((s) => s.state === subFilter.value),
)

async function loadSubs() {
  subsLoading.value = true
  try {
    const r: any = await api.subscriptions.list()
    subs.value = r.items ?? []
    subCounts.value = r.counts ?? {}
    builtinTMDB.value = !!r.builtin_tmdb
    if (r.interval_minutes) subForm.value.interval = r.interval_minutes
  } catch (e: any) {
    toast.warning('订阅列表没拉到', e?.message || '')
  } finally {
    subsLoading.value = false
  }
}

// 「立即检查」要等后端跑完 TMDB 查询、搜索和离线提交，所以按钮锁到这里为止。
const runningId = ref(0)

async function runSub(s: SubRow) {
  if (runningId.value) return
  runningId.value = s.id
  try {
    const r: any = await api.subscriptions.run(s.id)
    toast.success(`${s.title}：${r.result || '检查完成'}`)
  } catch (e: any) {
    toast.warning('检查失败', e?.message || '')
  } finally {
    runningId.value = 0
    // 失败也要刷：后端会把原因写进 last_result，用户得能看到。
    loadSubs()
  }
}

async function togglePause(s: SubRow) {
  const next = s.state === 'paused' ? 'watching' : 'paused'
  try {
    await api.subscriptions.update(s.id, { state: next })
    s.state = next
    toast.success(next === 'paused' ? `已暂停「${s.title}」` : `已恢复「${s.title}」`)
  } catch (e: any) {
    toast.warning('操作失败', e?.message || '')
  }
}

async function removeSub(s: SubRow) {
  try {
    await api.subscriptions.remove(s.id)
    subs.value = subs.value.filter((x) => x.id !== s.id)
    // 墙上/日历里的按钮共用同一份索引，这里删掉要顺手抹掉，否则它们还显示「已订阅」。
    // 本地已经把这行 filter 掉了，不用再为此重拉一次列表。
    skipSelfBump++
    forgetSub(s.media_type, s.tmdb_id)
    toast.success(`已取消订阅「${s.title}」`, '已经交出去的离线任务不受影响')
  } catch (e: any) {
    toast.warning('删除失败', e?.message || '')
  }
}

/** 进度文案。电影没有集数，别硬套 S01E01。 */
function subProgress(s: SubRow): string {
  if (s.media_type === 'movie') return s.state === 'done' ? '已入库' : '等待资源'
  const span = s.total_episodes > 0 ? ` / ${s.total_episodes} 集` : ''
  if (s.watermark_episode > 0) {
    return `S${pad2(s.watermark_season)}E${pad2(s.watermark_episode)}${span}`
  }
  return s.total_episodes > 0 ? `0 / ${s.total_episodes} 集` : '还没开始'
}

/** 时间：过去的显示「多久以前」，未来的显示「还要等多久」——这两件事用户都关心。 */
function subTime(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const diff = Date.now() - d.getTime()
  const mins = Math.round(Math.abs(diff) / 60000)
  const suffix = diff >= 0 ? '前' : '后'
  if (mins < 1) return '刚刚'
  if (mins < 60) return `${mins} 分钟${suffix}`
  if (mins < 1440) return `${Math.round(mins / 60)} 小时${suffix}`
  return `${Math.round(mins / 1440)} 天${suffix}`
}

function fmtInterval(min: number): string {
  if (min % 1440 === 0) return `${min / 1440} 天`
  if (min % 60 === 0) return `${min / 60} 小时`
  return `${min} 分钟`
}

const links = ref('')
const target = ref('电影')
const transferring = ref(false)
const afterTransfer = ref({ organize: true, nfo: true, wash: false })

function doTransfer() {
  const list = links.value.split('\n').map((s) => s.trim()).filter(Boolean)
  if (!list.length) {
    toast.warning('先粘贴至少一条分享链接')
    return
  }
  transferring.value = true
  setTimeout(() => {
    transferring.value = false
    toast.success(`已提交 ${list.length} 条转存`, `目标库：${target.value}`)
    links.value = ''
  }, 700)
}

/* ---------------- 起播线（水位线与限制线已合并） ---------------- */
/**
 * 每条剧集订阅都给一根滑杆，和影视详情用的是同一个组件：
 * 拖到哪一集，就从这一集开始找（含这一集）；往回拖会先问一次。
 *
 * 电影没有集数，起播线对它没意义，这一块只列剧集。
 */
const seriesSubs = computed(() => subs.value.filter((s) => s.media_type !== 'movie'))

/** 「订阅」表格里的滑杆入口：点开影视详情抽屉，那里有同一根滑杆。 */
function openLineDetail(s: SubRow) {
  openDetail({
    type: s.media_type === 'movie' ? 'movie' : 'tv',
    tmdbId: s.tmdb_id,
    title: s.title,
    year: s.year,
  })
}

/** 保存成功后立刻回填表格，别等下一轮列表刷新 —— 表格里的「已交到」也要跟着变。 */
function onLineSaved(s: SubRow, e: { season: number; episode: number }) {
  s.watermark_season = e.season
  s.watermark_episode = e.episode
}

/**
 * 切到「订阅」页签就重拉一次。
 *
 * 以前只在 onMounted 拉：在我的追更里订了一部剧、或者把某条取消了，
 * 切回这里看不到变化 —— 页面没重挂载，列表还是进页面时那一份。
 * 「订阅和我的追更里的订阅没有联动」说的就是这个。
 */
watch(tab, (t) => {
  if (t === 'subs') void loadSubs()
})

/**
 * 详情抽屉里订阅 / 取消订阅也会让版本号 +1，这里跟着重拉，
 * 否则刚在抽屉里取消掉的订阅还挂在表格里。
 *
 * skipSelfBump 用来吃掉「本页自己删除」那一次：那种情况上面已经 filter 过了，
 * 再拉一遍纯属多余。
 */
let skipSelfBump = 0
watch(subsVersion, () => {
  if (skipSelfBump > 0) {
    skipSelfBump--
    return
  }
  void loadSubs()
})

const subForm = ref({ interval: 360 })
const saving = ref(false)

async function saveSub() {
  saving.value = true
  try {
    const r: any = await api.subscriptions.saveConfig(subForm.value.interval)
    subForm.value.interval = r.interval_minutes
    toast.success('已保存', `每 ${fmtInterval(r.interval_minutes)} 检查一轮`)
  } catch (e: any) {
    toast.warning('保存失败', e?.message || '')
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  void loadSubs()
  // 老链接 /media-transfer?tab=res&q=... 直接转到独立搜索页，别停在一个
  // 已经不存在的标签上。
  const q = String(route.query.q || '').trim()
  if (route.query.tab === 'res') {
    void router.replace({ path: '/search', query: q ? { q, en: String(route.query.en || '') } : {} })
    return
  }
  // 从影视详情页「返回搜索结果」回来：把上一次认片的词重新搜一遍，
  // 用户看到的就是刚才那批候选，不用再敲一遍。
  if (q && tab.value === 'search') {
    void nextTick(() => finderRef.value?.setQuery(q))
  }
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <HTabs v-model="tab" :items="tabs" />

    <!-- 找资源 -->
    <template v-if="tab === 'search'">
      <div class="panel search-hero">
        <div class="col" style="gap: 8px">
          <h2 style="font-size: 18px">找片 → 订阅 → 自动转存整理</h2>
          <p class="muted small">
            先在上面认片：TMDB 和豆瓣同时给候选，点准的那部会打开影视详情页。
            下面按平台分行展示片单，点海报看详情，看完再决定要不要找资源。
          </p>
          <div class="row wrap" style="gap: 8px">
            <span class="badge badge-accent">TMDB 数据源</span>
            <span class="badge badge-success">豆瓣补国产剧与国内档期</span>
            <span class="badge">资源走 TG 频道 + 种子站</span>
          </div>
          <TitlePicker ref="finderRef" compact @pick="onFinderPick" @skip="onFinderSkip" />
        </div>
      </div>

      <WallBrowser
        v-if="focusProvider"
        :key="`${focusProvider.code}:${focusProvider.type}`"
        :initial-provider="focusProvider.code"
        :initial-type="focusProvider.type"
        show-back
        @back="focusProvider = null"
      />
      <PlatformWall v-else @focus="focusProvider = $event" />
    </template>

    <!-- 我的追更：只列用户已订阅剧集的每一集，按 TMDB 单集播出日排 -->
    <template v-else-if="tab === 'mycalendar'">
      <MyCalendar @find="onCalendarFind" />
    </template>

    <!-- 订阅 -->
    <template v-else-if="tab === 'subs'">
      <HPanel
        title="我的订阅"
        desc="每轮检查问 TMDB 这部剧该出到第几集，有新的就自动搜资源、提交离线"
        flush
      >
        <template #actions>
          <button class="btn btn-sm btn-tertiary" :disabled="subsLoading" @click="loadSubs">
            <AppIcon name="refresh" :size="14" />刷新
          </button>
          <button class="btn btn-sm btn-primary" @click="tab = 'search'">
            <AppIcon name="plus" :size="14" />去片单添加
          </button>
        </template>

        <div v-if="builtinTMDB" class="hint-line">
          还没在「系统配置」里填自己的 TMDB Key：后端会用 STRM115_TMDB_KEY 兜底，两者都没有时订阅查不了新集。
        </div>

        <div style="padding: 12px 16px 0">
          <div class="row wrap" style="gap: 8px">
            <button
              v-for="f in subFilters"
              :key="f.value"
              class="chip"
              :class="{ 'is-active': subFilter === f.value }"
              @click="subFilter = f.value"
            >
              {{ f.label }}
              <span v-if="f.value !== 'all' && subCounts[f.value]" class="muted">
                {{ subCounts[f.value] }}
              </span>
            </button>
          </div>
        </div>

        <div class="table-wrap" style="margin-top: 10px">
          <table class="table">
            <thead>
              <tr>
                <th>片名</th>
                <th>类型</th>
                <th>已交到</th>
                <th>画质</th>
                <th>状态</th>
                <th>最近一轮</th>
                <th style="width: 164px">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in subRows" :key="s.id">
                <td>
                  <!-- 片名可点：订阅表里只有 tmdb_id，正好是 /api/title 要的东西 -->
                  <TitleLink :type="s.media_type" :tmdb-id="s.tmdb_id" :title="s.title" :year="s.year">
                    {{ s.title }}
                  </TitleLink>
                  <span class="muted small"> ({{ s.year }})</span>
                </td>
                <td class="muted">{{ s.media_type === 'tv' ? '剧集' : '电影' }}</td>
                <td class="mono small">{{ subProgress(s) }}</td>
                <td class="small">{{ s.quality }}</td>
                <td>
                  <span class="badge" :class="subStateTone[s.state]">{{ subStateText[s.state] }}</span>
                  <span v-if="s.fail_count > 0" class="badge badge-danger" :title="`连续失败 ${s.fail_count} 次`">
                    失败 {{ s.fail_count }}
                  </span>
                </td>
                <td class="muted small">
                  <div v-if="s.last_result">{{ s.last_result }}</div>
                  <div v-else class="muted">还没检查过</div>
                  <div class="muted" style="font-size: 11px">下次 {{ subTime(s.next_check_at) }}</div>
                </td>
                <td>
                  <div class="row" style="gap: 2px">
                    <button
                      class="btn btn-sm btn-ghost"
                      :disabled="!!runningId"
                      :title="runningId === s.id ? '检查中…' : '立即检查'"
                      @click="runSub(s)"
                    >
                      <AppIcon name="play" :size="14" />
                    </button>
                    <button
                      v-if="s.media_type !== 'movie'"
                      class="btn btn-sm btn-ghost"
                      title="打开影视详情，用滑杆调起播线"
                      @click="openLineDetail(s)"
                    >
                      <AppIcon name="filter" :size="14" />
                    </button>
                    <button
                      class="btn btn-sm btn-ghost"
                      :title="s.state === 'paused' ? '继续追更' : '暂停'"
                      @click="togglePause(s)"
                    >
                      <AppIcon :name="s.state === 'paused' ? 'play' : 'power'" :size="14" />
                    </button>
                    <button class="btn btn-sm btn-ghost" title="取消订阅" style="color: var(--danger)" @click="removeSub(s)">
                      <AppIcon name="trash" :size="14" />
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <HEmpty
          v-if="!subRows.length"
          :title="subs.length ? '这个筛选下没有订阅' : '还没有订阅'"
          :desc="subs.length ? '' : '在「找资源」里点开一部剧，详情里有「订阅」按钮'"
        />
      </HPanel>
    </template>

    <!-- 链接转存 -->
    <template v-else-if="tab === 'link'">
      <div class="grid-2">
        <HPanel title="链接转存" desc="一行一条 115 分享链接，合集会自动拆成多部">
          <textarea
            v-model="links"
            class="textarea"
            style="min-height: 168px"
            placeholder="https://115.com/s/xxxx?password=abcd&#10;https://115.com/s/yyyy"
          />
          <div class="row-between" style="margin-top: 12px">
            <div class="row" style="gap: 8px">
              <span class="small muted">目标库</span>
              <select v-model="target" class="select-native" style="width: 140px">
                <option>电影</option>
                <option>剧集</option>
                <option>动漫</option>
              </select>
            </div>
            <button class="btn btn-primary" :disabled="transferring" @click="doTransfer">
              {{ transferring ? '提交中…' : '开始转存' }}
            </button>
          </div>
        </HPanel>

        <HPanel title="转存后的动作" desc="转存只负责把文件放进网盘，整理与刮削在这里决定">
          <div class="col" style="gap: 12px">
            <label class="checkline"><HSwitch v-model="afterTransfer.organize" />转存完立刻跑一轮整理</label>
            <label class="checkline"><HSwitch v-model="afterTransfer.nfo" />整理后写 NFO 与海报</label>
            <label class="checkline"><HSwitch v-model="afterTransfer.wash" />命中已存在片目时按洗版策略替换</label>
            <p class="muted small">
              转存是 115 风控敏感操作，默认串行、每条间隔 3 秒；失败会保留记录并在任务中心重试。
            </p>
          </div>
        </HPanel>
      </div>
    </template>

    <!-- 订阅设置 -->
    <template v-else>
      <div class="col" style="gap: 16px">
        <HPanel title="订阅设置" desc="只管「多久查一次」；画质和体积是每条订阅自己的条件">
          <HField
            label="检查周期"
            tip="追平的剧按这个间隔再问一次 TMDB"
            :hint="`当前每 ${fmtInterval(subForm.interval)} 检查一轮 · 范围 15 分钟 ~ 7 天`"
          >
            <div class="row" style="gap: 8px; align-items: center">
              <input v-model.number="subForm.interval" class="input" type="number" min="15" max="10080" style="max-width: 140px" />
              <span class="muted small">分钟</span>
            </div>
          </HField>

          <div class="col" style="gap: 8px; margin-top: 4px">
            <p class="muted small">
              有新集但还没搜到资源时，用的是 <strong>10 分钟</strong> 的短间隔重试，不按这里的周期。
            </p>
            <p class="muted small">
              下一集有确定的播出日期时，会等到<strong>播出日次日</strong>再查 —— 当天播完通常还拿不到资源。
            </p>
            <p class="muted small">
              订阅提交出去的离线任务和网页手动提交走同一条链路：下完自动整理、同步、刮削，并推通知。
            </p>
          </div>

          <template #footer>
            <button class="btn btn-primary" :disabled="saving" @click="saveSub">
              {{ saving ? '保存中…' : '保存设置' }}
            </button>
          </template>
        </HPanel>

        <!-- 起播线：只找这条线之后的资源；水位线与限制线已合并成一条 -->
        <HPanel
          title="起播线"
          desc="拖滑杆选到哪一集，就只找它之后的资源并订阅；提交成功会自动往前推"
          flush
        >
          <div v-if="seriesSubs.length" class="wm-list">
            <div v-for="s in seriesSubs" :key="s.id" class="wm-row">
              <div class="wm-name">
                <TitleLink :type="s.media_type" :tmdb-id="s.tmdb_id" :title="s.title" :year="s.year">
                  {{ s.title }}
                </TitleLink>
                <span class="muted small">
                  {{ subProgress(s) }} · {{ subStateText[s.state] }}
                  <template v-if="s.total_episodes"> · 共 {{ s.total_episodes }} 集</template>
                </span>
              </div>
              <div class="wm-edit">
                <SubLineSlider
                  compact
                  :sub-id="s.id"
                  :season="s.watermark_season"
                  :episode="s.watermark_episode"
                  :total-episodes="s.total_episodes"
                  :title="s.title"
                  @saved="onLineSaved(s, $event)"
                />
              </div>
            </div>
          </div>
          <HEmpty v-else title="还没有剧集订阅" desc="在片单里点开一部剧，详情里点「订阅追更」" />

          <div v-if="seriesSubs.length" class="wm-notes">
            <p class="muted small">
              <strong>起播线</strong>：这一集之前的不管，这一集本身和之后的都会搜。
              程序提交成功后会自己往前推；这里和影视详情抽屉用的是同一根滑杆。
            </p>
            <p class="muted small">
              往回拖会重新搜索、重新提交那些集，花 115 的离线配额，所以会先问一次。已经下过的集不会被删。
            </p>
          </div>
        </HPanel>
      </div>
    </template>
  </div>
</template>

<style scoped>
.search-hero {
  padding: 20px 22px;
  background:
    radial-gradient(80% 140% at 10% 0%, var(--accent-soft), transparent 62%),
    var(--surface);
}
.list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 16px;
}
.checkline {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
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

/* 追更进度 */
.wm-list {
  display: flex;
  flex-direction: column;
}
.wm-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--separator);
  flex-wrap: wrap;
}
.wm-row:last-child {
  border-bottom: 0;
}
.wm-name {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}
.wm-edit {
  flex: 1 1 340px;
  max-width: 460px;
  min-width: 260px;
}
.wm-notes {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 16px 14px;
  border-top: 1px solid var(--separator);
}
</style>
