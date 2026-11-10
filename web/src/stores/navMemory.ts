/**
 * 「影视详情页该退回哪」的记忆。
 *
 * 详情页以前写死返回「发现与订阅」，可它其实有四个入口：资源搜索结果、
 * 平台海报墙、发现与订阅的认片、播出日历。从搜索结果点进去、返回却被扔到
 * 另一个页面（而且那边的结果早没了），用户只能重新搜一遍。
 *
 * 这里只记两件事：来时的完整地址（含 ?q= 这类查询串）、以及按钮上该写的字。
 * 详情页只用它决定「退到哪、按钮叫什么」，真正的状态由目标页面自己保
 * （资源搜索页有 KeepAlive，地址里的 q 就是那一次搜索）。
 */
import { ref } from 'vue'

export interface NavOrigin {
  /** 来时的完整路径，例如 /search?q=沙丘2&en=Dune */
  path: string
  /** 返回按钮上的文案，例如「返回搜索结果」 */
  label: string
}

export const titleOrigin = ref<NavOrigin | null>(null)

/** 进详情页之前记一下来路。 */
export function rememberOrigin(path: string, label: string): void {
  titleOrigin.value = path ? { path, label } : null
}

/** 详情页自己用完就清，免得下一次从别处进来还带着上一次的来路。 */
export function clearOrigin(): void {
  titleOrigin.value = null
}
