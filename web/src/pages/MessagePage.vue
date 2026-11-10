<script setup lang="ts">
/**
 * 消息配置：八个通知渠道 + 事件订阅 + 测试发送。
 *
 * 这一页之前整页是示例数据（写死的 webhook、点「保存」只弹 toast）。
 * 现在每个开关都真的落库：
 *  - GET  /api/notify            读配置（密钥打码，用 secret_set 标记「已保存」）
 *  - PUT  /api/notify            存配置（密钥字段留空 = 不修改）
 *  - POST /api/notify/test       传 channel 只测那一个渠道
 *  - POST /api/notify/tg/discover 用 Bot Token 反查 Chat ID
 *
 * 两个刻意的取舍：
 * 1. 密钥永不回传明文，界面只显示「已保存 · 留空不改」。所以不要把打码值
 *    当成真值再发回去，否则会用一个星号串覆盖掉真 Token。
 * 2. 测试发的是**已保存**的配置。改了没保存就点测试，测的是旧配置——
 *    这点必须写在界面上，否则排查半天以为是配置不生效。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import HField from '@/components/HField.vue'
import HPanel from '@/components/HPanel.vue'
import HSwitch from '@/components/HSwitch.vue'
import { api } from '@/api/endpoints'
import { toast } from '@/stores/toast'

interface FieldDef {
  key: string
  label: string
  tip?: string
  hint?: string
  placeholder?: string
  required?: boolean
  /** 密钥类字段：不回传明文，留空表示不修改 */
  secret?: boolean
  mono?: boolean
  textarea?: boolean
  options?: { value: string; label: string }[]
}
interface ChannelDef {
  /** 配置里的键名 */
  key: string
  /** 后端渠道名，测试接口按这个找 */
  name: string
  title: string
  desc: string
  fields: FieldDef[]
}

const defs: ChannelDef[] = [
  {
    key: 'tg',
    name: 'telegram',
    title: 'Telegram 机器人',
    desc: 'Bot 推送到私聊 / 群 / 频道，需要能访问 api.telegram.org',
    fields: [
      { key: 'token', label: 'Bot Token', required: true, secret: true, mono: true, tip: '找 @BotFather 发 /newbot 拿 Token，形如 123456:AAE...', placeholder: '123456789:AAE...' },
      { key: 'chat_id', label: 'Chat ID', required: true, mono: true, hint: '不知道就点下面的「获取 Chat ID」；群是负数，如 -1001234567890' },
    ],
  },
  {
    key: 'wecom',
    name: 'wecom',
    title: '企业微信应用',
    desc: '自建应用推送给成员，可顺带接双向机器人回调',
    fields: [
      { key: 'corp_id', label: 'CorpID', required: true, mono: true, tip: '企业微信后台 → 我的企业 → 企业信息' },
      { key: 'secret', label: 'Secret', required: true, secret: true, mono: true, tip: '应用详情页的 Secret；不是通讯录 Secret' },
      { key: 'agent_id', label: 'AgentID', required: true, mono: true },
      { key: 'api_url', label: 'API 地址', mono: true, hint: '留空走官方 https://qyapi.weixin.qq.com，私有化部署才填' },
    ],
  },
  {
    key: 'dingtalk',
    name: 'dingtalk',
    title: '钉钉群机器人',
    desc: '群自定义机器人，配了加签会自动算 sign',
    fields: [
      { key: 'webhook', label: 'Webhook', required: true, mono: true, tip: '群设置 → 智能群助手 → 添加机器人 → 自定义 → 复制 Webhook', placeholder: 'https://oapi.dingtalk.com/robot/send?access_token=' },
      { key: 'secret', label: '加签密钥', secret: true, mono: true, hint: '安全设置选了「加签」才需要，留空表示没开' },
      { key: 'keyword', label: '关键词', hint: '安全设置选了「自定义关键词」时必填，会自动加在正文开头' },
    ],
  },
  {
    key: 'feishu',
    name: 'feishu',
    title: '飞书群机器人',
    desc: '群自定义机器人 webhook，配了签名会自动带上',
    fields: [
      { key: 'webhook', label: 'Webhook', required: true, mono: true, placeholder: 'https://open.feishu.cn/open-apis/bot/v2/hook/...' },
      { key: 'secret', label: '签名密钥', secret: true, mono: true, hint: '安全设置开了「签名校验」才需要' },
    ],
  },
  {
    key: 'qq_onebot',
    name: 'qq_onebot',
    title: 'QQ 机器人（OneBot）',
    desc: 'NapCat / Lagrange / LLOneBot 暴露的 HTTP 接口',
    fields: [
      { key: 'url', label: '接口地址', required: true, mono: true, placeholder: 'http://192.168.1.10:3000' },
      { key: 'token', label: 'Access Token', secret: true, mono: true, hint: 'OneBot 配了 token 才填' },
      { key: 'target_type', label: '发送到', options: [{ value: 'group', label: '群聊' }, { value: 'private', label: '私聊' }] },
      { key: 'target', label: '群号 / QQ 号', required: true, mono: true },
    ],
  },
  {
    key: 'qq_official',
    name: 'qq_official',
    title: 'QQ 官方机器人',
    desc: 'q.qq.com 创建的机器人，走开放平台接口',
    fields: [
      { key: 'app_id', label: 'AppID', required: true, mono: true },
      { key: 'secret', label: 'AppSecret', required: true, secret: true, mono: true },
      { key: 'group_id', label: '群 OpenID', required: true, mono: true, tip: '不是群号，是开放平台的群 openid' },
    ],
  },
  {
    key: 'iyuu',
    name: 'iyuu',
    title: 'IYUU 微信推送',
    desc: '爱语飞飞公众号推送，连发会被限速（默认间隔 5 秒）',
    fields: [
      { key: 'token', label: 'IYUU Token', required: true, secret: true, mono: true, tip: 'https://iyuu.cn 扫码后拿到的 Token' },
    ],
  },
  {
    key: 'webhook',
    name: 'webhook',
    title: '通用 Webhook',
    desc: 'POST / GET 到你自己的地址，可以自定义请求体',
    fields: [
      { key: 'url', label: '地址', required: true, mono: true, placeholder: 'https://example.com/hook' },
      { key: 'method', label: '方法', options: [{ value: 'POST', label: 'POST' }, { value: 'GET', label: 'GET' }] },
      { key: 'body', label: '请求体模板', textarea: true, mono: true, hint: '留空发默认 JSON；可用 {{title}} {{content}} {{type}} {{type_name}} {{time}} {{link}} {{image}}' },
    ],
  },
]

