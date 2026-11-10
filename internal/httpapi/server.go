// Package httpapi 是管理后台的 HTTP 层：路由、鉴权、参数校验、错误翻译。
//
// 这里刻意不引 Web 框架：Go 1.22 起 ServeMux 已经支持 "GET /api/x/{id}" 这种
// 方法+路径参数，够用了。少一个依赖，就少一次升级时的破坏性变更。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"strm115/internal/calendar"
	"strm115/internal/config"
	"strm115/internal/embygw"
	"strm115/internal/douban"
	"strm115/internal/notify"
	"strm115/internal/offline"
	"strm115/internal/panpool"
	"strm115/internal/resource"
	"strm115/internal/scrape"
	"strm115/internal/store"
	"strm115/internal/subscribe"
	syncsvc "strm115/internal/sync"
	"strm115/internal/tgbot"
	"strm115/internal/tmdb"
)

// 登录态有效期。7 天足够长，不打扰；密码一改全部作废。
const sessionTTL = 7 * 24 * time.Hour

type Server struct {
	cfg     config.Config
	st      *store.Store
	pool    *panpool.Pool
	sync    *syncsvc.Service
	notify  *notify.Client
	offline *offline.Service
	scrape  *scrape.Service
	// res 常驻而不是每次请求 new：TG 频道页有 60 秒的进程内缓存，
	// 每次新建就等于没缓存，而 t.me 会把同一频道的连续请求挂到超时。
	res *resource.Service
	// cal 常驻：整周缓存挂在它身上，每次请求 new 等于没缓存。
	cal   *calendar.Service
	log   *slog.Logger
	sess  *sessionStore
	login *loginGuard // 登录失败限速，按来源 IP
	qrMu  sync.Mutex
	qr    map[string]*qrSession
	// 夸克TV 的扫码会话单独一张表：两家的登录协议完全不同，
	// 塞进同一个 struct 只会让两边都得写「这个字段对我没用」的注释。
	qkMu  sync.Mutex
	qk    map[string]*quarkSession
	start time.Time
	// 夸克网页 Cookie 的自动续期（__puus 每 ~100 分钟主动续一次）。
	quarkKeep *quarkKeeper
	// gw 是 Emby 302 网关的监听管理器，没启用时为 nil。
	gw *embygw.Manager

	// 首页系统资源（CPU / 内存 / 磁盘 / 网盘容量）的 30 秒缓存。
	sysMu    sync.Mutex
	sysCache systemStatus
	sysAt    time.Time

	// 管理员凭据的内存副本。启动时从数据库读一次，改动时同步更新，
	// 这样每个请求不必都去查 SQLite。
	credMu   sync.RWMutex
	credUser string
	credHash string

	// 首次设置只允许发生一次。并发两个请求同时「都没看到账号」时，
	// 靠这把锁把「检查 + 写入」变成原子操作。
	setupMu sync.Mutex

	// 整库补字幕是按库串行的后台任务，一个库同时只跑一个。
	subMu   sync.Mutex
	subJobs map[int64]*subtitleJob

	// Telegram 机器人：常驻长轮询，退出时由 Close 停掉。
	botMu     sync.Mutex
	bot       *tgbot.Bot
	botCancel context.CancelFunc

	// subs 是订阅轮询器。它自己带心跳（每分钟扫一次到期订阅），
	// 所以这里只需要持有引用，供手动「立即检查」调用。
	subs      *subscribe.Service
	subCancel context.CancelFunc

	// 海报墙 TMDB 数据源的进程内缓存。翻页 / 排序 / 反复点详情都会重新
	// 问同一批作品，没这层缓存每翻一页就是 30 次 TMDB 请求。
	catalogTMDBMu sync.Mutex
	catalogTMDB   map[string]catalogTMDBEntry

	// 整理是「列目录 → 移动 → 同步」三步，两轮同时跑会在 115 侧互相
	// 挪到一半的目录上（网页面板点一次 + 入库后自动跑一次，很容易撞）。
	// 用 TryLock：撞上了就跳过，而不是排队把 115 拖到风控。
	organizeMu sync.Mutex

	// 播放地址自愈用：seenHosts 记录用户实际用过、且能打开的 Host。
	// 容器里枚举网卡只有 172.x，宿主机 IP 只能从请求里学，见 netfollow.go。
	seenMu    sync.Mutex
	seenHosts map[string]time.Time
	// playProbe 可替换，便于测试；nil 时走真实 HTTP 探测。
	playProbe   func(context.Context, string) bool
	playFixMu   sync.Mutex
	playAlerted string
}

