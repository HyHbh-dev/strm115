/**
 * 接口清单（按域分组）。
 *
 * 这份文件是「外壳」与后端之间的契约：函数名、参数、返回都按 StrmStation 的
 * 接口形状写好，页面接逻辑时把 mock 换成这里的调用即可。
 *
 * 标注说明：
 *   ✅ 本项目 Go 后端已经实现（internal/httpapi 里有对应路由）
 *   🚧 后端尚未实现，前端先留空壳；接上之前调用会 404
 */

import { http } from './client'

const q = (v: boolean | undefined) => (v ? 1 : undefined)

export const api = {
  /* ---------------- 会话 ---------------- */
  auth: {
    /** ✅ GET /api/session → {setup_needed, authed, user, version, uptime_sec} */
    session: () => http.get('/session'),
    /** ✅ POST /api/login {username,password} */
    login: (username: string, password: string, otp?: string) =>
      http.post('/login', { username, password, otp }),
    /** ✅ POST /api/setup 首次设置管理员 */
    setup: (username: string, password: string) => http.post('/setup', { username, password }),
    /** ✅ POST /api/logout */
    logout: () => http.post('/logout'),
    /** ✅ POST /api/credentials 改账号/密码 */
    changeCreds: (current_password: string, username: string, password: string) =>
      http.post('/credentials', { current_password, username, password }),
  },

  /* ---------------- 概览 ---------------- */
  dashboard: {
    /**
     * ✅ GET /api/dashboard —— 库 / 台账 / 刮削统计 / 在跑的任务 / 最新入库
     *
     * Emby 的连通性不在这个接口里（探活要走外部 HTTP，会拖慢首屏），
     * 页面并行调 config.serverStatus() 再合并。
     */
    overview: (refresh?: boolean) => http.get('/dashboard', { refresh: q(refresh) }),
    /** ✅ GET /api/instance 服务自身信息（版本、运行时长、路径） */
    instance: () => http.get('/instance'),
  },

  /* ---------------- 115 账号 / 媒体库 ---------------- */
  storage: {
    /** ✅ GET /api/accounts → [{id,kind,name,created_at}] */
    accounts: () => http.get('/accounts'),
    /** ✅ DELETE /api/accounts/{id} */
    removeAccount: (id: number) => http.del(`/accounts/${id}`),
    /** ✅ GET /api/accounts/{id}/stats 请求/重试/风控/冷却 */
    accountStats: (id: number) => http.get(`/accounts/${id}/stats`),
    /** ✅ GET /api/accounts/{id}/browse?cid=0 列网盘目录（也用来探活凭据） */
    browse: (id: number, cid = '0') => http.get(`/accounts/${id}/browse`, { cid }),
    /** ✅ POST /api/115/qr → {id,png,expires_in} */
    qrStart: () => http.post('/115/qr'),
    /**
     * ✅ GET /api/115/qr/{id} → {status,msg,account?}
     *
     * 115 的状态接口是长轮询：没扫码时它会挂到几十秒才回「等待扫码」，
     * 所以这里给个客户端超时，超时由调用方决定要不要继续下一轮。
     */
    qrPoll: (id: string, timeout = 40000) => http.get(`/115/qr/${id}`, undefined, { timeout }),
    /** ✅ POST /api/115/cookie {cookie,name} → 账号 */
    cookieLogin: (cookie: string, name?: string) => http.post('/115/cookie', { cookie, name }),
    /**
     * ✅ POST /api/115/share 把一条 115 分享转存到自己的网盘
     *
     * url / share_code 二选一，后端自己会从链接里认出分享码与提取码；
     * cid 是目标目录，留空则用设置项 res_save_cid（资源搜索页可改）。
     */
    saveShare: (body: { url?: string; share_code?: string; password?: string; cid?: string; account_id?: number }) =>
      http.post('/115/share', body, { timeout: 120_000 }),
    /** 🚧 容量统计（后端暂无空间接口，只有请求侧统计） */
    stats: () => http.get('/storage'),
  },

  /* ---------------- 夸克（TV 播放 + 网页转存） ---------------- */
  quark: {
    /**
     * ✅ POST /api/accounts/quarktv/qr → {id, png(data:image/jpeg), expires_in}
     *
     * 夸克TV 扫码登录。png 里是 JPEG 的 base64，前缀也一并给好了，直接塞 <img src>。
     */
    qrStart: () => http.post('/accounts/quarktv/qr'),
    /** ✅ GET /api/accounts/quarktv/qr/{id} → {status:'waiting'|'ok'|'expired', account?} */
    qrPoll: (id: string) => http.get(`/accounts/quarktv/qr/${id}`, undefined, { timeout: 30000 }),
    /**
     * ✅ POST /api/accounts/{id}/quark-cookie {cookie}
     *
     * 只给「转存分享」用：夸克TV 接口没有转存能力，网页 Cookie 才有。
     * 会话字段 __puus 会轮换，后端每 ~100 分钟自动续期并写回账号，
     * 只要服务不停就不用重贴；真失效时转存会明确报错。
     */
    setCookie: (id: number, cookie: string) => http.post(`/accounts/${id}/quark-cookie`, { cookie }, { timeout: 60_000 }),
    /** ✅ GET /api/quark/keepalive 每个夸克账号的 Cookie 保活状态 */
    keepalive: () => http.get('/quark/keepalive'),
    /** ✅ POST /api/quark/keepalive 立刻续一次期（并返回最新状态） */
    keepaliveNow: () => http.post('/quark/keepalive', {}, { timeout: 60_000 }),
    /** ✅ POST /api/accounts/quarktv/web-qr 网页 Cookie 扫码登录（返回 PNG data URI） */
    webQrStart: (accountId: number) =>
      http.post('/accounts/quarktv/web-qr', { account_id: accountId }, { timeout: 30_000 }),
    /** ✅ GET /api/accounts/quarktv/web-qr/{id} → waiting / ok / expired */
    webQrPoll: (id: string) =>
      http.get(`/accounts/quarktv/web-qr/${id}`, undefined, { timeout: 30_000 }),
    /** ✅ POST /api/quark/test {account_id} → {ok, tv_ok?, web_ok?, tv_error?, web_error?} */
    test: (accountId?: number) => http.post('/quark/test', { account_id: accountId }),
    /**
     * ✅ POST /api/quark/share 把一条夸克分享转存进夸克网盘
     *
     * url / share_code 二选一；fid 留空用设置项 quark_save_fid（0 = 根目录）。
     * 转存是异步的，后端会等到任务完成再回，所以要给足超时。
     */
    save: (body: { url?: string; share_code?: string; passcode?: string; fid?: string; account_id?: number }) =>
      http.post('/quark/share', body, { timeout: 240_000 }),
  },

  libraries: {
    /** ✅ GET /api/libraries */
    list: () => http.get('/libraries'),
    /** ✅ POST /api/libraries */
    create: (body: unknown) => http.post('/libraries', body),
    /** 🚧 PUT /media-libs/{id} */
    update: (id: number, body: unknown) => http.put(`/media-libs/${id}`, body),
    /** ✅ DELETE /api/libraries/{id} */
    remove: (id: number) => http.del(`/libraries/${id}`),
    /** ✅ PUT /api/libraries/{id}/keep-ext 直链是否带扩展名，下一轮同步生效 */
    setKeepExt: (id: number, keep: boolean) => http.put(`/libraries/${id}/keep-ext`, { keep_ext: keep }),
    /**
     * ✅ 一个库挂多个网盘目录（115 + 夸克）
     *
     * 两边的 .strm 写进同一个本地目录，Emby 只认一个库；差别只在文件里的
     * 302 地址（115 是 /d/{pickcode}，夸克是 /d/q/{fid}）。
     */
    sources: {
      list: (id: number) => http.get(`/libraries/${id}/sources`),
      add: (id: number, body: { account_id: number; remote_cid: string; remote_path?: string }) =>
        http.post(`/libraries/${id}/sources`, body),
      update: (id: number, sid: number, body: { remote_cid: string; remote_path?: string }) =>
        http.put(`/libraries/${id}/sources/${sid}`, body),
      remove: (id: number, sid: number) => http.del(`/libraries/${id}/sources/${sid}`),
    },
    /**
     * ✅ GET /api/libraries/{id}/reldir?cid= 网盘目录相对库根的路径
     *
     * 「加入媒体库」时用它算落盘目录。不要在前端按 remote_path 剥前缀：
     * 同名目录（电影/电影/…）剥错一次，.strm 就会落在两个地方。
     */
    relDir: (id: number, cid: string) =>
      http.get(`/libraries/${id}/reldir`, { cid }, { timeout: 90_000 }),
    /** 🚧 POST /media-library/calibrate 容量/路径校准 */
    calibrate: (apply: boolean) => http.post('/media-library/calibrate', { apply }),
    /** 🚧 GET /library/leftovers 本地残留 */
    leftovers: () => http.get('/library/leftovers'),
    /** 🚧 POST /library/leftovers/clean */
    cleanLeftover: (name: string) => http.post('/library/leftovers/clean', { name }),
  },

  /* ---------------- STRM 同步 ---------------- */
  sync: {
    /**
     * ✅ POST /api/libraries/{id}/sync 触发单个库同步
     *
     * force=true 走全量语义：内容没变也重写 .strm，Emby 会整库重扫；
     * 不传则是增量：只补新增与变化的，删失效由 delete_stale 控制。
     */
    start: (id: number, body?: { force?: boolean; delete_stale?: boolean; min_size?: number }) =>
      http.post(`/libraries/${id}/sync`, body ?? {}),
    /** ✅ GET /api/libraries/{id}/job */
    job: (id: number) => http.get(`/libraries/${id}/job`),
    /** ✅ POST /api/libraries/{id}/cancel */
    cancel: (id: number) => http.post(`/libraries/${id}/cancel`),
    /** 🚧 GET /sync/capabilities 能力探测（115 接口/权限） */
    capabilities: () => http.get('/sync/capabilities'),
    /** 🚧 POST /sync/full 全量同步 */
    full: (body: unknown) => http.post('/sync/full', body),
    /** 🚧 POST /sync/incremental */
    incremental: (body: unknown) => http.post('/sync/incremental', body),
    /** 🚧 GET /sync/incr-status */
    incrStatus: () => http.get('/sync/incr-status'),
    /** 🚧 GET /sync/incr-rounds */
    incrRounds: () => http.get('/sync/incr-rounds'),
    /** 🚧 GET /sync/incr-events */
    incrEvents: () => http.get('/sync/incr-events'),
    /** 🚧 POST /sync/incr-probe */
    incrProbe: () => http.post('/sync/incr-probe'),
    /** 🚧 POST /sync/adopt/preview 接管已有 STRM 预览 */
    adoptPreview: (body: unknown) => http.post('/sync/adopt/preview', body),
    /** 🚧 POST /sync/adopt/apply */
    adoptApply: (body: unknown) => http.post('/sync/adopt/apply', body),
    /** 🚧 GET /sync/orphans 台账里指向已删除文件的 STRM */
    orphans: () => http.get('/sync/orphans'),
    /** 🚧 POST /sync/orphans/clean */
    cleanOrphans: (body: unknown) => http.post('/sync/orphans/clean', body),
    /** 🚧 POST /sync/strm-rebase/preview 换 115 账号后改写地址 */
    rebasePreview: (body: unknown) => http.post('/sync/strm-rebase/preview', body),
    /** 🚧 POST /sync/strm-rebase */
    rebase: (body: unknown) => http.post('/sync/strm-rebase', body),
    /** 🚧 GET /sync/deep-delete/records */
    deepDeleteRecords: () => http.get('/sync/deep-delete/records'),
    /** 🚧 POST /sync/cron-preview 定时表达式预览 */
    cronPreview: (cron: string) => http.post('/sync/cron-preview', { cron }),
  },

  /* ---------------- 自动整理 ---------------- */
  organize: {
    /** ✅ GET /api/organize/config —— 配置 + 账号 + 媒体库 + 台账统计 */
    config: () => http.get('/organize/config'),
    /** ✅ PUT /api/organize/config */
    saveConfig: (config: unknown) => http.put('/organize/config', { config }),
    /** ✅ POST /api/organize/scan —— 只看不动，返回「会怎么整理」 */
    scan: () => http.post('/organize/scan', {}),
    /** ✅ POST /api/organize/run —— 真移动；不传 file_ids 就按扫描顺序跑一批。
     *  rule_id 只跑某一条规则；留空跑全部启用的规则。 */
    run: (fileIds?: string[], ruleId?: string) =>
      http.post('/organize/run', { file_ids: fileIds ?? [], rule_id: ruleId ?? '' }),
    /** ✅ POST /api/organize/preview —— 拿真实识别器试算一个名字 */
    preview: (name: string, isDir = false) =>
      http.post('/organize/preview', { name, is_dir: isDir }),
    /** ✅ GET /api/organize/records?status=&limit=&offset= */
    records: (q?: { status?: string; limit?: number; offset?: number }) =>
      http.get('/organize/records', q),
    /** ✅ POST /api/organize/records/{id}/ignore */
    ignore: (id: number) => http.post(`/organize/records/${id}/ignore`),
    /** ✅ POST /api/organize/records/{id}/retry */
    retry: (id: number) => http.post(`/organize/records/${id}/retry`),
    /** ✅ POST /api/organize/records/clear —— 只清台账，不动网盘文件 */
    clearRecords: (status?: string) => http.post('/organize/records/clear', { status: status ?? '' }),
  },

  /* ---------------- 影视刮削 ---------------- */
  scrape: {
    /** ✅ GET /api/settings —— 返回 scrape / subtitle 两个已解析的配置对象 */
    config: () => http.get('/settings'),
    /**
     * ✅ PUT /api/settings
     *
     * 两个配置在后端都是「一行 JSON」，这里要 stringify 后放进去。
     */
    saveConfig: (body: Record<string, string>) => http.put('/settings', body),

    /** ✅ POST /api/libraries/{id}/scrape 整库刮削（后台任务） */
    start: (libId: number, body: { overwrite?: boolean; refresh_cache?: boolean } = {}) =>
      http.post(`/libraries/${libId}/scrape`, body),
    /** ✅ GET /api/libraries/{id}/scrape 进度 */
    job: (libId: number) => http.get(`/libraries/${libId}/scrape`),
    /** ✅ POST /api/libraries/{id}/scrape/cancel */
    cancel: (libId: number) => http.post(`/libraries/${libId}/scrape/cancel`),

    /** ✅ GET /api/libraries/{id}/media?status=ok|unmatched|error&limit&offset */
    media: (libId: number, q: { status?: string; limit?: number; offset?: number } = {}) =>
      http.get(`/libraries/${libId}/media`, q),

    /**
     * ✅ POST /api/media/{id}/scrape 按 IMDb ID 重刮单条
     *
     * 后端只发 1 次 meta 请求（命中缓存时 0 次），就地改写 .strm 旁边的
     * nfo / 海报 / 字幕。翻译字幕可能要几十秒，所以超时给足。
     */
    rescrape: (
      mediaId: number,
      body: { imdb_id: string; overwrite?: boolean; subtitles?: boolean },
    ) => http.post(`/media/${mediaId}/scrape`, body, { timeout: 180_000 }),

    /** ✅ POST /api/media/{id}/subtitle 单条补中文字幕 */
    subtitle: (mediaId: number, overwrite = false) =>
      http.post(`/media/${mediaId}/subtitle`, { overwrite }, { timeout: 180_000 }),

    /** ✅ POST /api/libraries/{id}/subtitles 整库补字幕（后台任务） */
    subtitles: (libId: number, redo = false) => http.post(`/libraries/${libId}/subtitles`, { redo }),
    /** ✅ GET /api/libraries/{id}/subtitles/job */
    subtitleJob: (libId: number) => http.get(`/libraries/${libId}/subtitles/job`),
  },

  /* ---------------- 网盘文件 ---------------- */
  files: {
    /** ✅ GET /api/accounts/{id}/browse?cid= 浏览 115 目录 */
    browse115: (accountId: number, cid = '0') => http.get(`/accounts/${accountId}/browse`, { cid }),
    /**
     * ✅ GET /api/files/browse?account_id=&cid=&dirs_only=
     *
     * 和 browse115 的区别：这条会顺带返回「这个文件在台账里对应的 .strm」，
     * 所以文件页能直接标出哪些已入库，而不是只列网盘内容。
     */
    browse: (
      accountId: number,
      cid = '0',
      opts: { dirsOnly?: boolean; timeout?: number } = {},
    ) =>
      http.get(
        '/files/browse',
        { account_id: accountId, cid, dirs_only: opts.dirsOnly ? 1 : undefined },
        { timeout: opts.timeout ?? 70_000 },
      ),
    /** ✅ POST /api/files/move 移动网盘条目（本地 .strm 等下次同步跟上） */
    move: (body: { account_id: number; ids: string[]; to_cid: string }) =>
      http.post('/files/move', body, { timeout: 90_000 }),
    /** ✅ POST /api/files/remove 删除网盘条目（进 115 回收站，可恢复） */
    remove: (body: { account_id: number; ids: string[] }) =>
      http.post('/files/remove', body, { timeout: 90_000 }),
    /** 🚧 POST /files/organize 对选中目录手动整理 */
    organize: (body: unknown) => http.post('/files/organize', body),
    /** 🚧 POST /files/library/episodes/preview 指定季集 */
    episodesPreview: (body: unknown) => http.post('/files/library/episodes/preview', body),
    /** 🚧 POST /files/library/episodes */
    episodes: (body: unknown) => http.post('/files/library/episodes', body),
  },

  /* ---------------- STRM 管理 ---------------- */
  strm: {
    /** ✅ GET /api/strm/overview 库状态 + 流水账 + 账号节流状态 */
    overview: () => http.get('/strm/overview'),
    /** ✅ GET /api/strm/runs */
    runs: (limit = 30) => http.get('/strm/runs', { limit }),
    /** ✅ GET /api/strm/orphans?library_id= 扫本地孤儿（走磁盘，可能十几秒） */
    orphans: (libraryId?: number) =>
      http.get(
        '/strm/orphans',
        libraryId ? { library_id: libraryId } : undefined,
        { timeout: 240_000 },
      ),
    /** ✅ POST /api/strm/orphans/clean（all=true 时服务端重扫后全清） */
    cleanOrphans: (body: {
      library_id: number
      extra?: string[]
      missing?: string[]
      all?: boolean
    }) => http.post('/strm/orphans/clean', body, { timeout: 180_000 }),
  },

  /* ---------------- 海报墙 / 本地媒体库 ---------------- */
  local: {
    /** ✅ GET /api/local/overview —— 每个库的本地根目录 + 台账数 */
    overview: () => http.get('/local/overview'),
    /** ✅ GET /api/local/browse?library_id=&path= —— 列一层真实目录 */
    browse: (libraryId: number, path = '') =>
      http.get('/local/browse', { library_id: libraryId, path }, { timeout: 60_000 }),
    /** ✅ POST /api/local/stat —— 真遍历一遍，算体积 / 孤儿 / 空目录 */
    stat: (libraryId: number) => http.post('/local/stat', { library_id: libraryId }, { timeout: 90_000 }),
    /** ✅ POST /api/local/remove —— 删本地文件；非空目录要 recursive */
    remove: (body: { library_id: number; path: string; recursive?: boolean }) =>
      http.post('/local/remove', body, { timeout: 60_000 }),
    /**
     * ✅ POST /api/local/open —— 在「运行服务的机器」上打开目录。
     * 只收相对库根的路径；服务跑在 Docker 里时返回 opened:false + hint，
     * 前端要提示用户复制路径自己打开。
     */
    open: (body: { library_id: number; path: string }) => http.post('/local/open', body),
    /** 🚧 GET /local/titles?page=&filter= */
    titles: (params?: Record<string, string | number>) => http.get('/local/titles', params),
    /** 🚧 GET /local/titles/detail?key= */
    detail: (key: string) => http.get('/local/titles/detail', { key }),
    /** 🚧 POST /local/scrape */
    scrape: (keys: string[]) => http.post('/local/scrape', { keys }),
    /** 🚧 GET /local/titles/emby */
    emby: (key: string) => http.get('/local/titles/emby', { key }),
    /** 🚧 GET /local/titles/emby-stats */
    embyStats: () => http.get('/local/titles/emby-stats'),
    /** 🚧 POST /local/titles/probe */
    probe: (key: string, confirm = false) => http.post('/local/titles/probe', { key, confirm }),
    /** 🚧 GET /local/titles/reconcile Emby 与本地对账 */
    reconcile: () => http.get('/local/titles/reconcile', undefined, { timeout: 180_000 }),
    /** ✅ GET /api/wall（本项目 TMDB 电影墙） */
    wall: (params?: Record<string, string | number>) => http.get('/wall', params),
    /** ✅ GET /api/wall/providers 静态平台清单，不打 TMDB */
    wallProviders: () => http.get('/wall/providers'),
  },

  /* ---------------- 我的海报墙：Emby 里已有的影视 ---------------- */
  catalog: {
    /** ✅ GET /api/catalog 列表（默认 TMDB，可切 Emby / 本地台账） */
    list: (params?: Record<string, string | number | boolean | undefined>) =>
      http.get('/catalog', params),
    /** ✅ GET /api/catalog/item?key=&source= 详情 + 分集 + 本地台账 */
    item: (key: string, source?: string) =>
      http.get('/catalog/item', source ? { key, source } : { key }),
    /** ✅ POST /api/catalog/item 改元数据（改 IMDb ID 会按 ID 重刮） */
    update: (body: unknown) => http.post('/catalog/item', body, { timeout: 120_000 }),
    /** ✅ DELETE /api/catalog/item?key=&purge=1 删条目（默认保留 115 源文件） */
    remove: (key: string, purge = false) =>
      http.del('/catalog/item', { query: { key, purge: purge ? 1 : 0 }, timeout: 120_000 }),
    /** ✅ POST /api/catalog/scan 预览某个 115 目录里能加多少片 */
    scan: (body: unknown) => http.post('/catalog/scan', body, { timeout: 90_000 }),
    /** ✅ POST /api/catalog/add 生成 .strm + 登记台账 + 自动刮削 */
    add: (body: unknown) => http.post('/catalog/add', body, { timeout: 120_000 }),
  },

  /* ---------------- 任务中心 ---------------- */
  tasks: {
    /** ✅ GET /api/tasks 进行中的任务 + 汇总 + 可选的库 */
    list: () => http.get('/tasks'),
    /** ✅ GET /api/tasks/history 同步流水账 */
    history: (params?: Record<string, string | number>) => http.get('/tasks/history', params),
    /** ✅ POST /api/tasks/cancel 停掉一个正在跑的任务 */
    cancel: (body: { kind: string; library_id: number }) => http.post('/tasks/cancel', body),
    /** ✅ POST /api/tasks/run 手动起一轮同步或刮削 */
    run: (body: { kind: 'sync' | 'scrape'; library_id: number; full?: boolean; refresh?: boolean }) =>
      http.post('/tasks/run', body, { timeout: 30_000 }),
    /** ✅ POST /api/tasks/clear 清空已结束的历史 */
    clear: () => http.post('/tasks/clear'),
  },

  /* ---------------- 发现与订阅 ---------------- */
  subscriptions: {
    /** ✅ GET /api/subscriptions —— 列表 + 分类计数 + 全局周期 */
    list: (state?: string) => http.get('/subscriptions', state ? { state } : undefined),
    /** ✅ POST /api/subscriptions 建订阅；剧集的服务端会用 TMDB 校正标题与集数 */
    create: (body: {
      tmdb_id: number
      media_type: 'tv' | 'movie'
      title?: string
      original_title?: string
      year?: number
      imdb_id?: string
      library_id?: number
      quality?: string
      min_size?: number
      max_size?: number
      /** 起播线：这一集及之前不找资源不下发；0/0 = 从头追 */
      watermark_season?: number
      watermark_episode?: number
    }) => http.post('/subscriptions', body),
    /**
     * ✅ PUT /api/subscriptions/{id} 改画质 / 状态 / 库 / 水位线
     *
     * watermark_season + watermark_episode 必须成对出现，语义是「把追更位置
     * 直接设到这里」—— 可前进也可后退，用户用它跳过补不到的老集。
     */
    update: (
      id: number,
      body: {
        quality?: string
        state?: string
        library_id?: number
        watermark_season?: number
        watermark_episode?: number
      },
    ) => http.put(`/subscriptions/${id}`, body),
    /** ✅ DELETE /api/subscriptions/{id} */
    remove: (id: number) => http.del(`/subscriptions/${id}`),
    /**
     * ✅ POST /api/subscriptions/{id}/run 立即检查一轮
     *
     * 同步返回这一轮的结论（「已提交 S01E05」/「已追平」）。最坏情况要
     * 走完 TMDB + 搜索 + 离线提交，所以超时给到 200 秒 —— 比后端的
     * 3 分钟上限略短，宁可前端先放弃，也不要两个都等。
     */
    run: (id: number) => http.post(`/subscriptions/${id}/run`, {}, { timeout: 200_000 }),
    /** ✅ GET /api/subscriptions/seasons?tmdb_id= 季清单 */
    seasons: (tmdbId: number) => http.get('/subscriptions/seasons', { tmdb_id: tmdbId }),
    /** ✅ GET /api/subscriptions/config */
    config: () => http.get('/subscriptions/config'),
    /** ✅ PUT /api/subscriptions/config 只改检查周期（15 分钟 ~ 7 天） */
    saveConfig: (intervalMinutes: number) =>
      http.put('/subscriptions/config', { interval_minutes: intervalMinutes }),
  },

  transfer: {
    /**
     * ✅ POST /api/offline/add 把磁力 / .torrent 直链交给 115 离线下载
     *
     * 种子站的搜索结果直接走这条，不用先复制磁力再去别处粘。
     * 不传 account_id 时后端自动挑第一个支持离线的账号。
     */
    offlineAdd: (body: { uris: string[]; save_cid?: string; label?: string; account_id?: number }) =>
      http.post('/offline/add', body, { timeout: 120_000 }),
    /** 🚧 POST /transfer/submit 链接转存 */
    submit: (body: unknown) => http.post('/transfer/submit', body),
    /** 🚧 GET /transfer/sources 资源站 */
    sources: () => http.get('/transfer/sources'),
    /** 🚧 POST /transfer/sources */
    addSource: (body: unknown) => http.post('/transfer/sources', body),
    /** 🚧 GET /transfer/targets 转存目标目录 */
    targets: () => http.get('/transfer/targets'),
    /** ✅ GET /api/res/search 本项目资源搜索（TG + 种子站） */
    resSearch: (params: Record<string, string | number>) => http.get('/res/search', params),
    /**
     * ✅ POST /api/res/test 测一个搜索源还活着吗
     *
     * 和「搜一次看看有没有结果」的区别：「这个源没这条片」和「这个源挂了」
     * 在搜索结果里长得一样，这里把连没连上、解析出几条、失败原文分开返回。
     * kind: tg（频道）| site（内置种子站 key）| custom（自定义 RSS/Torznab 那一行原文）
     */
    resTest: (body: { kind: 'tg' | 'site' | 'custom'; target: string; query?: string }) =>
      http.post('/res/test', body, { timeout: 30_000 }),
  },

  /* ---------------- TMDB / 元数据 ---------------- */
  /* ---------------- 播出日历 ---------------- */
  calendar: {
    /**
     * ✅ GET /api/calendar?start=YYYY-MM-DD&refresh=1
     * 七天排期，整周缓存（本周 30 分钟）。冷启动要并发问几十次 TMDB，超时放宽到 90 秒。
     */
    week: (start?: string, refresh?: boolean) =>
      http.get('/calendar', { start, refresh: refresh ? 1 : undefined }, { timeout: 90_000 }),
    /**
     * ✅ GET /api/calendar/douban?start=YYYY-MM-DD
     *
     * 豆瓣数据从 /api/calendar 主响应里拆出来的独立接口：国产剧（tv）+ 本周
     * 上映的国内电影（movies，日历 Entry）。后端有 5 秒上限且失败/超时也回
     * 200 + ok=false，前端由它单独驱动「国产剧」那一栏，慢也不拖累周历主体。
     * 客户端超时略大于服务端的 5s，好让服务端那份降级响应能正常回来。
     */
    douban: (start?: string) =>
      http.get('/calendar/douban', { start }, { timeout: 8_000 }),
    /**
     * ✅ POST /api/calendar/tvdb/test —— 不传 body 就测库里已存的凭据。
     * base 是输入框里还没保存的接口根；传空串表示「按官方地址测」（不是没传）。
     */
    testTVDB: (body?: { api_key?: string; pin?: string; base?: string }) =>
      http.post('/calendar/tvdb/test', body ?? {}),
    /**
     * ✅ POST /api/calendar/tvmaze/test —— 免 Key 的公共排期接口。
     * base 是输入框里还没保存的反代根；传空串表示按官方地址测。
     */
    testTVMaze: (body?: { base?: string }) =>
      http.post('/calendar/tvmaze/test', body ?? {}),
  },

  /* ---------------- 豆瓣（国产剧 / 国内档期） ---------------- */
  douban: {
    /**
     * ✅ GET /api/douban/search?q=&limit=
     *
     * 「先认片再搜资源」里国产片的那一半：TMDB 常常查不到或查错（中文名、
     * 剧集分季），豆瓣的搜索建议更准。返回的是**候选**，不带任何资源。
     */
    search: (q: string, limit = 10) => http.get('/douban/search', { q, limit }),
    /**
     * ✅ GET /api/douban/collection?key=&start=&count=
     *
     * key 是豆瓣内部的集合名（服务端有常量）：movie_showing 正在上映、
     * movie_soon 即将上映、tv_domestic 国产剧、tv_hot 热播剧。
     */
    collection: (key: string, start = 0, count = 20) =>
      http.get('/douban/collection', { key, start, count }, { timeout: 30_000 }),
    /** ✅ GET /api/douban/detail?id=&kind= —— kind 猜错时服务端会换端点重试 */
    detail: (id: number, kind: 'movie' | 'tv' = 'movie') => http.get('/douban/detail', { id, kind }),
    /** ✅ POST /api/douban/test —— 设置页「测试豆瓣」，真打一次搜索 */
    test: () => http.post('/douban/test', {}),
  },

  /* ---------------- 影视详情（任意页面点片名） ---------------- */
  title: {
    /**
     * ✅ GET /api/title?type=tv|movie&tmdb_id=&media_id=&q=&year=
     *
     * 和 catalog.item 的分工：那个只认已入库的 emby:/local: key；这个按
     * tmdb_id 查，**不要求入库** —— 订阅列表和播出日历里点开的就是这种。
     * q + year 是「只有片名」的兜底（首页最近入库、整理记录），服务端先按
     * 片名问 TMDB 换 id，之后和直接给 id 走同一条路。
     * 返回 TMDB 详情 + 本地台账（有就带）+ 订阅状态（订了就带）。
     */
    detail: (p: {
      type?: string
      tmdb_id?: number | string
      media_id?: number | string
      q?: string
      year?: string | number
    }) =>
      http.get('/title', {
        type: p.type || undefined,
        tmdb_id: p.tmdb_id || undefined,
        media_id: p.media_id || undefined,
        q: p.q || undefined,
        year: p.year || undefined,
      }),
  },

  tmdb: {
    /** ✅ GET /api/tmdb/search?q=&type=&year= — 先认片：返回 TMDB 影视候选 */
    search: (q: string, type = '', year = '') => http.get('/tmdb/search', { q, type, year }),
    /** 🚧 GET /tmdb/discover?list=&page= */
    discover: (list: string, page = 1, extra?: Record<string, string | number>, sort?: string) =>
      http.get('/tmdb/discover', { list, page, ...(extra ?? {}), ...(sort ? { sort } : {}) }),
    /** 🚧 GET /tmdb/discover 无参时的榜单定义 */
    discoverMeta: () => http.get('/tmdb/discover'),
    /** 图片走同源代理，避免浏览器直连 image.tmdb.org 被墙 */
    img: (path: string, size = 'w342') =>
      path ? `/api/tmdb/img?path=${encodeURIComponent(path)}&size=${size}` : '',
  },

  /* ---------------- 系统配置 ---------------- */
  config: {
    /** ✅ GET /api/settings —— 密钥只回传 *_masked / *_set，不回明文 */
    settings: () => http.get('/settings'),
    /** ✅ PUT /api/settings，一次提交多个键；密钥留空表示不修改 */
    saveSettings: (patch: Record<string, string>) => http.put('/settings', patch),
    /** ✅ POST /api/settings/test-server 用库里存的 Key 测 Emby */
    testServer: (body?: unknown) => http.post('/settings/test-server', body || {}),
    /** ✅ POST /api/settings/connect-server */
    connectServer: (body: unknown) => http.post('/settings/connect-server', body),
    /** ✅ GET /api/settings/server-status */
    serverStatus: () => http.get('/settings/server-status'),
    /** ✅ GET /api/settings/server-libraries 真实媒体库列表 */
    serverLibraries: () => http.get('/settings/server-libraries'),
    /** ✅ POST /api/settings/server-libraries/scan 触发扫描（空 id = 全库） */
    scanServerLibrary: (libraryId?: string) =>
      http.post('/settings/server-libraries/scan', { library_id: libraryId || '' }),
    /** ✅ GET /api/net/check 外网连通性（能拿到 HTTP 响应即算通） */
    netCheck: () => http.get('/net/check'),
    /** ✅ GET /api/net/address 当前页面用的地址，用来一键填播放域名 */
    netAddress: () => http.get('/net/address'),
    /** ✅ POST /api/settings/rewrite-domain 改域名并把已有 .strm 的地址一起重写 */
    rewriteDomain: (domain: string) =>
      http.post('/settings/rewrite-domain', { domain }, { timeout: 180_000 }),
    /** ✅ GET /api/imdb/check?key= —— 不传 key 就用库里存的那份 */
    imdbCheck: (key?: string) => http.get('/imdb/check', key ? { key } : undefined, { timeout: 30_000 }),
    /** 🚧 POST /config/test-tmdb */
    testTmdb: (body: unknown) => http.post('/config/test-tmdb', body),
    /** 🚧 POST /config/test-ai */
    testAI: (body: unknown) => http.post('/config/test-ai', body),
  },

  system: {
    /** ✅ GET /api/system/status 首页系统资源 + 各网盘容量（后端 30 秒缓存） */
    status: () => http.get('/system/status'),
    /** 🚧 GET /system/logs */
    logs: (params?: Record<string, string | number>) => http.get('/system/logs', params),
    /** 🚧 POST /system/logs/clear */
    clearLogs: () => http.post('/system/logs/clear'),
    /** 🚧 GET /system/backups */
    backups: () => http.get('/system/backups'),
    /** 🚧 POST /system/backups 立即备份 */
    createBackup: (body?: unknown) => http.post('/system/backups', body ?? {}),
    /** 🚧 DELETE /system/backups/{name} */
    deleteBackup: (name: string) => http.del(`/system/backups/${encodeURIComponent(name)}`),
    /** 🚧 POST /system/backups/{name}/restore */
    restoreBackup: (name: string) => http.post(`/system/backups/${encodeURIComponent(name)}/restore`),
    /** 🚧 POST /system/restart */
    restart: () => http.post('/system/restart'),
    /** 🚧 GET /system/update */
    update: () => http.get('/system/update'),
    /** 🚧 POST /system/update/check */
    updateCheck: () => http.post('/system/update/check'),
    /** 🚧 POST /system/update/apply */
    updateApply: (body?: unknown) => http.post('/system/update/apply', body ?? {}),
  },

  /* ---------------- 消息通知 ---------------- */
  notify: {
    /** ✅ GET /api/notify — 配置（密钥打码）+ 已启用渠道 + 事件清单 */
    get: () => http.get('/notify'),
    /** ✅ PUT /api/notify — 密钥字段留空 = 不修改 */
    save: (body: unknown) => http.put('/notify', body),
    /** ✅ POST /api/notify/test — 传 channel 只测该渠道，不传测全部 */
    test: (body?: { channel?: string }) => http.post('/notify/test', body ?? {}),
    /** ✅ POST /api/notify/tg/discover — 用 Bot Token 反查 Chat ID */
    tgDiscover: (token?: string) => http.post('/notify/tg/discover', { token: token ?? '' }),
    /** ✅ GET /api/notify/tg/bot — 双向机器人状态（Token / 白名单 / 最近状态） */
    tgBot: () => http.get('/notify/tg/bot'),
    /** ✅ PUT /api/notify/tg/bot — 改白名单；reset=true 解绑全部 */
    saveTgBot: (body: { chat_ids?: string; reset?: boolean }) => http.put('/notify/tg/bot', body),
  },

  /* ---------------- 站内通知中心（右上角铃铛） ---------------- */
  notifications: {
    /** ✅ GET /api/notifications?limit=50 — 最近消息 + 未读数 */
    list: (limit = 50) => http.get('/notifications', { limit }),
    /** ✅ POST /api/notifications/read — ids 指定条目，all=true 全部已读 */
    read: (body?: { ids?: number[]; all?: boolean }) => http.post('/notifications/read', body ?? {}),
  },

}

export type Api = typeof api