/** 后端 Config 的零值形状。服务端省略的字段（proxy / 超时）靠它兜底。 */
function blankForm(): Record<string, any> {
  const f: Record<string, any> = {
    http_timeout_sec: 15,
    proxy_url: '',
    iyuu_gap_sec: 5,
  }
  for (const d of defs) {
    const ch: Record<string, any> = { enabled: false }
    for (const x of d.fields) {
      ch[x.key] = x.options ? x.options[0].value : ''
    }
    f[d.key] = ch
  }
  return f
}

const form = reactive<Record<string, any>>(blankForm())
const secretSet = ref<Record<string, boolean>>({})
const channels = ref<string[]>([])
const events = ref<string[]>([])
const eventLabels = ref<{ id: string; label: string; desc: string }[]>([])
const autoSync = ref(true)
const loading = ref(true)
const loaded = ref(false)
const saving = ref(false)
const savingEvents = ref(false)

const sel = ref('tg')
const cur = computed(() => defs.find((d) => d.key === sel.value) ?? defs[0])

/* ==================== 双向机器人 ==================== */
/**
 * Telegram 机器人是个「接收端」：同一个 Bot Token，既能往外推通知，
 * 也能在聊天窗口里收 115 分享 / 磁力 / 片名直接入库。
 * 这里只管三件事：Token 有没有、有没有绑定会话、白名单怎么写。
 */
interface BotStatus {
  token_set: boolean
  bound: boolean
  allowed: { chat_id: string; label: string }[]
  status: string
  commands: { command: string; desc: string }[]
}
const bot = ref<BotStatus | null>(null)
const botIDs = ref('')
const botSaving = ref(false)

async function loadBot() {
  try {
    const r = await api.notify.tgBot()
    bot.value = r as BotStatus
    botIDs.value = (r?.allowed ?? []).map((a: { chat_id: string }) => a.chat_id).join(', ')
  } catch {
    // 面板加载失败不该挡住整页配置
  }
}