type Deps struct {
	Cfg   config.Config
	Store *store.Store
	Pool  *panpool.Pool
	Sync  *syncsvc.Service
	Log   *slog.Logger
	// Gateway 是 Emby 302 网关的监听管理器：设置页一改端口/上游就按新配置起停。
	// 允许为 nil（单测与不开网关的部署）。
	Gateway *embygw.Manager
}

func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{
		cfg:     d.Cfg,
		st:      d.Store,
		pool:    d.Pool,
		sync:    d.Sync,
		gw:      d.Gateway,
		log:     d.Log,
		sess:    newSessionStore(sessionTTL),
		login:   newLoginGuard(),
		qr:      map[string]*qrSession{},
		qk:      map[string]*quarkSession{},
		subJobs: map[int64]*subtitleJob{},
		start:   time.Now(),
	}
	// TMDB 上游是进程级配置，必须在任何业务请求进来之前设好：
	// 电影墙、演员刮削、订阅三处都会 NewClient，启动时漏了就全走官方直连。
	s.applyTMDBUpstream(context.Background())
	// 夸克 TV 接口根同理：driverquarktv 是包级配置，登录与列目录都读它。
	s.applyQuarkUpstream(context.Background())
	// 出站代理同理：tmdb / tvdb / 夸克登录等包级单例启动时就建好了 Client，
	// 必须在这里把数据库或环境变量里的代理推进进程级开关，否则设置页形同虚设。
	s.applyNetProxy(context.Background())
	// 网页 Cookie 自动续期：__puus 会轮换，不主动续就得每几小时重贴一次。
	s.quarkKeep = newQuarkKeeper(s)
	s.quarkKeep.start()
	// 播出日历：TMDB 出剧单 / 分集，TVDB 补时刻与电视网。
	// Credentials 每轮现读设置，用户在设置页改完 Key 下一次请求就换掉 Client。
	s.cal = calendar.New(calendar.Deps{
		Store: d.Store,
		TMDb:  func() *tmdb.Client { return s.tmdbClient(context.Background()) },
		Credentials: func() (string, string) {
			ctx := context.Background()
			return strings.TrimSpace(s.setting(ctx, "tvdb_api_key")),
				strings.TrimSpace(s.setting(ctx, "tvdb_pin"))
		},
		// 接口根也现读：设置页换成自建反代后，下一轮日历就用新地址，不用重启。
		TVDBBaseFunc: func() string {
			return strings.TrimSpace(s.setting(context.Background(), "tvdb_base"))
		},
		// 时刻增强源三项都现读设置，改完下一轮生效；留空由 calendar 兜成 tvmaze。
		SourceFunc: func() string {
			return strings.TrimSpace(s.setting(context.Background(), "calendar_source"))
		},
		TVMazeBaseFunc: func() string {
			return strings.TrimSpace(s.setting(context.Background(), "tvmaze_base"))
		},
		NetProxy: func() string {
			// 走 netProxy 而不是直接读设置：环境变量部署（docker -e HTTPS_PROXY）
			// 也要让日历的 TVMaze 跟着走，否则「代理不起作用」会漏在这一处。
			return s.netProxy(context.Background())
		},
		// 豆瓣：国产剧与国内档期。设置页关掉时返回 nil，日历就不再等它，
		// 也不会在境外部署里白等十几秒超时。
		Douban: func() *douban.Client {
			ctx := context.Background()
			if !s.doubanOn(ctx) {
				return nil
			}
			return s.doubanClient(ctx)
		},
		Log: d.Log,
	})
	s.notify = notify.New(s.loadNotifyConfig(context.Background()))
	s.scrape = scrape.New(d.Store, func(ctx context.Context) string {
		// 刮削现在只有 TMDB 一家上游；没填自己的 Key 时由 tmdb.DefaultKey 兜底。
		return s.setting(ctx, "tmdb_api_key")
	}, func() string { return d.Cfg.MediaRoot }, d.Log)
	s.offline = offline.New(d.Pool, d.Log)
	s.offline.OnDone(s.onOfflineDone)
	s.res = resource.New(d.Log)
	s.subs = subscribe.New(d.Store, d.Log, subscribe.Deps{
		TMDb:   s.tmdbClient,
		Search: s.resourceSearch,
		// 复用网页和机器人用的同一个提交入口：账号挑选、目录、错误翻译
		// 都只有这一份实现，订阅不该长出第二套。
		Submit: func(ctx context.Context, acctID int64, uris []string, cid, label string) ([]string, error) {
			_, hashes, err := s.submitOffline(ctx, acctID, uris, cid, label)
			return hashes, err
		},
		Interval: s.subInterval,
	})
	// 用可取消的 ctx 启动：Close() 要能停掉这个 goroutine，
	// 否则容器重启 / 测试收尾时它还在后台扫库。
	subCtx, subCancel := context.WithCancel(context.Background())
	s.subCancel = subCancel
	s.subs.Start(subCtx)
	s.initCreds(context.Background())
	// 老配置里只有上一版的四个事件，新事件要补进去，否则升级后静默收不到。
	s.migrateNotifyEvents(context.Background())
	// 302 网关从「独占端口」改成「挂在播放端口上」，老配置的端口号要翻译成开关。
	s.migrateGatewayOn(context.Background())
	go s.janitor()
	// 资源保存目录的巡检：手动在 115 里转存的东西没有任何事件能通知到程序，
	// 只能自己定期看一眼（开关跟「入库后自动整理」共用）。
	go s.watchIngestFolder(context.Background())
	// 播放地址自愈：DHCP 换 IP 会让整库 .strm 指向一个不存在的地址，
	// 表现为「昨天还能放、今天全都播不了」，见 netfollow.go。
	go s.watchPlayDomain(context.Background())
	// Telegram 机器人：Token 没配时它会自己等着，配了就能用。
	s.startTGBot()
	return s
}

