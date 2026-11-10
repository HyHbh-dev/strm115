<script setup lang="ts">
/**
 * 影视详情抽屉 —— 全站唯一一份。
 *
 * 任何页面调 openDetail({ type, tmdbId }) 就能弹出来，不要求这部片已经入库。
 * 数据来自 /api/title：TMDB 详情 + 本地台账（有就带上）+ 订阅状态（订了就带上）。
 */
import { computed, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HModal from '@/components/HModal.vue'
import HSwitch from '@/components/HSwitch.vue'
import SubLineSlider from '@/components/SubLineSlider.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'
import { closeDetail, detailTarget } from '@/stores/detail'
import { forgetSub, loadSubsIndex, rememberSub } from '@/stores/subscriptions'
import { pad2, copyText } from '@/utils/format'

interface LocalView {
  exists: boolean
  media_id?: number
  library_id?: number
  library?: string
  rel_path?: string
  kind?: string
  season?: number
  episode?: number
  episodes: number
  sub_state?: string
  sub_origin?: string
  sub_lang?: string
  poster?: string
  backdrop?: string
  /** 目录跳转：网盘 cid / 显示路径 / 本地绝对路径 / 相对库根路径 */
  remote_cid?: string
  remote_dir_cid?: string
  remote_dir_exact?: boolean
  remote_path?: string
  local_root?: string
  local_dir?: string
  host_dir?: string
  local_rel_dir?: string
  /** 已入库条目在海报墙里的 key，删除按钮直接用它 */
  catalog_key?: string
}

interface SubView {
  id: number
  state: string
  quality: string
  /** 起播线：从这一集开始追（含），程序提交成功后自己往前推。0/0 = 从头追 */
  watermark_season: number
  watermark_episode: number
  total_episodes: number
  last_result?: string
  next_check_at?: string
}

/** 一季有几集。TMDB 的 /tv/{id} 里带着，详情接口原样给过来。 */
interface SeasonLen {
  season: number
  episodes: number
}

interface TitleDetail {
  type: string
  tmdb_id: number
  imdb_id?: string
  title: string
  original?: string
  year?: number
  rating?: number
  runtime?: string
  overview?: string
  genres?: string[]
  cast?: string[]
  director?: string[]
  poster?: string
  backdrop?: string
  seasons?: number[]
  /** 每季集数：起播线滑杆靠它把「全剧第 N 集」换算成季/集 */
  season_episodes?: SeasonLen[]
  local?: LocalView
  sub?: SubView
  note?: string
}

const d = ref<TitleDetail | null>(null)
const loading = ref(false)
const err = ref('')
const busy = ref(false)
const broken = ref(false)

const target = computed(() => detailTarget.value)
const open = computed(() => !!target.value)

watch(
  detailTarget,
  (t) => {
    if (!t) {
      d.value = null
      err.value = ''
      broken.value = false
      return
    }
    void load()
  },
  { immediate: true },
)

async function load() {
  const t = target.value
  if (!t) return
  loading.value = true
  err.value = ''
  broken.value = false
  try {
    const r: any = await api.title.detail({
      type: t.type,
      tmdb_id: t.tmdbId,
      media_id: t.mediaId,
      q: t.query,
      year: t.year,
    })
    d.value = r
    // 接口是「订没订」的权威答案，顺手把本地索引对齐 —— 可能是在别的设备上订的。
    if (r?.sub?.id) {
      rememberSub(r.type, r.tmdb_id, Number(r.sub.id))
    } else if (r?.tmdb_id) {
      forgetSub(r.type, r.tmdb_id)
    }
  } catch (e: any) {
    err.value = e?.message || '详情没拉到'
  } finally {
    loading.value = false
  }
}

/* ---------------- 订阅 ---------------- */

const subUser = computed(() => d.value?.sub ?? null)
const isSeries = computed(() => d.value?.type !== 'movie')
const canSubscribe = computed(() => !!d.value?.tmdb_id)

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

/** 水位线文案。0 表示一集都还没交过，别写成「S00E00」。 */
function watermarkText(s: SubView): string {
  if (!isSeries.value) return s.state === 'done' ? '已入库' : '等待资源'
  if (!s.watermark_episode) return '还没开始'
  return `S${pad2(s.watermark_season)}E${pad2(s.watermark_episode)}`
}

/* ---------------- 起播线 ----------------
 * 滑杆的换算、往回拖确认、保存、刷新订阅索引全在 SubLineSlider 里。
 * 这里只负责把详情里的分季集数 / 订阅行喂给它；保存完 refresh() 拉回权威值。
 */

async function subscribe() {
  const t = d.value
  if (!t || busy.value || !t.tmdb_id) return
  if (subUser.value) {
    busy.value = true
    try {
      await api.subscriptions.remove(subUser.value.id)
      forgetSub(t.type, t.tmdb_id)
      toast.success(`已取消订阅「${t.title}」`, '已经交出去的离线任务不受影响')
      await refresh()
    } catch (e: any) {
      toast.warning('取消失败', e?.message || '')
    } finally {
      busy.value = false
    }
    return
  }
  busy.value = true
  try {
    const r: any = await api.subscriptions.create({
      tmdb_id: t.tmdb_id,
      media_type: isSeries.value ? 'tv' : 'movie',
      title: t.title,
      original_title: t.original,
      year: t.year || 0,
      imdb_id: t.imdb_id,
    })
    rememberSub(t.type, t.tmdb_id, Number(r?.id) || 0)
    toast.success(`已订阅「${t.title}」`, '出新集会自动提交离线，画质默认 1080p')
    await refresh()
  } catch (e: any) {
    // 409 = 之前在别的页面订过。这不是错误，把索引对齐就行。
    if (e?.status === 409) {
      toast.info('已经在订阅里了')
      await refresh()
    } else {
      toast.warning('订阅失败', e?.message || '')
    }
  } finally {
    busy.value = false
  }
}

/** 改完订阅状态要重拉详情，别让抽屉里显示的和列表页不一样。 */
async function refresh() {
  await load()
  void loadSubsIndex(true)
}

/* ---------------- 删除：详情页底部左侧的明确入口 ---------------- */
/**
 * 以前删除只藏在海报墙的卡片悬停按钮里，从别处点片名打开的这份统一详情
 * 抽屉找不到入口。现在 /api/title 直接带回 catalog_key，这里就能复用
 * DELETE /api/catalog/item（默认只清本地索引，可选连 115 源文件一起删）。
 */
const canDelete = computed(() => !!d.value?.local?.exists && !!d.value.local.catalog_key)
const delOpen = ref(false)
const purge = ref(false)
const deleting = ref(false)

function openDelete() {
  purge.value = false
  delOpen.value = true
}

async function submitDelete() {
  const key = d.value?.local?.catalog_key
  if (!key || deleting.value) return
  deleting.value = true
  try {
    await api.catalog.remove(key, purge.value)
    toast.success(
      '已从媒体库移除',
      purge.value ? '115 源文件也一并删除（回收站可恢复）' : '115 源文件保留，下次同步可能重新生成',
    )
    delOpen.value = false
    closeDetail()
  } catch (e: any) {
    toast.warning('删除失败', e?.message || '稍后再试')
  } finally {
    deleting.value = false
  }
}

/* ---------------- 展示 ---------------- */

const cover = computed(() => {
  if (!d.value || broken.value) return ''
  return d.value.poster || d.value.local?.poster || ''
})

const headBg = computed(() => {
  const b = d.value?.backdrop
  return b && !broken.value ? { backgroundImage: `url(${b})` } : undefined
})

const imdbURL = computed(() =>
  d.value?.imdb_id ? `https://www.imdb.com/title/${d.value.imdb_id}/` : '',
)
const tmdbURL = computed(() =>
  d.value?.tmdb_id ? `https://www.themoviedb.org/${isSeries.value ? 'tv' : 'movie'}/${d.value.tmdb_id}` : '',
)

/* ---------------- 目录跳转 ---------------- */

/**
 * 115 网页版靠 cid 定位目录。能解析到「这部片所在目录」就跳到那里，
 * 解析不到（没台账 / 115 风控 / 目录已删）就退回媒体库根目录 ——
 * 绝不给一个拼名字拼出来的错误地址。
 * 注意：115 网页版的登录态在浏览器里，服务端的 Cookie 不会自动带上。
 */
const panURL = computed(() => {
  const l = d.value?.local
  if (!l?.exists) return ''
  const cid = l.remote_dir_cid || l.remote_cid
  if (!cid) return ''
  return `https://115.com/?cid=${encodeURIComponent(cid)}&offset=0&tab=&mode=wangpan`
})
const panHint = computed(() =>
  d.value?.local?.remote_dir_exact
    ? '打开这部片在 115 上的目录（浏览器需已登录 115 网页版）'
    : '打开这个媒体库在 115 上的根目录（浏览器需已登录 115 网页版）',
)

/**
 * 给人用的本地路径：Docker 部署时优先用换算出来的宿主机路径，
 * 容器里的 /media/... 在 Windows 宿主上根本没有意义。
 */
const bestLocalPath = computed(() => d.value?.local?.host_dir || d.value?.local?.local_dir || '')

/** file:// 链接：浏览器通常会拦，所以旁边一定留「复制路径」和「打开目录」。 */
const localFileURL = computed(() => {
  const p = bestLocalPath.value
  if (!p) return ''
  const norm = p.replace(/\\/g, '/')
  if (/^[A-Za-z]:\//.test(norm)) return `file:///${norm}`
  return `file://${norm.startsWith('/') ? '' : '/'}${norm}`
})

// 模板 @click 直接绑 copy，保留名字；剪贴板与提示统一走 copyText。
async function copy(text?: string) {
  if (!text) {
    toast.warning('这条没有可复制的路径')
    return
  }
  await copyText(text, '路径已复制')
}

const openingDir = ref(false)
async function openLocalDir() {
  const l = d.value?.local
  if (!l?.library_id || openingDir.value) return
  openingDir.value = true
  try {
    const r: any = await api.local.open({
      library_id: l.library_id,
      path: l.local_rel_dir || '',
    })
    if (r?.opened) {
      toast.success('已打开本地目录', r.path)
    } else {
      // 容器部署时把宿主机路径直接放进剪贴板，省得用户再点一次「复制路径」。
      if (r?.host_path) {
        try {
          await navigator.clipboard.writeText(r.host_path)
        } catch {
          /* 剪贴板不可用时下面仍会把路径显示出来 */
        }
      }
      toast.info('没法自动打开', r?.host_path || r?.hint || '请复制路径自己打开')
    }
  } catch (e: any) {
    toast.warning('打开失败', e?.message || '')
  } finally {
    openingDir.value = false
  }
}

const subLabel: Record<string, string> = {
  ok: '有中文字幕',
  missing: '没有中文字幕',
  mismatch: '字幕和片源对不上',
  failed: '字幕下载失败',
}

function subText(l: LocalView): string {
  if (!l.sub_state) return '未补字幕'
  const base = subLabel[l.sub_state] || l.sub_state
  if (l.sub_origin === 'translated') return `${base}（AI 翻译）`
  return base
}

function subTone(l: LocalView): string {
  if (l.sub_state === 'ok') return 'badge-success'
  if (!l.sub_state) return 'badge'
  return 'badge-warning'
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') closeDetail()
}
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="overlay md-overlay" @click.self="closeDetail" @keydown="onKey" tabindex="-1">
      <aside class="drawer">
        <header class="md-head" :style="headBg">
          <div class="md-head-main">
            <div class="md-art">
              <img v-if="cover" :src="cover" alt="" @error="broken = true" />
              <div v-else class="ph">{{ d?.title || target?.title || '—' }}</div>
            </div>
            <div class="md-titles">
              <h2 class="md-title">{{ d?.title || target?.title || '加载中…' }}</h2>
              <p v-if="d?.original && d.original !== d.title" class="muted small">{{ d.original }}</p>
              <div class="row wrap" style="gap: 6px; margin-top: 8px">
                <span v-if="d" class="badge badge-accent">{{ isSeries ? '剧集' : '电影' }}</span>
                <span v-if="d?.year" class="badge">{{ d.year }}</span>
                <span v-if="d?.rating" class="badge badge-info">★ {{ d.rating.toFixed(1) }}</span>
                <span v-if="d?.local?.exists" class="badge badge-success">已入库</span>
                <span v-if="subUser" class="badge" :class="subStateTone[subUser.state]">
                  {{ subStateText[subUser.state] }}
                </span>
              </div>
            </div>
          </div>
          <button class="btn btn-sm btn-ghost" title="关闭" @click="closeDetail">
            <AppIcon name="close" :size="16" />
          </button>
        </header>

        <div class="drawer-body">
          <div v-if="loading" class="col" style="gap: 10px">
            <span class="skeleton" style="width: 100%; height: 14px" />
            <span class="skeleton" style="width: 92%; height: 14px" />
            <span class="skeleton" style="width: 70%; height: 14px" />
          </div>

          <p v-else-if="err" class="md-empty">{{ err }}</p>

          <template v-else-if="d">
            <p v-if="d.note" class="md-note">{{ d.note }}</p>

            <div class="row wrap small muted" style="gap: 10px; margin-bottom: 12px">
              <span v-if="d.runtime">{{ d.runtime }}</span>
              <span v-if="d.director?.length">导演 {{ d.director.join(' / ') }}</span>
              <span v-if="d.genres?.length">{{ d.genres.join(' · ') }}</span>
            </div>

            <p v-if="d.overview" class="md-desc">{{ d.overview }}</p>
            <p v-else class="md-empty">TMDB 上没有简介</p>

            <p v-if="d.cast?.length" class="small muted" style="margin-bottom: 14px">
              主演 {{ d.cast.slice(0, 6).join(' / ') }}
            </p>

            <!-- 本地 -->
            <section class="md-block">
              <div class="md-block-title">本地</div>
              <template v-if="d.local?.exists">
                <div class="md-line">
                  <span class="muted">位置</span>
                  <b class="mono small" style="word-break: break-all">
                    {{ d.local.library }} / {{ d.local.rel_path }}
                  </b>
                </div>
                <div class="md-line md-dir-line">
                  <span class="muted">目录</span>
                  <div class="md-dir-actions">
                    <a
                      v-if="panURL"
                      class="btn btn-sm btn-tertiary"
                      :href="panURL"
                      target="_blank"
                      rel="noreferrer"
                      :title="panHint"
                    >
                      <AppIcon name="external" :size="12" />115 目录
                    </a>
                    <a
                      v-if="localFileURL"
                      class="btn btn-sm btn-ghost"
                      :href="localFileURL"
                      target="_blank"
                      rel="noreferrer"
                      title="file:// 链接，浏览器可能拦截；拦了就用旁边的按钮"
                    >
                      <AppIcon name="external" :size="12" />本地链接
                    </a>
                    <button
                      v-if="bestLocalPath"
                      class="btn btn-sm btn-ghost"
                      title="复制本地目录的绝对路径"
                      @click="copy(bestLocalPath)"
                    >
                      <AppIcon name="folder" :size="12" />复制路径
                    </button>
                    <button
                      v-if="d.local.library_id"
                      class="btn btn-sm btn-tertiary"
                      :disabled="openingDir"
                      title="在运行本服务的机器上打开这个文件夹（容器部署时不可用）"
                      @click="openLocalDir"
                    >
                      <AppIcon name="folder" :size="12" />{{ openingDir ? '打开中…' : '打开目录' }}
                    </button>
                  </div>
                </div>
                <div v-if="bestLocalPath" class="md-line">
                  <span class="muted">本地路径</span>
                  <b class="mono small" style="word-break: break-all; text-align: right">
                    {{ bestLocalPath }}
                  </b>
                </div>
                <div
                  v-if="d.local.host_dir && d.local.host_dir !== d.local.local_dir"
                  class="md-line"
                >
                  <span class="muted">容器内</span>
                  <b class="mono small" style="word-break: break-all; text-align: right">
                    {{ d.local.local_dir }}
                  </b>
                </div>
                <div v-if="d.local.remote_path" class="md-line">
                  <span class="muted">网盘路径</span>
                  <b class="mono small" style="word-break: break-all; text-align: right">
                    {{ d.local.remote_path }}
                  </b>
                </div>
                <div v-if="isSeries" class="md-line">
                  <span class="muted">已入库</span>
                  <b>{{ d.local.episodes }} 集</b>
                </div>
                <div class="md-line">
                  <span class="muted">字幕</span>
                  <span class="badge" :class="subTone(d.local)">{{ subText(d.local) }}</span>
                </div>
              </template>
              <p v-else class="muted small">还没入库。订阅之后出现资源会自动提交离线。</p>
              <p v-if="canDelete" class="muted small" style="margin-top: 8px">
                要移除这条，用抽屉底部左侧的「删除」按钮（可选连同 115 源文件一起删）。
              </p>
            </section>

            <!-- 订阅 -->
            <section class="md-block">
              <div class="md-block-title">订阅</div>
              <template v-if="subUser">
                <div class="md-line">
                  <span class="muted">{{ isSeries ? '已交到' : '进度' }}</span>
                  <b class="mono">
                    {{ watermarkText(subUser) }}
                    <span v-if="isSeries && subUser.total_episodes" class="muted small">
                      / 共 {{ subUser.total_episodes }} 集
                    </span>
                  </b>
                </div>
                <div class="md-line">
                  <span class="muted">画质</span>
                  <b>{{ subUser.quality }}</b>
                </div>
                <div v-if="subUser.last_result" class="md-line">
                  <span class="muted">最近一轮</span>
                  <span class="small" style="text-align: right">{{ subUser.last_result }}</span>
                </div>

                <!-- 起播线：和订阅设置、播出日历详情共用同一个滑杆组件 -->
                <SubLineSlider
                  v-if="isSeries && subUser"
                  :sub-id="subUser.id"
                  :season="subUser.watermark_season"
                  :episode="subUser.watermark_episode"
                  :total-episodes="subUser.total_episodes"
                  :season-episodes="d?.season_episodes"
                  :title="d?.title"
                  @saved="refresh"
                />
              </template>
              <p v-else class="muted small">
                {{
                  isSeries
                    ? '没订阅。订了之后每轮问 TMDB 该出到第几集，有新的就自动搜资源并提交离线。'
                    : '没订阅。订了之后有资源就自动提交离线。'
                }}
              </p>
            </section>
          </template>
        </div>

        <footer class="md-foot">
          <div class="row wrap" style="gap: 6px">
            <a v-if="imdbURL" class="btn btn-sm btn-tertiary" :href="imdbURL" target="_blank" rel="noreferrer">
              IMDb <AppIcon name="external" :size="12" />
            </a>
            <a v-if="tmdbURL" class="btn btn-sm btn-tertiary" :href="tmdbURL" target="_blank" rel="noreferrer">
              TMDB <AppIcon name="external" :size="12" />
            </a>
            <button
              v-if="canDelete"
              class="btn btn-sm btn-danger"
              :disabled="busy"
              title="从媒体库移除这条（含 .strm / 台账 / Emby 条目）"
              @click="openDelete"
            >
              <AppIcon name="trash" :size="12" />删除
            </button>
          </div>
          <button
            v-if="canSubscribe"
            class="btn"
            :class="subUser ? 'btn-danger-ghost' : 'btn-primary'"
            :disabled="busy"
            :title="subUser ? '再点一下取消订阅' : '出新集自动提交离线'"
            @click="subscribe"
          >
            {{ busy ? '处理中…' : subUser ? '已订阅 · 点此取消' : isSeries ? '订阅追更' : '订阅' }}
          </button>
        </footer>
      </aside>
    </div>
  </Teleport>

  <!-- 删除确认：和海报墙用的是同一个后端接口，默认只清本地索引 -->
  <HModal v-model="delOpen" title="从媒体库移除">
    <template #desc>会清理本地 .strm、nfo、海报、字幕、台账和 Emby 条目</template>
    <div class="col" style="gap: 12px">
      <div class="md-del-target">
        <strong>{{ d?.title }}</strong>
        <p class="mono small muted" style="margin-top: 4px">
          {{ d?.local?.host_dir || d?.local?.local_dir || d?.local?.rel_path || '—' }}
        </p>
      </div>
      <div class="row-between" style="gap: 12px">
        <div>
          <strong class="small">同时删除 115 网盘里的源文件</strong>
          <p class="muted small">源文件进 115 回收站可恢复；不勾选则网盘保留，下次同步会重新生成。</p>
        </div>
        <HSwitch v-model="purge" label="同时删除 115 源文件" />
      </div>
    </div>
    <template #footer>
      <button class="btn btn-tertiary" @click="delOpen = false">取消</button>
      <button class="btn btn-danger" :disabled="deleting" @click="submitDelete">
        {{ deleting ? '删除中…' : purge ? '删除条目与源文件' : '只移除索引' }}
      </button>
    </template>
  </HModal>
</template>

<style scoped>
.md-del-target {
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--surface-secondary);
  overflow-wrap: anywhere;
}
.md-overlay {
  justify-items: end;
  padding: 0;
  /* 日历 / 海报墙的操作抽屉也是 .overlay（同一个 z-index）。在那些抽屉的
     标题上点片名时，两个抽屉会叠在一起 —— 统一详情必须压在最上面，
     否则点了没反应，看起来又像「片名点不开」。 */
  z-index: calc(var(--z-overlay) + 1);
}
.md-head {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  padding: 18px 20px 14px;
  border-bottom: 1px solid var(--separator);
  background-size: cover;
  background-position: center 18%;
  background-color: var(--surface-secondary);
}
.md-head-main {
  display: flex;
  gap: 14px;
  min-width: 0;
}
.md-art {
  position: relative;
  flex: none;
  width: 82px;
  aspect-ratio: 2 / 3;
  border-radius: var(--r-md);
  overflow: hidden;
  border: 1px solid var(--border);
  background: var(--surface-tertiary, var(--surface-secondary));
}
.md-art img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}
.md-art .ph {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  padding: 8px;
  color: var(--muted);
  font-size: 11px;
  font-weight: 600;
  text-align: center;
  line-height: 1.35;
}
.md-titles {
  min-width: 0;
}
.md-title {
  font-size: 18px;
  line-height: 1.3;
}
.md-note {
  margin-bottom: 10px;
  padding: 8px 10px;
  border-radius: var(--r-sm);
  background: var(--warning-soft, var(--surface-secondary));
  color: var(--muted);
  font-size: 12px;
  line-height: 1.6;
}
.md-desc {
  font-size: 13px;
  line-height: 1.8;
  color: var(--text-secondary, var(--muted));
  margin-bottom: 12px;
}
.md-empty {
  font-size: 13px;
  color: var(--muted);
  padding: 6px 0;
}
.md-block {
  margin-top: 6px;
  margin-bottom: 16px;
  padding: 12px 14px;
  border-radius: var(--r-md);
  background: var(--surface-secondary);
  border: 1px solid var(--border);
}
.md-block-title {
  font-size: 12px;
  font-weight: 500;
  color: var(--muted);
  margin-bottom: 8px;
  letter-spacing: 0.02em;
}
.md-line {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  font-size: 13px;
  line-height: 1.7;
}
.md-line + .md-line {
  margin-top: 2px;
}
/* 目录行放的是按钮，基线对齐会把它们挤到奇怪的位置 */
.md-dir-line {
  align-items: center;
}
.md-dir-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 6px;
}
.md-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 12px 20px;
  border-top: 1px solid var(--separator);
  background: var(--surface-secondary);
}
</style>
