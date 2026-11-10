import { reactive } from 'vue'

export type Tone = 'default' | 'success' | 'warning' | 'danger' | 'accent'

export interface ToastItem {
  id: number
  text: string
  tone: Tone
  desc?: string
  ttl: number
}

export const toasts = reactive<ToastItem[]>([])

let seq = 1

export function pushToast(text: string, tone: Tone = 'default', desc?: string, ttl = 4200) {
  const id = seq++
  toasts.push({ id, text, tone, desc, ttl })
  if (ttl > 0) setTimeout(() => dismissToast(id), ttl)
  return id
}

export function dismissToast(id: number) {
  const i = toasts.findIndex((t) => t.id === id)
  if (i >= 0) toasts.splice(i, 1)
}

export const toast = {
  info: (text: string, desc?: string) => pushToast(text, 'default', desc),
  success: (text: string, desc?: string) => pushToast(text, 'success', desc),
  warning: (text: string, desc?: string) => pushToast(text, 'warning', desc),
  error: (text: string, desc?: string) => pushToast(text, 'danger', desc),
  accent: (text: string, desc?: string) => pushToast(text, 'accent', desc),
}