// Handler 返回管理后台的完整 handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// —— 会话 / 首次设置 ——
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/credentials", s.handleChangeCreds)

	// —— 概览 ——
	mux.HandleFunc("GET /api/instance", s.handleInstance)
	// 总览面板：库 / 台账 / 刮削 / 在跑的任务 / 最近入库，一次拿全。
	// Emby 的连通性不在这里探（会拖慢首屏），前端并行调 server-status。
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)

	// —— 账号 / 登录 115 ——
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("GET /api/accounts/{id}/stats", s.handleAccountStats)
	mux.HandleFunc("GET /api/accounts/{id}/browse", s.handleBrowse)
	// 夸克：TV 接口扫码登录（播放）+ 网页 Cookie（转存分享）
	mux.HandleFunc("POST /api/accounts/quarktv/qr", s.handleQuarkQRStart)
	mux.HandleFunc("GET /api/accounts/quarktv/qr/{id}", s.handleQuarkQRPoll)
	mux.HandleFunc("POST /api/accounts/quarktv/web-qr", s.handleQuarkWebQRStart)
	mux.HandleFunc("GET /api/accounts/quarktv/web-qr/{id}", s.handleQuarkWebQRPoll)
	mux.HandleFunc("POST /api/accounts/{id}/quark-cookie", s.handleQuarkCookie)
	mux.HandleFunc("POST /api/quark/share", s.handleQuarkShare)
	mux.HandleFunc("POST /api/quark/test", s.handleQuarkTest)
	mux.HandleFunc("GET /api/quark/keepalive", s.handleQuarkKeepAlive)
	mux.HandleFunc("POST /api/quark/keepalive", s.handleQuarkKeepAlive)
	mux.HandleFunc("POST /api/115/qr", s.handleQRStart)
	mux.HandleFunc("GET /api/115/qr/{id}", s.handleQRPoll)
	mux.HandleFunc("POST /api/115/cookie", s.handleCookieLogin)
	// 把一条 115 分享直接转存到自己的网盘（资源搜索里的「存到 115」）
	mux.HandleFunc("POST /api/115/share", s.handleSaveShare)

	// —— 媒体库 ——
	mux.HandleFunc("GET /api/libraries", s.handleListLibraries)
	mux.HandleFunc("POST /api/libraries", s.handleCreateLibrary)
	mux.HandleFunc("DELETE /api/libraries/{id}", s.handleDeleteLibrary)
	mux.HandleFunc("POST /api/libraries/{id}/sync", s.handleStartSync)
	mux.HandleFunc("GET /api/libraries/{id}/job", s.handleJob)
	mux.HandleFunc("POST /api/libraries/{id}/cancel", s.handleCancelSync)
	mux.HandleFunc("GET /api/libraries/{id}/files", s.handleListFiles)
	mux.HandleFunc("POST /api/libraries/{id}/server", s.handleAttachServer)
	// 一个库挂多个网盘目录：115 一份 + 夸克一份，写进同一个本地目录
	mux.HandleFunc("GET /api/libraries/{id}/sources", s.handleListLibrarySources)
	mux.HandleFunc("POST /api/libraries/{id}/sources", s.handleAddLibrarySource)
	mux.HandleFunc("PUT /api/libraries/{id}/sources/{sid}", s.handleUpdateLibrarySource)
	mux.HandleFunc("DELETE /api/libraries/{id}/sources/{sid}", s.handleDeleteLibrarySource)
	// 直链是否带扩展名：改完下一轮同步生效，所以放在库这一层
	mux.HandleFunc("PUT /api/libraries/{id}/keep-ext", s.handleSetKeepExt)

	// —— 网盘文件（真实读 115，不是台账） ——
	mux.HandleFunc("GET /api/files/browse", s.handleFilesBrowse)
	mux.HandleFunc("POST /api/files/move", s.handleFilesMove)
	mux.HandleFunc("POST /api/files/remove", s.handleFilesRemove)
	// 目录相对库根的路径：加入媒体库前先算准，别把同名层级拼错
	mux.HandleFunc("GET /api/libraries/{id}/reldir", s.handleLibraryRelDir)

	// —— STRM 管理 ——
	// —— 本地文件（宿主机 /media 里真实落了什么） ——
	mux.HandleFunc("GET /api/local/overview", s.handleLocalOverview)
	mux.HandleFunc("GET /api/local/browse", s.handleLocalBrowse)
	mux.HandleFunc("POST /api/local/stat", s.handleLocalStat)
	mux.HandleFunc("POST /api/local/remove", s.handleLocalRemove)
	// 在「服务端所在机器」上打开一个目录：浏览器点不动 file://，
	// 本机跑服务时用它才能真正跳到资源管理器。
	mux.HandleFunc("POST /api/local/open", s.handleLocalOpen)

	mux.HandleFunc("GET /api/strm/overview", s.handleStrmOverview)
	mux.HandleFunc("GET /api/strm/runs", s.handleStrmRuns)
	mux.HandleFunc("GET /api/strm/orphans", s.handleStrmOrphans)
	mux.HandleFunc("POST /api/strm/orphans/clean", s.handleCleanOrphans)

	mux.HandleFunc("GET /api/organize/config", s.handleOrganizeGetConfig)
	mux.HandleFunc("PUT /api/organize/config", s.handleOrganizePutConfig)
	mux.HandleFunc("POST /api/organize/scan", s.handleOrganizeScan)
	mux.HandleFunc("POST /api/organize/run", s.handleOrganizeRun)
	mux.HandleFunc("POST /api/organize/preview", s.handleOrganizePreview)
	mux.HandleFunc("GET /api/organize/records", s.handleOrganizeRecords)
	mux.HandleFunc("POST /api/organize/records/clear", s.handleOrganizeClear)
	mux.HandleFunc("POST /api/organize/records/{id}/ignore", s.handleOrganizeIgnore)
	mux.HandleFunc("POST /api/organize/records/{id}/retry", s.handleOrganizeRetry)

	// —— 设置 ——
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("GET /api/system/status", s.handleSystemStatus)
	mux.HandleFunc("GET /api/settings/server-status", s.handleServerStatus)
	mux.HandleFunc("POST /api/settings/connect-server", s.handleConnectServer)
	mux.HandleFunc("POST /api/settings/test-server", s.handleTestServer)
	// 系统配置 → EMBY 管理：列真实媒体库、触发扫描
	mux.HandleFunc("GET /api/settings/server-libraries", s.handleServerLibraries)
	mux.HandleFunc("POST /api/settings/server-libraries/scan", s.handleServerLibraryScan)
	mux.HandleFunc("POST /api/settings/rewrite-domain", s.handleRewriteDomain)
	// 播放地址探测：把「你现在是用哪个地址打开这个页面」告诉前端，
	// 一键填成 http://<该地址>:6086，不必让用户自己去查本机 IP。
	mux.HandleFunc("GET /api/net/address", s.handleNetAddress)

	// —— 网络检测 / 资源搜索 ——
	mux.HandleFunc("GET /api/net/check", s.handleNetCheck)
	// IMDb 检测不放在 /api/net/check 里：那条只证明网络能到，
	// 这条要拿 Key 真查一次，能分出「断网 / Key 无效 / 配额用光」。
	mux.HandleFunc("GET /api/imdb/check", s.handleIMDbCheck)
	mux.HandleFunc("GET /api/res/search", s.handleResSearch)
	mux.HandleFunc("POST /api/res/test", s.handleResTest)

	// —— 找片：解析文本 / 提交离线下载 / 看任务 ──
	mux.HandleFunc("POST /api/find", s.handleFind)
	mux.HandleFunc("POST /api/offline/add", s.handleOfflineAdd)
	mux.HandleFunc("GET /api/offline/tasks", s.handleOfflineTasks)

	// —— 电影墙：TMDB 榜单，详情里再查一次平台 ——
	mux.HandleFunc("GET /api/wall", s.handleWall)
	// 平台清单是静态的，单独给「每平台一行」的海报墙用：一进页面就能把行标题
	// 全画出来，不用先白等一整页 TMDB 才拿到 provider_options。
	mux.HandleFunc("GET /api/wall/providers", s.handleWallProviders)
	// 资源搜索的「先认片再搜」：按输入词返回 TMDB 候选，选中后才打资源源
	mux.HandleFunc("GET /api/tmdb/search", s.handleTMDBSearch)

	// —— 豆瓣：国产剧与国内档期的数据源（发现与订阅的搜索框、播出日历用）——
	mux.HandleFunc("GET /api/douban/search", s.handleDoubanSearch)
	mux.HandleFunc("GET /api/douban/collection", s.handleDoubanCollection)
	mux.HandleFunc("GET /api/douban/detail", s.handleDoubanDetail)
	mux.HandleFunc("POST /api/douban/test", s.handleDoubanTest)

	// —— 播出日历：TMDB 在播剧单 + 分集，TVDB 补播出时刻 / 电视网 ——
	mux.HandleFunc("GET /api/calendar", s.handleCalendar)
	// 豆瓣拆成独立接口：主响应不再等豆瓣，前端异步拉这一栏（见 Bug 1）。
	mux.HandleFunc("GET /api/calendar/douban", s.handleCalendarDouban)
	mux.HandleFunc("POST /api/calendar/tvmaze/test", s.handleCalendarTVMazeTest)
	mux.HandleFunc("POST /api/calendar/tvdb/test", s.handleCalendarTVDBTest)

	// —— 影视详情：任意页面点片名都走这里（不要求已入库）——
	mux.HandleFunc("GET /api/title", s.handleTitleDetail)

	// —— 我的海报墙：Emby 里的影视（台账兜底）+ 增删改 ——
	mux.HandleFunc("GET /api/catalog", s.handleCatalog)
	mux.HandleFunc("GET /api/catalog/item", s.handleCatalogItem)
	mux.HandleFunc("GET /api/catalog/image", s.handleCatalogImage)
	mux.HandleFunc("POST /api/catalog/item", s.handleCatalogUpdate)
	mux.HandleFunc("DELETE /api/catalog/item", s.handleCatalogDelete)
	mux.HandleFunc("POST /api/catalog/scan", s.handleCatalogScan)
	mux.HandleFunc("POST /api/catalog/add", s.handleCatalogAdd)

	// —— 消息通知 ——
	// —— 刮削 / 字幕 ——
	mux.HandleFunc("POST /api/libraries/{id}/scrape", s.handleStartScrape)
	mux.HandleFunc("GET /api/libraries/{id}/scrape", s.handleScrapeJob)
	mux.HandleFunc("POST /api/libraries/{id}/scrape/cancel", s.handleCancelScrape)
	mux.HandleFunc("GET /api/libraries/{id}/media", s.handleListMedia)
	mux.HandleFunc("POST /api/libraries/{id}/subtitles", s.handleLibrarySubtitles)
	mux.HandleFunc("GET /api/libraries/{id}/subtitles/job", s.handleSubtitleJob)
	mux.HandleFunc("POST /api/media/{id}/subtitle", s.handleFetchOneSubtitle)
	mux.HandleFunc("POST /api/media/{id}/scrape", s.handleRescrapeMedia)
	mux.HandleFunc("GET /api/media/{id}/image", s.handleMediaImage)

	mux.HandleFunc("GET /api/notify", s.handleGetNotify)
	mux.HandleFunc("PUT /api/notify", s.handlePutNotify)
	mux.HandleFunc("POST /api/notify/test", s.handleTestNotify)
	mux.HandleFunc("POST /api/notify/tg/discover", s.handleTGDiscover)

	// —— 站内通知中心：右上角铃铛的数据源 ——
	mux.HandleFunc("GET /api/notifications", s.handleListNotifications)
	mux.HandleFunc("POST /api/notifications/read", s.handleReadNotifications)

	// 任务中心：进行中 / 历史 / 取消 / 重跑 / 清空。
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("GET /api/subscriptions", s.handleListSubs)
	mux.HandleFunc("POST /api/subscriptions", s.handleCreateSub)
	mux.HandleFunc("PUT /api/subscriptions/{id}", s.handleUpdateSub)
	mux.HandleFunc("DELETE /api/subscriptions/{id}", s.handleDeleteSub)
	mux.HandleFunc("POST /api/subscriptions/{id}/run", s.handleRunSub)
	mux.HandleFunc("GET /api/subscriptions/seasons", s.handleSubSeasons)
	mux.HandleFunc("GET /api/subscriptions/config", s.handleGetSubConfig)
	mux.HandleFunc("PUT /api/subscriptions/config", s.handlePutSubConfig)

	mux.HandleFunc("GET /api/tasks/history", s.handleTaskHistory)
	mux.HandleFunc("POST /api/tasks/cancel", s.handleTaskCancel)
	mux.HandleFunc("POST /api/tasks/run", s.handleTaskRun)
	mux.HandleFunc("POST /api/tasks/clear", s.handleTaskClear)
	// 双向机器人：接收 115 分享 / 磁力 / 片名，直接入库
	mux.HandleFunc("GET /api/notify/tg/bot", s.handleTGBotStatus)
	mux.HandleFunc("PUT /api/notify/tg/bot", s.handleTGBotConfig)

	// —— 前端 ——
	// "/" 走 Vue 版（web/ 构建产物）。上一版的单文件界面已经删了。
	mux.HandleFunc("GET /", s.handleSPA)

	return s.middleware(mux)
}

