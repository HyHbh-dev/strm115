/**
 * 全站共用的格式化 / 剪贴板小工具。
 * 之前 pad2 / fmtSize / 时间格式化 / 复制提示在多个组件里各写一份，口径还略有
 * 出入，这里收成一处；行为按各调用方原来的边界值对齐，避免动到现有 UI。
 */
import { toast } from '@/stores/toast'

/** 个位数补 0：水印线 S01E01 这类文案用。与三份旧实现逐字一致。 */
export function pad2(n: number): string {
  return n < 10 ? `0${n}` : `${n}`
}

/**
 * 体积：1024 进率，保留 1 位小数；>=100 或字节级不保留小数。
 * 空值口径沿用三处旧实现：null / undefined / 0 / 负数 / NaN 一律返回 '—'
 * （不是空串也不是 '-'），表格里就不会冒出 "0 B" / "NaN B"，和现有 UI 一致。
 */
export function fmtSize(n?: number | null): string {
  if (n == null || !(n > 0)) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

/** 把 string / 时间戳 / Date 统一转成 Date；Date 走 getTime() 绕开构造重载的类型限制。 */
function toDate(v: string | number | Date): Date {
  return new Date(v instanceof Date ? v.getTime() : v)
}

/**
 * 完整日期时间 yyyy-MM-dd HH:mm（本地时间）。
 * 空值 / 无效日期 / 2000 年前一律 '—'：对齐账号页原来的脏时间戳过滤。
 */
export function fmtDate(v?: string | number | Date): string {
  if (v == null || v === '') return '—'
  const d = toDate(v)
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—'
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`
}

/**
 * 月日 + 时分 MM-dd HH:mm（通知栏用，同年内不带年份省空间）。
 * 空值返回 ''、无效时退回原串，这两点和通知栏旧实现一致，别硬抹平成 fmtDate。
 */
export function fmtClock(v?: string | number | Date): string {
  if (v == null || v === '') return ''
  const d = toDate(v)
  if (Number.isNaN(d.getTime())) return typeof v === 'string' ? v : ''
  return `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`
}

/** 纯日期 yyyy-MM-dd：日历拿它拼 /api/calendar 的 start 参数，不能带时分。 */
export function fmtDay(v?: string | number | Date): string {
  if (v == null || v === '') return ''
  const d = toDate(v)
  if (Number.isNaN(d.getTime())) return ''
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
}

/**
 * 复制到剪贴板。成功 / 失败提示统一在这里发，各页面只把特有的成功文案传进来。
 * 返回布尔值，调用方需要判断结果时可用。
 */
export async function copyText(text: string, okMsg = '已复制'): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(okMsg)
    return true
  } catch {
    toast.warning('复制失败，请手动复制')
    return false
  }
}
