/** 侧边栏与路由共用的页面元信息 */
export interface NavItem {
  name: string
  path: string
  label: string
  desc: string
  icon: string
  group?: string
  /** 功能开关没打开时不显示（对齐 StrmStation 的 feature 标记） */
  feature?: string
  hideInNav?: boolean
}

export const navGroups: { label: string; items: NavItem[] }[] = [
  {
    label: '概览',
    items: [
      {
        name: 'dashboard',
        path: '/',
        label: '总览面板',
        desc: '最新入库 · 媒体库 · 容量与任务',
        icon: 'dashboard',
      },
    ],
  },
  {
    label: '媒体库',
    items: [
      {
        name: 'accounts',
        path: '/accounts',
        label: '账号与媒体库',
        desc: '管理 115 账号与媒体库位置',
        icon: 'accounts',
      },
      {
        name: 'media-transfer',
        path: '/media-transfer',
        label: '发现与订阅',
        desc: '找片 · 播出日历 · 订阅追更 · 链接转存',
        icon: 'download',
      },
      {
        name: 'search',
        path: '/search',
        label: '资源搜索',
        desc: '按片名搜种子站与 TG 频道 · 认准了直接入库',
        icon: 'search',
      },
      {
        name: 'sync',
        path: '/sync',
        label: 'Strm 管理',
        desc: '同步状态 · 配置 · 全量 · 增量 · 孤儿清理',
        icon: 'sync',
      },
      {
        name: 'organize',
        path: '/organize',
        label: '自动整理',
        desc: '扫描预览 · 批量移动 · 整理台账',
        icon: 'organize',
      },
      {
        name: 'library',
        path: '/library',
        label: '影视库',
        desc: '海报墙 · 刮削与字幕',
        icon: 'local',
      },
      {
        name: 'files',
        path: '/files',
        label: '网盘文件',
        desc: '读你登录的 115 网盘 · 移动 / 入库 / 删除',
        icon: 'files',
      },
      {
        name: 'local-files',
        path: '/local-files',
        label: '本地文件',
        desc: '宿主 /media 镜像 · 体积统计 · 删除残留',
        icon: 'folder',
      },
    ],
  },
  {
    label: '系统',
    items: [
      {
        name: 'tasks',
        path: '/tasks',
        label: '任务中心',
        desc: '任务队列与历史 · 整理记录',
        icon: 'tasks',
      },
      {
        name: 'settings',
        path: '/settings',
        label: '系统配置',
        desc: 'TMDB · 代理 · EMBY 配置',
        icon: 'settings',
      },
      {
        name: 'message',
        path: '/message',
        label: '消息配置',
        desc: 'Telegram · 企微 · 钉钉 · 飞书 · Webhook',
        icon: 'message',
      },
      {
        name: 'logs',
        path: '/logs',
        label: '实时日志',
        desc: '同步与整理操作的服务端与本地日志',
        icon: 'logs',
        hideInNav: true,
      },
    ],
  },
]

export const navFlat: NavItem[] = navGroups.flatMap((g) => g.items)

export function findNav(name: string): NavItem | undefined {
  return navFlat.find((n) => n.name === name)
}