// ==================== 中间件 ====================

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "rec", rec)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			}
		}()

		// 记下用户实际用的地址：一旦宿主机换了 IP，这是唯一可靠的替代候选。
		// 放在鉴权之前——没登录的请求也说明这个 Host 是通的。
		s.rememberHost(r.Host)

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		// HSTS 不在这里加：裸跑形态是明文 HTTP，加了自己反而会把浏览器
		// 锁死在连不上的 https 上。挂 HTTPS 反代时由 Caddy 加。

		// CSRF 第一道闸：只拦带 Origin 且来源不匹配的写请求。
		if crossSiteWrite(r) {
			writeErr(w, http.StatusForbidden, errors.New("跨站请求被拒绝"))
			return
		}

		if !s.public(r.URL.Path) {
			// 还没设置管理员账号时，除了「看一眼会话状态」和「提交初始化」，
			// 其它接口一律拒绝 —— 否则第一次启动就是一个人人可用的后台。
			if !s.credsReady() {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error":        "尚未设置管理员账号密码",
					"setup_needed": "1",
				})
				return
			}
			if !s.sess.valid(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
				return
			}
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/api/libraries/") || r.Method != http.MethodGet {
			s.log.Debug("http", "m", r.Method, "p", r.URL.Path, "ms", time.Since(start).Milliseconds())
		}
	})
}

