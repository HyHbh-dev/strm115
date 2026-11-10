import { computed, reactive } from 'vue'
import { ApiError } from '@/api/client'
import { api } from '@/api/endpoints'

/**
 * 会话状态。真源是 /api/session：刷新页面、改密码、服务重启后都以它为准，
 * 前端不复刻一份「登录过」的缓存，避免出现「页面能点、接口全 401」。
 */
export const session = reactive({
  ready: false,
  setupNeeded: false,
  authed: false,
  user: '',
  version: '',
  uptimeSec: 0,
  error: '' as string,
})

export const isAuthed = computed(() => session.authed)

export async function refreshSession(): Promise<boolean> {
  try {
    const r = await api.auth.session()
    session.setupNeeded = !!r?.setup_needed
    session.authed = !!r?.authed
    session.user = r?.user ?? ''
    session.version = r?.version ?? ''
    session.uptimeSec = Number(r?.uptime_sec ?? 0)
    session.error = ''
  } catch (e) {
    session.authed = false
    session.error = e instanceof Error ? e.message : '无法连接服务'
  } finally {
    session.ready = true
  }
  return session.authed
}

export async function login(username: string, password: string, otp?: string) {
  await api.auth.login(username, password, otp)
  await refreshSession()
}

export async function runSetup(username: string, password: string) {
  await api.auth.setup(username, password)
  await refreshSession()
}

export async function logout() {
  try {
    await api.auth.logout()
  } catch {
    /* 服务端会话可能已经失效，忽略 */
  }
  session.authed = false
}

/** 把接口错误翻成人话，页面里统一用这个提示 */
export function errText(e: unknown, fallback = '操作失败'): string {
  if (e instanceof ApiError) return e.message || fallback
  if (e instanceof Error) {
    if (e.name === 'AbortError') return '请求超时'
    return e.message || fallback
  }
  return fallback
}
