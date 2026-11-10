import { ref, watch } from 'vue'
import { http } from '@/api/client'
import { session } from './session'

export interface NotificationItem {
  id: number
  event: string
  kind: string
  title: string
  content: string
  link: string
  level: string
  read_at: string
  created_at: string
}

// 跟 toast.ts 一样：模块级 ref 单例 + 普通函数，不引 Pinia。
export const items = ref<NotificationItem[]>([])
export const unread = ref(0)
export const panelOpen = ref(false)
export const loading = ref(false)

let timer: ReturnType<typeof setInterval> | null = null
let inFlight: Promise<void> | null = null

export function loadNotifications(): Promise<void> {
  // 登录页 / 会话还没确认时别打这个接口：它和别的 /api 一样要鉴权，
  // 抢跑只会得到一条 401 并污染控制台。
  if (!session.authed) return Promise.resolve()
  // 30 秒轮询、会话 watch、鼠标滑过展开可能撞在同一秒。走公网隧道时一条
  // 请求几秒没回来又叠一条，点右侧就会明显发涩——同一时刻只留一条在飞。
  if (inFlight) return inFlight
  loading.value = true
  const task = (async () => {
    try {
      const data = await http.get<{ items: NotificationItem[]; unread: number }>(
        '/notifications',
        { limit: 50 },
      )
      items.value = Array.isArray(data?.items) ? data.items : []
      unread.value = data?.unread ?? 0
    } catch {
      // 站内通知是旁路能力，请求失败静默吞掉，不弹 toast 打扰用户。
    } finally {
      loading.value = false
    }
  })()
  inFlight = task
  void task.then(() => { if (inFlight === task) inFlight = null })
  return task
}

// 会话从「未登录」变成「已登录」时立刻拉一次，角标不用等 30 秒。
// immediate 让刷新页面时（守卫已 refreshSession 过）也走同一条路。
watch(
  () => session.authed,
  (v) => { if (v) void loadNotifications() },
  { immediate: true },
)

export async function markRead(ids?: number[]) {
  try {
    const data = await http.post<{ ok: boolean; unread: number }>('/notifications/read', {
      ids: ids ?? [],
      all: false,
    })
    const set = new Set(ids ?? [])
    for (const it of items.value) {
      if (set.has(it.id) && !it.read_at) it.read_at = new Date().toISOString()
    }
    if (typeof data?.unread === 'number') unread.value = data.unread
  } catch {
    // 静默
  }
}

export async function markAllRead() {
  try {
    const data = await http.post<{ ok: boolean; unread: number }>('/notifications/read', {
      all: true,
    })
    for (const it of items.value) {
      if (!it.read_at) it.read_at = new Date().toISOString()
    }
    unread.value = typeof data?.unread === 'number' ? data.unread : 0
  } catch {
    // 静默
  }
}

export function startNotificationPolling() {
  if (timer) return // 避免重复启动（热更新、多个 Topbar 实例）
  // 首次拉取交给上面的 watch；这里只负责「登录后保持新鲜」。
  timer = setInterval(() => {
    if (document.hidden) return // 页面不可见时跳过，后台标签页不空转
    void loadNotifications()
  }, 30_000)
}

export function stopNotificationPolling() {
  if (timer) {
    clearInterval(timer)
    timer = null
  }
}