func (s *Server) public(path string) bool {
	switch path {
	case "/", "/api/session", "/api/setup", "/api/login", "/api/logout", "/favicon.ico":
		return true
	}
	return !strings.HasPrefix(path, "/api/")
}

// ==================== 工具 ====================

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// pathID 取路径参数。Go 的 ServeMux 只给字符串，转数字在这里统一做。
func pathID(r *http.Request, name string) int64 {
	var id int64
	for _, c := range r.PathValue(name) {
		if c < '0' || c > '9' {
			return 0
		}
		id = id*10 + int64(c-'0')
	}
	return id
}

func (s *Server) setting(ctx context.Context, key string) string {
	v, _ := s.st.GetSetting(ctx, key)
	return v
}

func (s *Server) settingBool(ctx context.Context, key string, def bool) bool {
	v, ok := s.st.GetSetting(ctx, key)
	if !ok || v == "" {
		return def
	}
	return config.ParseBool(v, def)
}

// ==================== 会话 ====================

type sessionStore struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{m: map[string]time.Time{}, ttl: ttl}
}

func (s *sessionStore) create() string {
	tok := randToken()
	s.mu.Lock()
	s.m[tok] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) valid(r *http.Request) bool {
	c, err := r.Cookie("sid")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, c.Value)
		return false
	}
	return true
}

func (s *sessionStore) drop(r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		s.mu.Lock()
		delete(s.m, c.Value)
		s.mu.Unlock()
	}
}

// reset 清空所有会话。改密码时用：旧密码签发的会话全部失效。
func (s *sessionStore) reset() {
	s.mu.Lock()
	s.m = map[string]time.Time{}
	s.mu.Unlock()
}

// Version 由 main 注入。
var Version = "dev"
