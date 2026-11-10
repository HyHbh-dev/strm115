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
 * 来源清单（tg_channels / res_sites / res_sites_custom）在「系统配置 → 资源来源」里改，
 * 这个页面只负责搜，以及把认出的 115 分享一键转存。
 */
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import TitlePicker from '@/components/TitlePicker.vue'
import HPanel from '@/components/HPanel.vue'
import HField from '@/components/HField.vue'
import HEmpty from '@/components/HEmpty.vue'
import DirPicker from '@/components/DirPicker.vue'
import SubscribeDialog from '@/components/SubscribeDialog.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { loadSubsIndex, subIdOf, subsVersion } from '@/stores/subscriptions'
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

onMounted(() => {
  void loadShareTarget()
  // 订阅按钮要能立刻判断「这部订过没」；store 内部有 loaded 缓存，重复调无害。
  void loadSubsIndex()
})

/**
 * 只有认片锁定、且命中 TMDB 的条目才能订阅：专有日历靠 tmdb_id 拉集数，
 * 豆瓣条目没有 TMDB id，硬建一条订阅只会永远停在「查不到集数」。
 */
const canSubscribe = computed(
  () => !!picked.value && picked.value.source !== 'douban' && picked.value.id > 0,
)
const currentSubId = computed(() => {
  void subsVersion.value // 订阅增删后强制重算
  const h = picked.value
  if (!h || !canSubscribe.value) return 0
  return subIdOf(h.kind === 'tv' ? 'tv' : 'movie', h.id)
})
const subOpen = ref(false)

function onSubscribed() {
  // 弹窗里已经 rememberSub + 强刷索引，这里只把订阅页的表格也标脏。
  void loadSubsIndex(true)
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
          <button
            v-if="canSubscribe"
            class="btn btn-sm"
            :class="currentSubId ? 'btn-tertiary' : 'btn-primary'"
            :title="currentSubId ? '已经在订阅里了' : picked?.kind === 'tv' ? '按 TMDB 集数选起始集' : '有资源就自动下载'"
            @click="subOpen = true"
          >
            <AppIcon :name="currentSubId ? 'check' : 'plus'" :size="14" />
            {{ currentSubId ? '已订阅' : picked?.kind === 'tv' ? '订阅追更' : '订阅' }}
          </button>
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
              ? '换英文名再试，或放开体积筛选；也可以到「系统配置 → 资源来源」加更多来源'
              : '中文名直接搜就行，后端会去 IMDb 换英文名'
          "
          icon="search"
        />
      </div>
    </HPanel>

    <!-- 转存目录；来源清单已搬到「系统配置 → 资源来源」 -->
    <HPanel title="转存目录" desc="TG 频道、种子站和自定义 RSS 源在「系统配置 → 资源来源」里配置">
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
      <p class="muted small" style="margin-top: 10px">
        搜索用的 TG 频道、内置种子站、自定义 RSS 源与连通性测试，已移到
        <RouterLink to="/settings?tab=resource">系统配置 → 资源来源</RouterLink>；
        改完保存，下一轮搜索立即生效。
      </p>
    </HPanel>

    <DirPicker v-model="dirPicker" @pick="onPickDir" />
    <SubscribeDialog v-model="subOpen" :hit="picked" @subscribed="onSubscribed" />
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
</style>
