/**
 * 后端通信层。
 *
 * 约定：
 * - 所有接口都在 /api 下，Cookie 会话（sid），不用 Token。
 * - 401 时抛 AuthError，由 router 统一弹回登录页，页面里不用各写一遍。
 * - 页面还没接后端时用 mock 数据，接的时候把 mock 换成这里的函数即可。
 */

export class ApiError extends Error {
  status: number
  payload: unknown
  constructor(message: string, status: number, payload?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

export class AuthError extends ApiError {
  constructor(message = '登录已过期') {
    super(message, 401)
    this.name = 'AuthError'
  }
}

type Query = Record<string, string | number | boolean | undefined | null>

export interface RequestOptions {
  query?: Query
  body?: unknown
  /** 传 FormData 时置 true，让浏览器自己带 boundary */
  raw?: boolean
  signal?: AbortSignal
  timeout?: number
}

const BASE = '/api'

function withQuery(path: string, query?: Query): string {
  if (!query) return BASE + path
  const usp = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue
    usp.set(k, String(v))
  }
  const qs = usp.toString()
  return BASE + path + (qs ? `?${qs}` : '')
}

/** 会话过期时广播，App 层监听后跳登录页 */
export const authBus = new EventTarget()

export async function request<T = any>(
  method: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH',
  path: string,
  opts: RequestOptions = {},
): Promise<T> {
  const ctl = new AbortController()
  const timer = opts.timeout ? setTimeout(() => ctl.abort(), opts.timeout) : null
  const onAbort = () => ctl.abort()
  opts.signal?.addEventListener('abort', onAbort)

  const headers: Record<string, string> = { Accept: 'application/json' }
  let body: BodyInit | undefined

  if (opts.body !== undefined) {
    if (opts.body instanceof FormData || opts.raw) {
      body = opts.body as BodyInit
    } else {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(opts.body)
    }
  }

  try {
    const resp = await fetch(withQuery(path, opts.query), {
      method,
      headers,
      body,
      credentials: 'same-origin',
      signal: ctl.signal,
    })

    if (resp.status === 401) {
      let msg = '登录已过期'
      try {
        const data = await resp.json()
        msg = data?.error || data?.message || msg
      } catch {
        /* 响应不是 JSON，用默认文案 */
      }
      if (msg === '未登录' || msg === '登录已过期') {
        authBus.dispatchEvent(new Event('expired'))
        throw new AuthError(msg)
      }
      throw new ApiError(msg, 401)
    }

    const text = await resp.text()
    const data = text ? safeJSON(text) : null

    if (!resp.ok) {
      const msg =
        (data && (data.error || data.message)) || `请求失败（HTTP ${resp.status}）`
      throw new ApiError(String(msg), resp.status, data)
    }
    return data as T
  } finally {
    if (timer) clearTimeout(timer)
    opts.signal?.removeEventListener('abort', onAbort)
  }
}

function safeJSON(text: string): any {
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

export const http = {
  get: <T = any>(p: string, q?: Query, o?: Omit<RequestOptions, 'query'>) =>
    request<T>('GET', p, { ...o, query: q }),
  post: <T = any>(p: string, body?: unknown, o?: Omit<RequestOptions, 'body'>) =>
    request<T>('POST', p, { ...o, body }),
  put: <T = any>(p: string, body?: unknown, o?: Omit<RequestOptions, 'body'>) =>
    request<T>('PUT', p, { ...o, body }),
  del: <T = any>(p: string, o?: RequestOptions) => request<T>('DELETE', p, o),
  patch: <T = any>(p: string, body?: unknown, o?: Omit<RequestOptions, 'body'>) =>
    request<T>('PATCH', p, { ...o, body }),
}