async function saveBot(reset = false) {
  botSaving.value = true
  try {
    await api.notify.saveTgBot(reset ? { reset: true } : { chat_ids: botIDs.value })
    toast.success(
      reset ? '已解绑全部会话' : '白名单已保存',
      reset ? '下一个给 Bot 发消息的会话会重新绑定' : '改完立即生效，不用重启',
    )
    await loadBot()
  } catch (e: any) {
    toast.error(reset ? '解绑失败' : '保存失败', e?.message)
  } finally {
    botSaving.value = false
  }
}

/** 表单快照，用来判断「有没有没保存的改动」 */
const snap = ref('')
const snapEvents = ref('')
const dirty = computed(() => loaded.value && JSON.stringify(form) !== snap.value)
const eventsDirty = computed(
  () => loaded.value && JSON.stringify([events.value, autoSync.value]) !== snapEvents.value,
)

const activeInfo = computed(() => {
  const out: { title: string; name: string }[] = []
  for (const d of defs) {
    if (channels.value.includes(d.name)) out.push({ title: d.title, name: d.name })
  }
  return out
})

function stateOf(d: ChannelDef): 'on' | 'partial' | 'off' {
  if (channels.value.includes(d.name)) return 'on'
  return form[d.key]?.enabled ? 'partial' : 'off'
}
function stateLabel(d: ChannelDef): string {
  const s = stateOf(d)
  return s === 'on' ? '已启用' : s === 'partial' ? '未生效' : '未启用'
}
/** 列表里显示一行摘要，不用点进去就知道这个渠道配到哪了 */
function summaryOf(d: ChannelDef): string {
  const c = form[d.key] ?? {}
  const pick = (...keys: string[]) => {
    for (const k of keys) {
      const v = String(c[k] ?? '').trim()
      if (v) return v
    }
    return ''
  }
  switch (d.key) {
    case 'tg':
      return pick('chat_id') ? `chat ${pick('chat_id')}` : pick('token') || secretSet.value['tg.token'] ? 'Token 已保存' : '未配置'
    case 'wecom':
      return pick('corp_id', 'agent_id') || '未配置'
    case 'dingtalk':
    case 'feishu':
    case 'webhook':
      return pick('url', 'webhook') || '未配置'
    case 'qq_onebot':
      return pick('url') || '未配置'
    case 'qq_official':
      return pick('app_id') || '未配置'
    case 'iyuu':
      return pick('token') || (secretSet.value['iyuu.token'] ? 'Token 已保存' : '未配置')
    default:
      return '未配置'
  }
}
function placeholderOf(d: ChannelDef, f: FieldDef): string {
  if (f.secret && secretSet.value[`${d.key}.${f.key}`]) return '已保存 · 留空表示不修改'
  return f.placeholder ?? ''
}
function hintOf(d: ChannelDef, f: FieldDef): string {
  if (f.secret && secretSet.value[`${d.key}.${f.key}`]) {
    return '已保存一份密钥；留空保存不会覆盖，要换就填新的'
  }
  return f.hint ?? ''
}

/* ==================== 读取 ==================== */
async function load() {
  try {
    const r = await api.notify.get()
    const base = blankForm()
    for (const [k, v] of Object.entries(r?.config ?? {})) {
      if (v && typeof v === 'object' && !Array.isArray(v)) {
        base[k] = { ...(base[k] ?? {}), ...(v as Record<string, unknown>) }
      } else if (v !== undefined && v !== null) {
        base[k] = v
      }
    }
    Object.keys(form).forEach((k) => delete form[k])
    Object.assign(form, base)
    // 服务端省略的数值字段补回默认值，避免输入框显示空
    if (!form.http_timeout_sec) form.http_timeout_sec = 15
    if (!form.iyuu_gap_sec) form.iyuu_gap_sec = 5
    if (form.proxy_url == null) form.proxy_url = ''

    channels.value = r?.channels ?? []
    secretSet.value = r?.secret_set ?? {}
    eventLabels.value = r?.event_labels ?? []
    events.value = r?.events ?? []
    autoSync.value = r?.auto_sync ?? true
    snap.value = JSON.stringify(form)
    snapEvents.value = JSON.stringify([events.value, autoSync.value])
    loaded.value = true
  } catch (e: any) {
    toast.error('读取消息配置失败', e?.message)
  } finally {
    loading.value = false
  }
}

