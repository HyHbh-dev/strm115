/**
 * 前端构建版本。
 *
 * 由 vite.config.ts 在构建时读 VERSION 注入。它表示「这份 JS 是哪一版编出来的」，
 * 和服务端 /api/session 返回的版本是两回事：页面开着不刷新时，后端已经升到
 * 新版本，而手里这份 JS 还是旧的。侧栏必须显示前者，否则用户看到 v0.4.3
 * 会以为页面也更新了（实际是旧界面在跑）。
 */
declare const __APP_VERSION__: string

export const APP_VERSION = typeof __APP_VERSION__ === 'string' ? __APP_VERSION__ : ''
