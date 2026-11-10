import type { Component } from 'vue'
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { navFlat } from '@/data/nav'
import { refreshSession, session } from '@/stores/session'

// 页面全部静态引入，合成同一个 JS 包。
//
// 这里曾经是按需 import（每个页面一个 18~88KB 的 chunk），局域网里没感觉，
// 但走 Cloudflare 隧道时单次往返 0.5~1.5 秒 —— vue-router 必须等目标页面的
// chunk 下载完才切页，表现出来就是「点左侧菜单，过一会画面才变」。预取只是
// 把这段等待挪到后台，第一次点还是可能扑空。
//
// 合成一个包后首屏多下 100 多 KB（gzip 后），之后所有页面切换都在本地完成，
// 点一下就到。这是用户能直接感知的差别，值得把首屏稍微换长一点。
import DashboardPage from '@/pages/DashboardPage.vue'
import AccountsPage from '@/pages/AccountsPage.vue'
import MediaTransferPage from '@/pages/MediaTransferPage.vue'
import SyncPage from '@/pages/SyncPage.vue'
import OrganizePage from '@/pages/OrganizePage.vue'
import LibraryPage from '@/pages/LibraryPage.vue'
import FilesPage from '@/pages/FilesPage.vue'
import LocalFilesPage from '@/pages/LocalFilesPage.vue'
import TasksPage from '@/pages/TasksPage.vue'
import SettingsPage from '@/pages/SettingsPage.vue'
import MessagePage from '@/pages/MessagePage.vue'
import LogsPage from '@/pages/LogsPage.vue'
import SearchPage from '@/pages/SearchPage.vue'
import LoginPage from '@/pages/LoginPage.vue'
import TitlePage from '@/pages/TitlePage.vue'

const pages: Record<string, Component> = {
  dashboard: DashboardPage,
  accounts: AccountsPage,
  'media-transfer': MediaTransferPage,
  search: SearchPage,
  sync: SyncPage,
  organize: OrganizePage,
  library: LibraryPage,
  files: FilesPage,
  'local-files': LocalFilesPage,
  tasks: TasksPage,
  settings: SettingsPage,
  message: MessagePage,
  logs: LogsPage,
}

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: LoginPage,
    meta: { public: true, title: '登录', desc: '登录 StrmStation' },
  },
  ...navFlat.map<RouteRecordRaw>((n) => ({
    path: n.path,
    name: n.name,
    component: pages[n.name],
    meta: { title: n.label, desc: n.desc, icon: n.icon },
  })),
  // 老地址：/scrape 和 /local 合成 /library 了。直接 302 过去并带上对应标签，
  // 书签、浏览记录、别人发过的链接都还能落到对的地方，不是 404 也不是首页。
  { path: '/scrape', redirect: { path: '/library', query: { tab: 'scrape' } } },
  { path: '/local', redirect: '/library' },
  // 影视详情是独立页面（不在侧栏里）：认片搜索、平台海报墙点进来先看片子，
  // 详情页里再决定要不要「找资源」。
  {
    path: '/title',
    name: 'title',
    component: TitlePage,
    meta: { title: '影视详情', desc: '海报 · 简介 · 分季 · 入库与订阅状态' },
  },
  { path: '/:pathMatch(.*)*', redirect: '/' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // 返回时优先恢复原来的滚动位置：资源搜索页靠 KeepAlive 保住结果，
  // 如果这里强行 top:0，回来会看到「结果还在，但滚回顶部」。
  scrollBehavior: (_to, _from, savedPosition) => savedPosition || { top: 0 },
})

router.beforeEach(async (to) => {
  if (to.meta.public) return true

  if (!session.ready) await refreshSession()

  if (session.setupNeeded) {
    return { name: 'login', query: { setup: '1' } }
  }
  if (!session.authed) {
    return { name: 'login', query: to.fullPath !== '/' ? { redirect: to.fullPath } : {} }
  }
  return true
})

router.afterEach((to) => {
  const title = (to.meta.title as string) || 'StrmStation'
  document.title = `${title} · StrmStation`
})