/* ==================== 保存 ==================== */
async function saveConfig() {
  saving.value = true
  try {
    const r = await api.notify.save({ config: form })
    channels.value = r?.channels ?? channels.value
    toast.success(
      '配置已保存',
      channels.value.length ? `当前可用渠道：${channels.value.length} 个` : '还没有渠道满足发送条件',
    )
    await load()
  } catch (e: any) {
    toast.error('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

async function saveEvents() {
  savingEvents.value = true
  try {
    await api.notify.save({ config: form, events: events.value, auto_sync: autoSync.value })
    snapEvents.value = JSON.stringify([events.value, autoSync.value])
    toast.success('通知事件已保存', events.value.length ? `订阅 ${events.value.length} 类事件` : '已全部关闭，不会收到任何推送')
  } catch (e: any) {
    toast.error('保存失败', e?.message)
  } finally {
    saving.value = false
  }
}

function toggleEvent(id: string, on: boolean) {
  const set = new Set(events.value)
  if (on) set.add(id)
  else set.delete(id)
  events.value = [...set]
}

/* ==================== 测试 ==================== */
interface TestResult {
  channel: string
  ok: boolean
  skipped?: boolean
  error?: string
}
const testing = ref('')
const testResults = ref<TestResult[]>([])
const testMsg = ref('')
const testAt = ref('')

async function runTest(channelName?: string) {
  const key = channelName ?? ''
  testing.value = key || 'all'
  try {
    const r = await api.notify.test(key ? { channel: key } : {})
    testResults.value = r?.results ?? []
    testMsg.value = r?.message ?? ''
    testAt.value = new Date().toLocaleTimeString()
    if (r?.ok) toast.success('测试消息已发送', testMsg.value)
    else toast.warning('没发出去', testMsg.value)
  } catch (e: any) {
    testResults.value = []
    testMsg.value = e?.message || '测试失败'
    testAt.value = new Date().toLocaleTimeString()
    toast.error('测试失败', testMsg.value)
  } finally {
    testing.value = ''
  }
}

function channelTitle(name: string): string {
  return defs.find((d) => d.name === name)?.title ?? name
}

/* ==================== Telegram 会话探测 ==================== */
interface TGChat {
  id: string
  type: string
  title: string
  username: string
  name: string
  text?: string
}
const tg = reactive({
  busy: false,
  error: '',
  hint: '',
  bot: null as null | { id: number; name: string; username: string },
  chats: [] as TGChat[],
})

const tgTypeLabel: Record<string, string> = {
  private: '私聊',
  group: '群',
  supergroup: '超级群',
  channel: '频道',
}

async function discoverTG() {
  tg.busy = true
  tg.error = ''
  tg.hint = ''
  tg.chats = []
  try {
    const token = String(form.tg?.token ?? '').trim()
    const r = await api.notify.tgDiscover(token)
    tg.bot = r?.bot ?? null
    tg.chats = r?.chats ?? []
    tg.hint = r?.hint ?? ''
    if (r?.webhook) {
      toast.warning('这个 Bot 设了 Webhook', '要先删掉 webhook，消息才会进 getUpdates')
    } else if (tg.chats.length === 1 && !String(form.tg?.chat_id ?? '').trim()) {
      form.tg.chat_id = tg.chats[0].id
      toast.success('已自动填入 Chat ID', '记得点下面的保存')
    } else if (tg.chats.length) {
      toast.success(`找到 ${tg.chats.length} 个会话`, '选一个填进去')
    } else {
      toast.info('还没收到消息', tg.hint)
    }
  } catch (e: any) {
    tg.error = e?.message || '获取失败'
    toast.error('获取 Chat ID 失败', tg.error)
  } finally {
    tg.busy = false
  }
}

function useChat(c: TGChat) {
  form.tg.chat_id = c.id
  toast.info('已填入 Chat ID', `${c.name} · ${c.id}`)
}

onMounted(() => {
  load()
  loadBot()
})
</script>

<template>
  <div class="col" style="gap: 16px">
    <!-- 顶部：当前生效的渠道一眼看完 -->
    <HPanel title="通知渠道" desc="所有渠道共用一套事件，哪个开就推到哪个">
      <template #actions>
        <button class="btn btn-tertiary btn-sm" :disabled="testing !== '' || !channels.length" @click="runTest()">
          <AppIcon name="message" :size="14" />
          {{ testing === 'all' ? '发送中…' : '全部测试' }}
        </button>
      </template>

      <div class="chips">
        <span v-if="!activeInfo.length" class="muted small">
          还没有任何渠道可用 —— 填好下面的必填项并勾选「启用」，保存后这里会出现。
        </span>
        <span v-for="a in activeInfo" :key="a.name" class="chip is-active">
          <AppIcon name="check" :size="12" />{{ a.title }}
        </span>
      </div>

      <div class="chlist">
        <button
          v-for="d in defs"
          :key="d.key"
          class="chrow"
          :class="{ 'is-sel': sel === d.key }"
          @click="sel = d.key"
        >
          <span class="chname">
            <strong>{{ d.title }}</strong>
            <span class="mono muted small">{{ summaryOf(d) }}</span>
          </span>
          <span class="badge" :class="{ 'badge-success': stateOf(d) === 'on', 'badge-warning': stateOf(d) === 'partial' }">
            {{ stateLabel(d) }}
          </span>
          <AppIcon name="edit" :size="14" class="muted" />
        </button>
      </div>
    </HPanel>

    <!-- 选中渠道的配置表单 -->
    <HPanel v-if="loaded" :title="cur.title" :desc="cur.desc">
      <template #actions>
        <button
          v-if="channels.includes(cur.name)"
          class="btn btn-tertiary btn-sm"
          :disabled="testing !== ''"
          @click="runTest(cur.name)"
        >
          <AppIcon name="message" :size="14" />
          {{ testing === cur.name ? '发送中…' : '发送测试' }}
        </button>
      </template>

      <p v-if="!channels.includes(cur.name)" class="pending small">
        <AppIcon name="info" :size="14" />
        这个渠道还没生效：填完必填项保存后，上面的「发送测试」才会亮起来。
      </p>

      <HField label="启用">
        <label class="checkline">
          <HSwitch v-model="form[cur.key].enabled" :label="cur.title" />
          <span class="small">把通知推到这个渠道</span>
        </label>
      </HField>

      <HField
        v-for="f in cur.fields"
        :key="f.key"
        :label="f.label"
        :required="f.required"
        :tip="f.tip"
        :hint="hintOf(cur, f)"
      >
        <select v-if="f.options" v-model="form[cur.key][f.key]" class="select-native">
          <option v-for="o in f.options" :key="o.value" :value="o.value">{{ o.label }}</option>
        </select>
        <textarea
          v-else-if="f.textarea"
          v-model="form[cur.key][f.key]"
          class="textarea mono"
          style="min-height: 84px"
          :placeholder="placeholderOf(cur, f)"
        />
        <input
          v-else
          v-model="form[cur.key][f.key]"
          class="input"
          :class="{ mono: f.mono }"
          :type="f.secret ? 'password' : 'text'"
          autocomplete="new-password"
          :placeholder="placeholderOf(cur, f)"
        />
      </HField>

      <!-- Telegram 专属：Chat ID 不用自己去翻 getUpdates -->
      <div v-if="cur.key === 'tg'" class="tgbox">
        <div class="row-between">
          <div>
            <strong class="small">不知道 Chat ID？</strong>
            <p class="muted small" style="margin-top: 2px">
              先在 Telegram 里给 Bot 发一句「hi」，再点右边的按钮，会自动列出能发消息的会话。
            </p>
          </div>
          <button class="btn btn-tertiary btn-sm" :disabled="tg.busy" @click="discoverTG">
            <AppIcon name="search" :size="14" />
            {{ tg.busy ? '查询中…' : '获取 Chat ID' }}
          </button>
        </div>

        <p v-if="tg.error" class="field-error" style="margin-top: 8px">{{ tg.error }}</p>
        <p v-else-if="tg.hint" class="field-hint" style="margin-top: 8px">{{ tg.hint }}</p>

        <div v-if="tg.bot" class="botline small" style="margin-top: 8px">
          <span class="badge badge-info">{{ tg.bot.username ? '@' + tg.bot.username : 'Bot' }}</span>
          <span class="muted">{{ tg.bot.name }} · ID {{ tg.bot.id }}</span>
        </div>

        <div v-if="tg.chats.length" class="chatlist">
          <div v-for="c in tg.chats" :key="c.id" class="chatrow">
            <div class="grow" style="min-width: 0">
              <div class="row" style="gap: 8px">
                <strong class="small truncate">{{ c.name || c.id }}</strong>
                <span class="badge">{{ tgTypeLabel[c.type] ?? c.type }}</span>
                <span v-if="c.username" class="muted small mono">@{{ c.username }}</span>
              </div>
              <div class="muted small mono truncate">
                {{ c.id }}<template v-if="c.text"> · {{ c.text }}</template>
              </div>
            </div>
            <button class="btn btn-tertiary btn-sm" @click="useChat(c)">用这个</button>
          </div>
        </div>
      </div>

      <template #footer>
        <span v-if="dirty" class="small muted">有未保存的改动</span>
        <button class="btn btn-primary" :disabled="saving" @click="saveConfig">
          {{ saving ? '保存中…' : dirty ? '保存改动' : '保存配置' }}
        </button>
      </template>
    </HPanel>

    <!-- 双向机器人：同一个 Bot，既能收也能发 -->
    <HPanel
      v-if="sel === 'tg' && bot"
      title="机器人接收消息"
      desc="在 Telegram 里发 115 分享 / 磁力 / 片名，直接入库；走的是和网页同一套搜索与转存"
    >
      <template #actions>
        <button class="btn btn-tertiary btn-sm" :disabled="botSaving" @click="loadBot">
          <AppIcon name="search" :size="14" />
          刷新状态
        </button>
      </template>

      <p v-if="!bot.token_set" class="pending small">
        <AppIcon name="info" :size="14" />
        还没填 Bot Token。填好上面的 Token 并保存，机器人会自己起来，不用重启容器。
      </p>
      <p v-else-if="!bot.bound" class="pending small">
        <AppIcon name="info" :size="14" />
        还没绑定会话：在 Telegram 里给这个 Bot 发一句 <code>/start</code>，
        第一个说话的会话会被记成主人（之后别的会话一律拒绝）。
      </p>
      <p v-else class="okline small">
        <AppIcon name="check" :size="14" />
        已绑定 {{ bot.allowed.length }} 个会话，收到消息就会处理
      </p>

      <div v-if="bot.bound" class="chips" style="margin-top: 10px">
        <span v-for="a in bot.allowed" :key="a.chat_id" class="chip is-active mono">{{ a.chat_id }}</span>
      </div>

      <div class="cmdlist">
        <div v-for="c in bot.commands" :key="c.command" class="cmdrow">
          <code>{{ c.command }}</code>
          <span class="muted small">{{ c.desc }}</span>
        </div>
        <div class="cmdrow">
          <code>115.com/s/…</code>
          <span class="muted small">直接转存进网盘，回复文件数与目标目录</span>
        </div>
        <div class="cmdrow">
          <code>magnet:?xt=…</code>
          <span class="muted small">提交 115 离线，下完自动同步并通知</span>
        </div>
        <div class="cmdrow">
          <code>沙丘2</code>
          <span class="muted small">搜资源，结果列成按钮，点一条就入库</span>
        </div>
      </div>

      <HField
        label="允许的会话"
        tip="逗号分隔的 Chat ID；群组是负数。留空解绑后，下一个说话的会话会重新绑定"
        hint="只有这里的会话能用机器人，避免 Bot 被别人拿去转存"
      >
        <input v-model="botIDs" class="input mono" placeholder="123456789, -1009876543210" />
      </HField>

      <p v-if="bot.status" class="small muted" style="margin-top: 2px">最近状态：{{ bot.status }}</p>

      <template #footer>
        <button class="btn btn-primary" :disabled="botSaving" @click="saveBot(false)">
          {{ botSaving ? '保存中…' : '保存白名单' }}
        </button>
        <button class="btn btn-tertiary" :disabled="botSaving || !bot.bound" @click="saveBot(true)">
          解绑全部
        </button>
      </template>
    </HPanel>

    <div class="grid-2">
      <!-- 事件订阅 -->
      <HPanel title="通知事件" desc="按一条片的生命周期排：提交 → 下载 → 整理 → 同步 → 刮削 → 入库 → 字幕 → 删除">
        <div class="list">
          <div v-for="e in eventLabels" :key="e.id" class="item">
            <div class="grow">
              <strong class="small">{{ e.label }}</strong>
              <p class="muted small">{{ e.desc }}</p>
            </div>
            <HSwitch
              :model-value="events.includes(e.id)"
              :label="e.label"
              @update:model-value="(v: boolean) => toggleEvent(e.id, v)"
            />
          </div>
          <div class="item">
            <div class="grow">
              <strong class="small">离线完成自动同步</strong>
              <p class="muted small">离线任务下完，自动给对应媒体库生成 STRM</p>
            </div>
            <HSwitch v-model="autoSync" label="离线完成自动同步" />
          </div>
        </div>
        <template #footer>
          <span v-if="eventsDirty" class="small muted">有未保存的改动</span>
          <button class="btn btn-primary" :disabled="savingEvents" @click="saveEvents">
            {{ savingEvents ? '保存中…' : '保存事件' }}
          </button>
        </template>
      </HPanel>

      <!-- 高级：代理与超时 -->
      <HPanel title="发送参数" desc="出海部署时 Telegram 必须走代理">
        <HField label="HTTP 代理" tip="形如 http://192.168.1.10:7890；企业微信 / 钉钉这类国内接口也可以走同一个代理" hint="留空直连">
          <input v-model="form.proxy_url" class="input mono" placeholder="http://127.0.0.1:7890" />
        </HField>
        <HField label="请求超时（秒）" hint="默认 15 秒，网络慢可以调大">
          <input v-model.number="form.http_timeout_sec" class="input mono" type="number" min="3" max="120" />
        </HField>
        <HField label="IYUU 发送间隔（秒）" hint="IYUU 连发会被拒，默认 5 秒">
          <input v-model.number="form.iyuu_gap_sec" class="input mono" type="number" min="0" max="60" />
        </HField>
        <template #footer>
          <button class="btn btn-primary" :disabled="saving" @click="saveConfig">保存参数</button>
        </template>
      </HPanel>
    </div>

    <!-- 测试结果 -->
    <HPanel v-if="testResults.length || testMsg" title="最近一次测试" :desc="testAt ? `发送于 ${testAt}` : ''">
      <p class="small" :class="testResults.some((r) => r.ok) ? '' : 'muted'">{{ testMsg }}</p>
      <div v-if="testResults.length" class="table-wrap" style="margin-top: 10px">
        <table class="table">
          <thead>
            <tr>
              <th>渠道</th>
              <th style="width: 90px">结果</th>
              <th>说明</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in testResults" :key="r.channel">
              <td>{{ channelTitle(r.channel) }}</td>
              <td>
                <span class="badge" :class="r.ok ? 'badge-success' : r.skipped ? '' : 'badge-danger'">
                  {{ r.ok ? '成功' : r.skipped ? '跳过' : '失败' }}
                </span>
              </td>
              <td class="small muted" style="white-space: pre-wrap">{{ r.error || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else class="muted small" style="margin-top: 8px">
        没有发出去的渠道。测试用的是「已保存」的配置，改完记得先保存。
      </p>
    </HPanel>
  </div>
</template>

<style scoped>
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.chlist {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.chrow {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface);
  color: inherit;
  cursor: pointer;
  text-align: left;
  transition: border-color 0.15s, background 0.15s;
}
.chrow:hover {
  border-color: var(--border-secondary);
}
.chrow.is-sel {
  border-color: var(--accent);
  background: var(--accent-soft);
}
.chname {
  display: flex;
  flex-direction: column;
  gap: 2px;
  flex: 1;
  min-width: 0;
}
.chname .mono {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
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
.checkline {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
/* 机器人能收什么：命令行式列出，比一段散文好扫 */
.okline {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--success);
}
.cmdlist {
  margin: 12px 0 4px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.cmdrow {
  display: flex;
  align-items: baseline;
  gap: 10px;
  padding: 4px 0;
}
.cmdrow code {
  flex: none;
  min-width: 132px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--accent);
}
.tgbox {
  margin-top: 4px;
  padding: 12px;
  border: 1px dashed var(--border-secondary);
  border-radius: var(--r-md);
  background: var(--surface-secondary);
}
.chatlist {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 10px;
  max-height: 260px;
  overflow: auto;
}
.chatrow {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
}
.botline {
  display: flex;
  align-items: center;
  gap: 8px;
}
</style>
