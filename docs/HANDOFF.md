# 交接文档（HANDOFF）

> 这份文档的读者是**下一个接手这个项目的人或 AI**。
> 目标：读完之后不需要问原作者任何问题，就能改代码、跑测试、部署、继续开发。
>
> 最后更新：2026-11-07 · 对应版本 v0.1.0

---

## 0. 三十秒理解这个项目

**strm115 把 115 网盘的目录变成 Emby/Jellyfin 里的本地媒体库。**

原理只有一句话：**把网盘文件映射成 `.strm` 文本文件，播放时让播放器 302 跳转到 115 的 CDN。**

```
115 网盘 /电影/流浪地球.mkv
      ↓ 全量同步（只读目录树，不动文件）
/media/115/电影/流浪地球.strm      ← 文件内容是一行 URL
      ↓ Emby 扫描这个目录
      ↓ 用户点播放（Emby 把 URL 交给播放器，或自己 302）
http://NAS:6086/d/{pickcode}.mkv?/流浪地球 (2019).mkv
      ↓ 本服务查 115 拿到临时直链，返回 302
https://cdn.115.com/...             ← 播放器直连 115，服务器不转发流量
```

所以这个服务的带宽消耗约等于零、磁盘占用约等于零（只有几十 KB 的 `.strm`）。
它唯一的重活是**遍历网盘目录树**，而那件事有严格的风控约束（见 §5）。

---

## 1. 当前状态（诚实版）

### 已经做完并**在真机上验证过**的

| 能力 | 验证方式 |
|---|---|
| 115 扫码登录（真实请求 115 接口，返回二维码 PNG） | 手工 curl |
| 115 Cookie 登录 + 账号落库 | 真机，用户已用自己的 Cookie 加过账号 |
| 浏览网盘目录（列出根目录 5 个文件夹） | 真机，`PAN115_CID` 自动列举功能跑过 |
| 全量同步 → 写 `.strm` | 假网盘集成测试 + **真机跑出 25 个 `.strm`** |
| 网页内设置 / 修改管理员账号密码 | 真机：首次设置 200 → 重复设置 409 → 错密码 401 → 改密后旧会话失效 |
| 「媒体服务器」一键连接（填 Emby 账号密码自动建 API Key） | 真机，建出 API Key 并写进 settings；明文 key 不再回传前端 |
| `sync` 子命令（不经过网页跑一次全量同步） | 真机容器内跑通，25 个文件 |
| 302 直链播放 | 真机 `/d/{pickcode}` 从 Emby 容器里 `wget`，302 → 115 CDN 返回 200 |
| **`.strm` 被 Emby 真正入库** | **真机：`电影` 库（movies 类型，`/media/movies`）入库 25 条**，含中文目录、`{tmdbid=…}`、全角冒号、`Season 1/2` 剧集 |
| Emby 启动向导自动完成 | **真机临时容器**，四步接口全部实测通过 |
| Emby API Key 自动创建 | 同上，实测 `POST /Auth/Keys` 返回 204 无 body、需从列表回读 |
| Emby 建库 + 触发扫描 | 真机实测；**建库必须 `refreshLibrary=true`，否则永远扫不出片**（见 5.7） |
| strm115 与 Emby 共用媒体卷 | 真机两容器互读同一文件 |
| Docker 降权（PUID/PGID） | 真机 `docker top` 确认主进程 uid=1000，`.strm` 权限 0644 |
| 一次性引导（compose 里的 bootstrap 服务） | 真机对临时 Emby 完整跑通 |
| `POST /api/find`：IMDb 链接 → 名称/年份/评分/平台深链 | 真机 `tt1160419` → `Dune / 2021 / 8.0`，平台 7 个（前端页面已下线，接口保留） |
| `POST /api/find`：磁力链 → `dn=` 洗出片名 → IMDb 反查 | 真机 `Dune Part Two 2024` → `tt15239678` |
| 离线任务列表 | 真机 115 返回 3000 条，进度越界 0 条，字段齐全 |
| **离线任务提交** | 真机：重复链接 → 422「任务已存在」（证明请求拼装与回包解析都对）；成功分支用 httptest 假服务器覆盖 |
| 通知推送 | 真机：本地 webhook 收到「测试消息」与「同步完成」两条真实推送 |
| 设置页密钥不回传明文 | 真机 `GET /api/settings` 只有 `imdb_api_key_set/masked` |
| **刮削：真机整库跑通** | 23 条台账 → 4 部作品 → 全部匹配；`实际请求 IMDb 0 次 · 命中缓存 4 次`（第二次） |
| **刮削：磁盘产物** | `.nfo`（含单集标题 `Let You Down`）/ `-poster.jpg` / `-fanart.jpg` 与 `.strm` 同目录 |
| **字幕：真机整库跑通** | 23 条 → 22 条直接命中中文字幕（OpenSubtitles），1 条无字幕源 |
| **字幕：翻译链路** | 真机《肖申克》（该源无中文）→ 下载英文 → Index-Translate 翻译成中文 srt，译文质量可用 |
| **刮削页前端（Vue）** | 种子数据 + 临时实例，Chrome 截图亮/暗两套：控制台、统计、筛选、结果表（海报缩略图/IMDb 链接/状态徽标）、配置卡、重刮弹窗；console 零错误 |
| **按 ID 重刮单条（真机）** | `tt15239678` → 8.4s 写入 nfo/海报/背景图到 .strm 同目录；第二次 0.97s（全命中本地缓存、0 请求） |
| **字幕 zht 语种** | 真机《流浪地球2》：OpenSubtitles 只有 `zht`（繁体），修复前被当「无中文」去翻译（>120s），修复后 4.1s 直接落盘 2785 条 |
| **我的海报墙（真机，连真实 Emby）** | 临时实例 + 数据库副本：4 个 Emby 条目（3 电影 + 1 剧集 20 集）、21 条本地台账；搜索 / 电影剧集筛选 / 类型标签 / 评分排序 / 分页 / `source=local` 切换全部通过；图片代理返回 73KB JPEG |
| **海报墙增删改（真机）** | 「改」→ 200，nfo 实际改写（含新标题与类型）；「删」→ 200，清掉 5 个本地文件 + 1 行台账、默认不动 115；「增」scan → 200，`rel_dir` 按库根正确换算；全程未触碰生产数据与 Emby 条目 |
| **剧集目录前缀富集** | Emby Series 的 Path 是剧目录、台账是一集一行：修复前 `in_local=false`，修复后 nfo / 字幕 / 20 集全部对上；单测 `TestCatalogSeriesEnrichment` |
| **海报墙前端（Vue）** | 临时实例 + Chrome 截图亮/暗两套 + 详情抽屉 + 编辑弹窗 + 添加弹窗；4 张真实海报，console 零错误 |
| **任务中心（真任务）** | 真机 `GET /api/tasks` → `running[{kind:scrape,phase:meta,0/23}]`、`summary{running:1,history_total:4}`；`POST /api/tasks/run {kind:scrape,refresh:true}` → 202 并真起一轮；取消 → 409（没在跑）与 200（在跑）都验过 |
| **演员头像 + 中文名（真 Emby + 真 TMDb）** | nfo 写出 `<name>朱一龙</name>/<role>/<thumb>`；Emby `People` 从 `Yilong Zhu / Tony Ka Fai Leung` 变 `朱一龙 / 梁家辉`，`/Items/{id}/Images/Primary` 从 **500** 变 **200 image/jpeg 21353 字节**；前端详情抽屉 12 个头像 `{"n":12,"loaded":12}` |
| **剧集演员表** | `tvshow.nfo` 与单集 nfo 都有 `<thumb>`；导演从 9 个（aggregate_credits 把每集分镜都算导演）收敛到 3 个 |
| **通知 10 个事件（真机 TG）** | 真发 `🏷 刮削完成 · 电影`、`💬 字幕补齐 · 电影` 均 `ok=true`；带封面入库卡片走 multipart `sendPhoto` 也 `ok=true`（本地海报 60KB 直传，无公网地址） |
| **通知事件升级迁移** | 真机老配置（存了 4 个事件）启动后自动补齐为 10 个，日志 `通知事件已补齐（新增事件默认开启…） added=6` |
| **TMDB 反代全链路** | 真机填 `https://tmdb.example.com/3`：`GET /api/settings` 回显 `upstream{api:/3, image:/t/p}`；电影墙 200 且海报变成 `.../t/p/w342/...`（真图 45172 字节）；订阅 `seasons` 200；网络检测两条 host 都是反代域名（401/404 均判可达） |
| **订阅追更闭环（真 TMDB + 真资源搜索）** | 真机建订阅《神探夏洛克》→ TMDB 校正出 `tt1475582` / 12 集；轮询器自动跑一轮「搜到 103 条，没有匹配 S01E01 且满足画质的」，水位线不动、退避到 10 分钟后；`POST /{id}/run` 同步返回同一结论；暂停 / 改周期 / 删除三个接口都 200。`max_size=1` 故意把结果全筛掉，**零下载**验完整条链路 |
| **播出日历（真 TMDB，TVDB 可选）** | 真机临时容器（独立数据库、走用户反代 `https://tmdb.example.com/3`）：冷抓 `11.8s / 110 条 / 7 天都有内容 / 48 部在播剧`，含《法律与秩序》S28E05 11-05、《辛普森一家》S38E06 11-08；二次 `0.004s` 且 `cached=true`；未配 TVDB 时 `/api/calendar/tvdb/test` 如实返回「没有配置 TVDB API Key」、日历照常出；`/calendar` SPA 200。TVDB 的登录 / Token 缓存 / 补时刻 / 补独有集 / 失败降级由假服务器单测覆盖 |
| **播出日历改 TVMaze 默认源（真 TVMaze 公共 API）** | 真机临时容器（独立卷、独立管理员、TMDB 走用户反代）：`POST /api/calendar/tvmaze/test` 真打官方当日排期 `54 条 / 2.6s`；默认 `tvmaze` 冷抓 `14.0s / 110 条 / 7 天 / 90 条补上 air_time+network`，`sources.source=tvmaze`、`tvmaze=true`、带 CC BY-SA 署名，二次 `0.003s cached=true`；切 `off` → `8.8s / 110 条 / 0 条有时刻`；切 `tvdb` 无 Key → `8.8s / 110 条` + 提示换 TVMaze；把 `tvmaze_base` 指向连不上的地址 → `8.9s` 仍返回完整 110 条，只写 `tvmaze_error` + `notice`。设置项校验：非法 `calendar_source` 400、`TVDB` 归一成 `tvdb`、无 scheme 的 `tvmaze_base` 400、测试按钮坏地址返回中文原因 |
| **刮削改纯 TMDB（真 TMDB 反代 + 真落盘）** | 真机临时容器 `strm115-tmdb-test`（独立卷、独立管理员、TMDB 走用户反代 `https://tmdb.example.com/3`）：整库刮削 `5 条 / 4 组 / 匹配 5 · 未匹配 0 · fetched 9 · 13.5s`；第二次 `fetched 0 · cached 4`；落盘 `movie.nfo` / `poster.jpg` / `fanart.jpg` / 同名 nfo / `tvshow.nfo` / 两集 `episodedetails` 全齐，单集标题是 TMDB 中文「向导」「猫在袋子里……」；nfo 含 `uniqueid type="imdb"` + `type="tmdb"`、中文演员 `<name>/<role>/<thumb>`、中文导演，图片地址全指向反代 `/t/p/`。`POST /api/catalog/item {tmdb_id}` 连改两次 ID 都真重刮（`693134 → 157336 → 27205`），标题/评分/演员/IMDb ID 全部跟着换 |
| **海报墙改 TMDB 默认源（真接口 + 真浏览器）** | 同一临时容器：`GET /api/catalog` 不传 source 返回 `source=tmdb`，冷抓 `2.5s / 4 张卡 / tmdb_missing=0 / emby_checked=false`；三条定位路径都真跑通 —— `tmdb_id` 直查（沙丘2 693134、绝命毒师 1396）、`imdb_id → /find`（星际穿越 → 157336）、片名+年份搜索（盗梦空间 → 27205）；二次 `0.0035s`（30 分钟缓存）；`source=local` 显示本地旧评分 1.0 / 旧类型，`source=emby` 未配时回落 local 并给中文原因；详情 `source=tmdb` 用同一份 TMDB 数据，不带 source 则是台账旧值。无头 Chrome + CDP 真点击：三个来源按钮默认 TMDB、数据源徽标「TMDB」、4 张海报 `loaded=true`（真从反代下载）、详情抽屉 TMDB/IMDb 外链正确且**不显示** Emby 徽标、编辑弹窗有 TMDB ID 与 IMDb ID 两个输入框、切 Emby → 提示 + 本地台账、切回 TMDB → 徽标复原，`exceptions=[]` |

### **没有**验证过的（改到这里要格外小心）

1. **播放器端真正播起来**：只验证到 Emby 拿到 302 并可读 CDN 响应头，没在电视/手机上点过播放。
2. **夸克网盘驱动**：接口预留了，实现是空的。
3. **Emby 侧操作反查网盘**（双向同步）：未开始。
4. **115 增量事件**：未接入，目前只有手工全量同步。
5. **Jellyfin**：代码里共用同一套接口，但只在 Emby 4.10.2 上验证过。
6. **前端「电影墙详情弹窗」「设置子页」与「消息通知」卡片**：脚本过了 `node --check`，接口都真机调通，
   但没有在浏览器里逐项点过（排版/交互可能有瑕疵）。
7. **通知的真实渠道**：只验了通用 Webhook（本地 node 收的）。企微/钉钉/TG/飞书/
   QQ/IYUU 这 7 个渠道没逐个真发过，改协议时按 `notify` 包的单元测试走。
8. **离线任务「自然下完」的全链路**：提交成功后的轮询/自动同步/推送链路用假网盘
   集成测试覆盖，真机上没等到一个磁力真的下完（几十分钟起）。
9. **中文字幕的命名匹配**：OpenSubtitles 返回的中文有 `zho/chi` 等写法，
   `langCode` 里都认。但**简繁没有区分**（`chs/cht` 都当中文收），遇到简繁混排
   的片源可能拿到繁体字幕 —— 没有真机样本，先不动。
10. **翻译接口的速率上限**：Index-Translate 是 B 站免费公共接口，整部电影的
    字幕（1500+ 条 cue）翻译一轮要一两分钟、几十次请求。没测过它的极限，
    并发默认 2，别往上加。
11. **Emby 对 nfo 的实际识别**：nfo / 图片 / `.zh.srt` 的文件名规则按 Kodi/
    Emby 文档写的，本机没有 Emby 实例，没做过「加库后界面上长什么样」的验收。
12. **真实 TVDB 凭据**：用户还没提供 Key / PIN，`POST /login` 与
    `/series/{id}/extended` 只在 httptest 假服务器上跑过。填完点设置页的
    「测试连接」即可验真；没填时日历本来就该降级，不算故障。
13. **播出日历页面在浏览器里的逐项点击**：接口、构建产物与 SPA 路由都验过，
    但没逐项点过筛选 / 翻周 / 抽屉（排版细节可能还要微调）。

---

## 2. 目录地图

```
cmd/server/main.go            进程装配：两个 HTTP 服务 + 优雅退出 + bootstrap 子命令
internal/
  config/config.go            环境变量 → Config（只管启动期必需的东西）
  store/store.go              SQLite：建表与账号/媒体库/台账/设置（手写 SQL，无 ORM）
  store/runs.go               同步流水账 sync_runs + 台账反查 + 按库开关
  pan/pan.go                  「网盘」抽象：Driver 接口、错误分类、注册表
  pan/manager.go              ★ 唯一出口：串行 + 节流 + 重试 + 熔断
  pan/driver115/driver.go     115 实现（基于 SheltonZhu/115driver）
  panpool/pool.go             按账号缓存 Manager（节流状态必须跨请求保持）；
                              Start(ctx) 每分钟把「今日调用数」落一次库
  sync/sync.go                全量遍历 → 比对 → 写/删 .strm → 通知 Emby
  strm/strm.go                路径净化、.strm 内容生成、原子写
  play/proxy.go               /d/{pickcode} → 302；直链缓存；IP 限流
  mediaserver/mediaserver.go  媒体服务器抽象（Server 接口）
  mediaserver/emby.go         建库 / 刷新 / 列库
  mediaserver/emby_admin.go   一次性动作：启动向导、管理员登录、API Key
  httpapi/server.go           路由 + 中间件 + 会话表
  httpapi/auth.go             管理员账号密码（PBKDF2）、首次设置、登录、改密码
  httpapi/handlers.go         业务接口
  httpapi/catalog.go          我的海报墙：Emby 列表/详情/图片代理 + 本地台账兜底
  httpapi/catalog_manage.go   海报墙增（115 目录选片）/ 改（nfo 重写）/ 删（按链逐段清理）
  httpapi/files.go            网盘文件：真实浏览（带台账状态）+ 移动 + 删除 + 目录相对路径反推
  httpapi/strm.go             Strm 管理：总览 / 流水账 / 孤儿扫描与清理 / 按库 keep_ext
  httpapi/dashboard.go        总览面板：库/台账/刮削统计 + 在跑的任务 + 最近入库（不探 Emby）
  httpapi/calendar.go         /api/calendar（七天周历）+ /api/calendar/tvdb/test
  calendar/calendar.go        ★ 播出日历聚合：TMDB 剧单/分集 + TVDB 增强 → 按天分组 + 整周缓存
  tvdb/tvdb.go                TVDB v4 最小客户端：/login 换 Token、/series/{id}/extended
  tmdb/airing.go              日历专用接口：on_the_air / airing_today / 剧详情 / 分季 / 本周电影
  httpapi/tasks.go            ★ 任务中心：三类内存 Job 摊平成一张表 + 取消 / 重跑 / 清空流水账
  scrape/edit.go              人工改元数据：重写 nfo + 台账 + 双键缓存，全程不联网
  mediaserver/catalog.go      Catalog 可选接口（列条目/详情/图片/删除）
  mediaserver/emby_catalog.go Emby 实现；GET /Items/{id} 在 4.10 上 404，取单条要用 /Items?Ids=
  httpapi/web.go              内嵌前端（SPA history 路由）；前端没构建时给「未构建」说明页
  httpapi/webdist/            Vue 构建产物（go:embed 输入，仓库只留 .gitkeep）
web/                          Vue3 + Vite 管理界面（src/pages 13 个页面、api/、stores/）
  components/LibraryWall.vue 海报墙标签：网格/筛选分页/详情抽屉/编辑/删除/从 115 添加
  components/ScrapeConsole.vue 刮削与字幕标签：选库开跑/进度/按 ID 重刮/刮削与字幕配置
  pages/LibraryPage.vue       「影视库」：海报墙 + 刮削与字幕，标签记在 ?tab=（发现片单已移回发现与订阅）
  pages/FilesPage.vue         网盘文件：真实 115 懒加载目录树 + 入库状态 + 移动/入库/删除
  components/DirTreeNode.vue  递归目录节点（文件页的导航树与移动目标选择共用）
  pages/SyncPage.vue          Strm 管理：状态/配置/全量/增量/孤儿清理，全部接真接口
  pages/TasksPage.vue         任务中心：进行中 / 历史 / 整理记录三个页签（3 秒轮询只在进行中）
  bootstrap/bootstrap.go      ★ 一次性引导：把手工步骤全自动化
  bootstrap/strm.go           管理 API 的瘦客户端（bootstrap 用它)
  linkparse/linkparser.go     从任意文本抠链接（磁力/ed2k/.torrent/InfoHash/IMDb）+ 洗片名
  resource/resource.go        ★ 资源搜索调度：并发各源、独立超时、结果统一排序
  resource/telegram.go        TG 公开频道：t.me/s/<ch>?q= 页面解析（认 115 分享码）
  resource/torrent.go         内置四站：海盗湾 JSON / Nyaa·动漫花园·Mikan RSS + 通用 RSS 解析
  resource/sites_html.go      HTML 站：UIndex（表格直给磁力）/ TheRarBG（翻详情页补磁力）
  resource/cilimao.go         磁力猫：关键词 base64 + 结果 payload 解 atob + slug→infohash
  resource/custom.go          自定义源：解析 `名称 | 地址`，认 Torznab 的 attr，能出磁力
  resource/test.go            单源探活：连没连上 / 解析出几条 / 失败原文，地址里的密钥打码
  imdb/imdb.go                IMDb 查询：官方 GraphQL / 免费 suggestion。只剩资源搜索换名兜底
                              与设置页检测在用，刮削主链路已完全不走这里
  tmdb/tmdb.go                ★ 唯一元数据上游：榜单/搜索/详情（Key 来自设置页或环境变量）
                              ★ SetUpstream 进程级上游（atomic.Value）：api 根 + image 根，
                              NewClient 与 imageURL 都读它；全项目图片地址只有这一个出口
  tmdb/meta.go                ★ 单部作品：SearchMeta / MetaByID / MetaByIMDbID /
                              SeasonEpisodesMeta；scrape 与海报墙共用
  tmdb/credits.go             ★ 演员：按 Meta.TMDbID 直取 credits?language=zh-CN
                              拿中文名 / 角色名 / 头像 profile_path
  httpapi/catalog_tmdb.go     ★ 海报墙 TMDB 源：按台账 tmdb_id 富集当页卡片 + 30 分钟缓存
  watchhub/client.go          Stremio 插件客户端（历史遗留，刮削主链路已不走）
  watchhub/fetcher.go         Detail（meta + streams 并发）/ Hot / 类型纠偏
  find/find.go                链接/片名识别编排（页面已下线，POST /api/find 仍在用）
  offline/service.go          ★ 离线任务提交 + 45s 轮询 + 终态回调（OnDone）
  notify/client.go            推送总入口：按渠道分发、日志、测试消息、SendTo 指定渠道
  notify/channels_*.go        8 个渠道各自的签名/加解密/回包校验
  notify/telegram_discover.go getMe + getUpdates 反查 Chat ID（409 webhook 冲突单独翻译）
  httpapi/notify_ingest.go    ★ 刮削完成 / 新片入库通知：台账 diff + 集数/画质/体积 + 本地海报
  httpapi/notify.go           事件清单与开关（notify_events）、事件升级迁移、异步扇出
  tgbot/bot.go                ★ Telegram 接收端：长轮询 + 游标持久化 + 白名单（先到先得）
  tgbot/handler.go            意图路由：115 分享 → 转存；磁力/ed2k → 离线；片名 → 搜索
  tgbot/render.go             回复文案（HTML 转义、入库信息、失败原文）
  tgbot/api.go                Bot API 六件套：getMe/getUpdates/send/edit/answer/setMyCommands
  organize/organize.go         自动整理规则：识别（复用 scrape.Parse）→ 目标目录 → 跳过判定
  httpapi/organize.go          /api/organize/*：扫描（只读）→ 执行（真移动）→ 台账
  store/organize.go            organize_records 台账（一个网盘条目一行，覆盖写）
  httpapi/find.go             /api/find、/api/offline/*
  httpapi/offline.go          离线完成回调：推通知 + 触发对应媒体库同步
  httpapi/notify.go           /api/notify*，密钥打码与「留空不修改」、单渠道测试、TG 会话探测
  httpapi/bot.go              机器人的接线：把转存/离线/搜索作为函数注入 tgbot，白名单读写
  tmdb/tv.go                  ★ 剧集详情：/tv/{id} 季清单 + last/next_episode_to_air + IMDb ID
                              （season 0 特别篇剔除；last 为空时用季集数兜底 Latest()）
  store/subscription.go       ★ subscriptions 表：水位线、状态、画质/体积、next_check_at、退避
  subscribe/pick.go           从搜索结果里挑一条：SxxExx / 季包匹配 + 画质 + 体积 + 种子数
  subscribe/service.go        ★ 常驻轮询器：每分钟扫 due → 串行检查 → 提交 → 推进水位线
  httpapi/subscribe.go        /api/subscriptions/* + 检查周期设置；建剧集订阅时真查 TMDB 校正
  pages/MediaTransferPage.vue 订阅页签（真接口）+ 订阅设置；WallBrowser 抽屉里加「订阅」按钮
  stores/subscriptions.ts     ★ 订阅索引（tv:1396 / movie:693134 → 订阅 id）：墙上与日历的
                              「订阅 / 已订阅 · 点此取消」共用它，日历「我的订阅」筛选也读它
  scrape/name.go              文件名解析：片名/年份/季集（含 1917.2019 这类坑）
  scrape/match.go             TMDB /search 结果打分选最佳（年份贴近 + 标题相似 + 类型一致）
  scrape/service.go           ★ 纯 TMDB 刮削编排：tmdb_id / /find / 片名 → 缓存（三键）→ 落盘；
                              RescrapeOne 与 RescrapeOneByTMDbID 共用 rescrapeWith
  scrape/nfo.go               Emby/Kodi 的 .nfo 生成（含单集标题 + imdb/tmdb 双 uniqueid）
  scrape/cast.go              ★ 演员富集：按 TMDbID 取 credits → Meta.CastDetail → nfo 的 <thumb>
                              （失败静默降级，不影响刮削主流程）
  scrape/root.go              ★ 库路径解析：跨环境搬库（容器 → Windows）的兜底
  subtitle/source.go          OpenSubtitles（Stremio 插件）字幕列表查询
  subtitle/fetch.go           补字幕编排：中文优先 → 英文翻译 → 原子落盘
  subtitle/subtitle.go        SRT/VTT/ASS 保真解析与序列化（只动文本不动时间轴/标签）
  translate/api.go            Index-Translate 客户端（prompt 构造对齐官方脚本）
  translate/batch.go          长字幕批量翻译 + 序号对齐 + 失败行回退原文
  store/media.go              media_meta / meta_cache 两张表 + 查询
  store/api_calls.go          api_calls 表：当日 115 调用次数（覆盖写）+ 清理旧天
docker/entrypoint.sh          按 PUID/PGID 对齐属主后降权
setup/1-install.ps1           Windows：装 WSL2 + Docker Desktop（需管理员）
setup/2-deploy.ps1            Windows：起服务 + 自动填播放域名
scripts/dev.ps1               本地开发：不构建镜像
.github/workflows/ci.yml      vet / test / 静态编译 / compose 校验
.github/workflows/docker.yml  推镜像到 GHCR（tag 时双架构）
```

规模约 **15,500 行 Go（生产）+ 6,200 行测试 + 5,000 行 Vue/CSS**（旧版 1,700 行单文件 HTML 已删除）。
（Vue 界面是「外壳先行」：页面、交互、设计系统都齐了，接口层 `web/src/api/endpoints.ts`
按域写全，标 ✅ 的已通、标 🚧 的等后端补齐。）

---

## 3. 一次同步的完整流程（改代码前必读）

`sync.Start()` 在 goroutine 里跑，HTTP 立刻返回 202 + 任务快照。

```
1. scan    递归 ListPage 遍历网盘目录树（每页 200，Manager 控制节奏）
           对每个视频文件：算出 relPath（sanitize + CleanRel）→ 记录到 seen 集合
2. write   与台账（files 表）比对：
             新文件 → 生成 .strm 内容 → 原子写（.tmp → rename）
             路径变了 → 覆盖写新路径，旧路径进待删列表
3. clean   台账里存在、但这次没见到的行 → 删 .strm + 删台账行
           （DeleteStale=false 时跳过，用于「网盘被误删但想保住本地」）
4. emby    如果有接入 Emby，触发一次库刷新
5. done    写 libraries.last_sync_at / last_sync_files
```

进度通过 `Job.Snapshot()` 暴露（`phase/dirs/files/written/removed/failed`），
前端 2 秒轮询 `/api/libraries/{id}/job`。

**关键：`seen` 集合决定「什么算被删除」**。写这个逻辑时最容易出的错是
「某次 ListPage 临时失败 → 文件被误判为删除 → 用户库里少了一半」。
所以遍历失败是**整体失败**（phase=error），不会进入 clean 阶段。

---

## 4. 数据模型（8 张表）

```sql
accounts   (id, kind, name, credential, created_at, updated_at)
           kind = "115"；credential 是 Cookie 串（JSON 或 k=v 串），**不出现在任何 API 响应里**
libraries  (id, account_id→accounts, name, remote_cid, remote_path,
            local_path, collection_type, keep_ext,
            server_name, server_lib_id, last_sync_at, last_sync_files, created_at)
files      (id, library_id→libraries, account_id, file_id, pick_code,
            rel_path, name, size, updated_at)
           ★ (library_id, file_id) 唯一；rel_path 是相对 local_path 的路径
settings   (key, value)   -- 键值对，见 §6
media_meta (id, library_id→libraries, rel_path, kind, title, year, season, episode,
            imdb_id, meta_title, meta_year, rating, runtime, genres, actors, director,
            overview, poster_url, backdrop_url, poster_file, backdrop_file,
            nfo_file, sub_file, sub_lang, sub_state, status, message, updated_at)
           ★ (library_id, rel_path) 唯一；status = ok / unmatched / error
           ★ Episodes（单集清单）**不在这张表里**——一季几十条，进库只占地方；
             它活在 meta_cache 的 JSON 里，写 nfo 时从 Meta 原样带过去
meta_cache (cache_key, imdb_id, payload, updated_at)
           cache_key 有两种：`imdb|tt15239678`（有 ID 时优先用这个）与
           `kind|normalizeTitle|year`；payload 是 scrape.Meta 的 JSON。
           一次查询会往两个键各写一份，两条路径互相预热
           ★ 这张表就是「避免风控」的本体：同一部作品只真正请求一次上游
           ★ payload 里的 `cast_detail`（演员中文名/角色/头像）是后加的字段：
             老缓存没有它 → nfo 只能写 `<name>`；补上要重刮一次或勾「忽略缓存」
sync_runs  (id, library_id, library_name, trigger, phase, files, written,
            removed, failed, skipped, message, started_at, finished_at)
           trigger = full / incremental / auto；phase = running / done / error / canceled
           ★ 开跑就插一行、结束补统计。Strm 管理页的「最近轮次 / 上次全量」读它，
             不读内存里的 Job——Job 只反映当前这轮，重启即丢
api_calls  (day, account_id, n)   PRIMARY KEY (day, account_id)
           day 是**本地时区**的 `2006-01-02`（不是 UTC：晚 8 点之后的调用会算到明天）。
           n 是「这一天这个账号调了多少次 115」，由 panpool 每分钟 UPSERT **覆盖**写一次
           —— 覆盖而不是累加，所以重启、重复 flush 都不会翻倍计数。
           内存里的当前值在 pan.Stats.Today，启动时由 SeedToday 把当天的数接回来
```

设计取舍：
- **不用 ORM**。8 张表、40 来个查询，手写 SQL 让「扫全表」这种事一眼可见。
- **files 表就是台账**。它记录的是「上一轮同步认为应该存在哪些 .strm」，
  是判断增量的唯一依据。删掉它不会丢媒体，但下一次同步会变成「全量重写」。
- `libraries.local_path` 可能是**别的环境的路径**（如容器里的 `/media/movies`）。
  直接拿它写文件在 Windows 上会落到 `D://media//movies`（当前盘符根），
  所以凡是要写库目录，一律走 `scrape.ResolveRoot`（约束 5.10）。
- `media_meta.poster_url/backdrop_url` 存远程地址、`*_file` 存本地相对路径。
  预览走 `/api/media/{id}/image`，不把媒体库目录直接挂成静态目录。

---

## 5. 不能破坏的约束

这些不是「最佳实践」，是**破坏了会出事故**的东西。改代码前请先读一遍。

### 5.1 所有网盘写操作必须经过 `pan.Manager`

`internal/pan/manager.go` 是唯一出口。业务代码拿不到裸 `Driver`，只能拿 `Manager`。
原因是它把三件必须同时发生的事物理绑在一起：

1. **串行**：同账号同时只有一个在途请求
2. **节流**：请求间隔 ≥ 1.2 秒（115 风控严格，这是防封号参数，不是性能参数）
3. **熔断**：判定风控后冷却 10 分钟，期间一个请求都不发

漏掉任何一条都会导致封号或临时封禁。**不要为了「快一点」调小 `MinGap`。**

### 5.2 Manager 必须按账号池化

`panpool.Pool` 缓存 `Manager`。如果每次请求都新建 Manager，
节流状态（上次请求时间、冷却截止时间）就丢了 —— 等于「每次都是第一次请求」，
限流形同虚设。`pool.Evict(id)` 只在删账号时调用。

### 5.3 `.strm` 必须原子写

先写 `xxx.strm.tmp` 再 `rename`。直接写目标文件的话，进程被杀会留下半截文件，
Emby 会当成一个 0 字节的坏条目，而且**不会自动修复**。

### 5.4 `.strm` 权限必须是 0644

写的是宿主目录，Emby 容器的 uid 和本服务的 PUID 经常不一致。
用 0640 时表现为「Emby 扫到了库但一个条目都没有」，极难排查。
`.strm` 里只有一行播放 URL，没有凭证，放宽到 0644 是安全的。

### 5.5 直链缓存的 key 必须带 UA

115 的直链**按 UA 签发**。用 A 播放器的 UA 拿到的链接，换 B 播放器会 403。
`play/linkCache` 的 key 是 `pickcode + UA`，不要「优化」成只按 pickcode 缓存。

### 5.6 前端只有一层：`web/`（Vue，需构建）

界面是 `web/` 下的 Vue3 + Vite 工程，构建产物落 `internal/httpapi/webdist`，
由 `web.go` 的 `//go:embed all:webdist` 打进二进制，`GET /` 提供 SPA
（history 模式：`/sync`、`/settings` 这类路径服务端找不到文件就回 `index.html`，
否则一刷新就 404）。

上一版的单文件 `index.html`（挂在 `/legacy`）已经删除 —— 两套 UI 共用同一批接口，
留着等于每个改动都要维护两遍，排查时还容易把老页面当成新页面。现在 `webdist`
缺失时 `GET /` 回 503 说明页并打印 `scripts/web-build.sh`，不再静默降级。

约束：

- `webdist` 是 `go:embed` 的**编译输入**，编译前目录必须存在。仓库里只提交
  `webdist/.gitkeep`，产物由 `scripts/web-build.sh` 或 Dockerfile 的 node 阶段生成。
  没跑过前端构建直接 `go build` 不会失败，但 `/` 会退回旧界面。
- 运行时依赖只有 `vue` + `vue-router`，样式全手写（`web/src/styles`）。
  **不要引 UI 组件库**：现有设计系统（tokens / panel / field / tabs…）已经够用，
  引一套进来第二天就会有两套按钮、两套圆角。
- `index.html` 只做兜底，新功能不要再往里加；两个界面同时改同一功能是最贵的维护方式。

### 5.7 Emby 侧：库类型必须是 movies，建库必须带扫描

这两条是花了很久才定位出来的，**改回任何一种「看起来更合理」的写法都会让库永远是空的**。

**（一）不能用 `collectionType=mixed`。**
Emby 4.10.2 的「混合内容」库扫 `.strm` 的表现是：目录结构全部建出来
（连 `Season 1/2` 都有），但一条影片都没有，日志里连 `FFProbeProvider`
都不出现，也不报错，`Scan media library` 报告 0 秒完成。
真机对照实验：

| 库根 | 类型 | 结果 |
|---|---|---|
| `/media/movies`（25 个 `.strm`，中文目录 + `{tmdbid=…}` + 全角冒号 + Season） | `movies` | **25 条全部入库** |
| 同一个库 | `mixed` | **0 条**（目录 12 个，影片 0 个） |
| `/media/中文库`（1 个 `.strm`） | `movies` | 1 条（中文库根没问题） |

所以 `mediaserver.EmbyCollectionType` 把 `mixed` 一律降级成 `movies`。
库里同时有电影和剧集时也这么建，Emby 仍会按 `Season N` 认出集数。

**（二）建库必须 `refreshLibrary=true`。**
建库那一下是 Emby **唯一一次**真正递归读目录、跑 ffprobe 把 `.strm` 变成影片的
时机。用 `refreshLibrary=false` 建的库，之后无论调多少次 `POST /Library/Refresh`
或 `POST /Items/{id}/Refresh` 都只补目录、不补影片 —— 因为那些目录项已经在库里，
`ValidatePhysicalRoots` 直接跳过整棵子树。真机验证过：把 `/media/movies` 的子目录
项从 `library.db` 里删干净再扫，结果是目录又全建回来、影片依然 0 条。

**（三）已知的 Emby 自身 bug**：`DELETE /Library/VirtualFolders` 会 500
（`System.NullReferenceException` at `RemoveVirtualFolder`），所以测试库删不掉。
真机上的清理办法是停掉 Emby，直接删 `emby-config/data/library.db` 里
`type=4`（CollectionFolder）那一行以及它在 `ItemLinks2` / `ItemExtradata` /
`AncestorIds2` 里的记录，再启动。**别把这个当正式功能做**。

### 5.8 离线提交不许退回 SDK 的 `AddOfflineTaskURIs`

115 已经把老的 `lixianssp/?ac=add_task_urls`（RSA 加密通道）关了，现在恒定回
纯文本 `decode fail!`。SDK 还在这条路上，会把那段文本当 JSON 解，于是报错变成
**误导性的** `invalid character 'd' looking for beginning of value` —— 看着像
解析 bug，实际是接口没了。

正确的路径写在 `internal/pan/driver115/offline.go` 顶部注释里：
`GET /?ct=offline&ac=space` 取 `sign/time` → `POST /web/lixian/?ct=lixian&ac=add_task_url`
（一次一条）。改动前务必看函数头那段注释，那是一次完整踩坑的记录。

### 5.9 通知配置的密钥只能「留空 = 不修改」

`mergeNotifySecrets` 是这件事的**唯一**实现点。前端渲染时密钥框永远是空的，
回传空串表示「别动它」。任何一处把「空串」当「清空」处理，用户保存一次别的字段
就会把 TG token 静默抹掉，而且不会有任何提示。

### 5.10 写库目录必须走 `scrape.ResolveRoot`，不许直接用 `lib.LocalPath`

库的 `local_path` 是**用户数据**，可能是从 Docker 里带出来的 `/media/movies`。
这在 Windows 上不是绝对路径，`filepath.Join` 之后落到「当前盘符的根」
`D://media//movies` —— 于是同步的 `.strm` 在项目 media 目录、刮削的海报在
D 盘根下另一个同名目录，Emby 读不到海报，日志一句错都没有。

真实踩过：第一次真机刮削 53 个文件全部写歪，靠对比磁盘才发现。
规则：**所有**要往媒体库里写文件的代码（刮削/字幕/以后任何功能），
取根目录一律 `scrape.ResolveRoot(lib.LocalPath, mediaRoot)`，
httpapi 里已经有现成的 `s.libRoot(lib)`。

### 5.11 刮削的请求量纪律（纯 TMDB 版）

刮削的每一个 HTTP 请求都要过这几道闸，改之前先想清楚加不加得：

- **meta_cache**：同一部作品最多查一次。缓存键同时写 `tmdb|<id>`、`imdb|tt…`
  与 `片名|年份` 三份；**没有 `tmdb_id` 的老缓存一律判失效重查**，否则
  IMDb/Cinemeta 时代的结果会冒充 TMDB 结果。
- **分组**：一季 24 集共用剧级 meta，逐集查等于把请求量放大 24 倍。
- **单集清单**：只请求文件名里真正出现过的季号（`/tv/{id}/season/{n}`），
  不许为了单集标题再发每集一次的请求。`meta_cache` 按 `episodes_seasons`
  判断覆盖；后来加了 S02 会自动补拉，别改回只看 `episodes_fetched`。
- **演员**：详情已经带回 `TMDbID`，**直接按 ID 取 credits，不再绕 `/find`**，
  固定 1 次/作品；结果连同 `cast_detail` 进缓存，命中缓存时 0 请求。
  类型必须由 `Meta.Kind` 直接推（`movie` → `/movie/{id}/credits`、
  `series` → `/tv/{id}/aggregate_credits`），不能借 `pickCreditsID(_,0,0)`
  反推 —— 那条路径在两个 id 都是 0 时会把电影也判成 tv，演员表静默变空。
  剧集 crew 按集合并，**有 Creator 就只用 Creator**，否则去重后截 3 个。
  任何一步失败都静默降级（`log.Debug`），不许让刮削挂掉。

`tmdb_api_key` 留空时用 `tmdb.DefaultKey` 兜底（值来自 `STRM115_TMDB_KEY`
环境变量，源码不内置 Key）：只有 TMDB 一家上游，返回 nil 客户端会让
没填 Key 的用户整库刮不动。TMDB 整体失败就记 `unmatched`，
**不许再回退 Cinemeta / IMDb**。

### 5.12 「加入媒体库」的落盘目录必须由 ParentID 反推，不许按名字剥前缀

网盘里同名目录可以套好几层（`电影/电影/华语电影`），而库的 `remote_path`
可能只记了最后一级。前端如果拿它去剥前缀，会算出 `电影/华语电影/…` 这种多一层
的路径：`.strm` 落在别处、台账和下次同步各记一份，Emby 里出现两部重复的片。

正确做法（`GET /api/libraries/{id}/reldir`）：从 `cid` 往上 `Stat`，用 115 自己
记的 `ParentID` 走到 `lib.RemoteCID` 为止。目录名做 `strm.Sanitize`，和 `sync`
拼 `rel_path` 的口径保持一致。

### 5.13 全量与增量不许写成两套实现

两者只差 `sync.Options.Force`：为真时跳过「内容没变就不写」的判断，让所有
`.strm` 的 mtime 都刷新一次（Emby 会整库重扫）。写成两套遍历逻辑，早晚会出现
「全量能删失效、增量不能」这类不对称 bug，然后没人记得哪套是对的。

### 5.14 演员头像必须写进 nfo 的 `<thumb>`，别指望 Emby 自己补

Emby 里「演员没有头像」的根因不在 Emby，在我们写出来的 nfo。这些人物条目是用本地
nfo 直接创建的，**没有外部 ID**（`ProviderIds: null`），Emby 无从知道他们是谁，
也就不会去 TMDb 抓图；没有 `<thumb>` 就永远是一个灰底占位图，
`/Items/{personId}/Images/Primary` 直接返 500。

所以 `<actor>` 必须是三件套：`<name>`（中文名）+ `<role>` + `<thumb>`（
`https://image.tmdb.org/t/p/w185/...`）。改这里时注意：
- `CastDetail` 要**从 `Meta` 一路带到写盘**（`nfoMeta()` 就是为此存在的）——
  曾经在 `writeArtifacts` 手拼 `Meta` 时漏掉这个字段，缓存里有、nfo 里没有，排查了半天
- 没配 TMDb Key 时退回只有 `<name>` 的老格式，**不要**为了凑 thumb 去编一个地址
- 已有 nfo 的「不覆盖用户手改」判断要留例外：旧文件有 `<actor>` 但一个 `<thumb>` 都没有、
  而新内容确实带 thumb 时才重写（`needsCastThumb`），否则老库永远补不上

### 5.15 刷新 Emby 元数据要连剧根目录一起刷

`RefreshPaths` 是**按目录粒度**的：只刷「文件所在目录」时，剧集只刷到 `Season 1`，
Series 条目（读 `tvshow.nfo` 那个）根本不动 —— 结果就是单集里有中文名和头像、
剧详情页还是英文名加灰头像。所以刷新路径集合必须同时包含
`scrape.ShowRoot(relPath)`（剧根）。加新的刷新入口时照这个来：
**内容写在哪个目录，就刷哪个目录**，一个 `.strm` 的目录不等于它的元数据目录。

### 5.16 通知文案：一行标题 + 最多三行数字

用户在手机上瞄一眼就要看懂发生了什么。所以每条推送的格式固定成：

```
✓ 同步完成 · 电影
目录 12 · 视频 23 · 写入 4 · 清理 0
```

四条纪律：

- **标题带状态符 + 事件 + 主体**（`✓` / `✕` / `⏳` / `📂` / `🏷` / `🆕` / `💬` / `🗑`），
  正文只放数字。不要写「尊敬的用户，您的媒体库……」。标题最多一行。
- **「新片入库」必须靠台账 diff**：刮削是整库跑的，按「本轮跑过的条目」推会把
  老片重推一遍。开跑前 `st.MediaRelPaths` 存一份快照，跑完只认之前没有的剧根
  （`startScrape` 里做）。同一部剧的多集合并成一张卡，一次最多 3 张，
  其余合成「另有 N 部已入库」—— 首次整库刮削几十部不能变成刷屏。
- **Telegram 的封面用本地文件直传**（`Message.ImageFile` + multipart `sendPhoto`），
  **不要改回图片 URL**：海报在磁盘上没有公网地址，就算有也多半是带登录态的
  管理端接口，TG 拉不到。读不到图就退回纯文本，不许为了凑封面卡住请求。
- **加新事件必须同时改 `notifyEventsSinceV2` 和 `notifyEventsVersion`**。
  只加 `notifyEventLabels` 的话，老用户存的 `notify_events` 里没有它，
  升级后会静默收不到，而界面上看起来是「没关过却关着」—— 这是最难自证的一类 bug。

### 5.17 订阅：数据源是 TMDB，水位线只能在提交成功后推进

播出日历（yysub 那类）回答「今天哪些剧更新」，`internal/subscribe` 要回答
「我订的这部剧该出到第几集」。前者是人工排的周表，HTML 里没有 TMDB ID，
中英文名还粘在一个 `title` 属性里（`嗜血法医：杀魔复生Dexter: Resurrection`）；
拿它当数据源就得先做一次必错的剧名→ID 对齐。所以只用 TMDB 的
`last_episode_to_air` + 每季 `episode_count`（特别篇 season 0 必须剔掉，
否则集数口径会串）。

- **watermark 的语义是「已经提交到第几集」，不是「已经入库」**。提交离线到落盘
  隔着几十分钟，页面显示的是前者。`SetSubscriptionWatermark` **只能在
  `Submit` 返回成功后调用**；存不上要显式报错（"已提交但水位线没存上"），
  否则下一轮重复下载同一集，而且是静默的。
- **一轮最多 3 集（maxCatchUp）**，且按集顺序挑、遇到没资源的就停。跳过中间
  一集去下后面的，水位线就没法单调推进。`pendingEpisodes` 要多算一集来判断
  「后面还有没有」，否则截断后 `len(picked)==len(pending)` 看着像追平了。
- **串行**，单 goroutine + 1 分钟心跳扫 `next_check_at <= now`。115 离线上传对
  并发敏感；同一条订阅用 busy 表挡住「手动 run 撞上自动轮询」。
- **三种间隔**：新集没资源 → 10 分钟重试且 `fail_count` 归零（刚播完没片源不是
  故障，算失败会把真出错时的退避抬高）；追平且有 `next_episode_to_air` →
  `nextCheckAfterAir` 等到播出日次日；其余按 `sub_interval_minutes`。
- **提交目录必须兜到 `res_save_cid`**（`Service.target`）。没指定媒体库时下到 115
  根目录，自动整理根本看不见，闭环就断在最后一环。网页「搜到就存」和 TG 机器人
  也是这么兜底的，最后都落到 `"0"`。
- **提交走 `Server.submitOffline` 这一个函数**，不要给订阅另写一套账号挑选 /
  目录解析 / 错误翻译。`handleOfflineAdd` 里那条 `offline_add` 通知在 handler 层，
  订阅这条路径暂时不推 —— 下一轮要补通知时别在这里偷偷加一份重复实现。
- **轮询器用可取消的 ctx 启动，`Close()` 必须 cancel**（`Server.subCancel`）。
  常驻 goroutine 停不掉，容器重启和单测收尾都会漏。

### 5.18 TMDB 上游必须是进程级统一配置

三处会各自 `tmdb.NewClient`：`httpapi/wall.go`（两处）、`scrape/cast.go`、
`httpapi/subscribe.go`。**不要只在其中一处设 `c.Base`** —— 用户配了反代之后会出现
「订阅快、电影墙超时、头像还是官方地址」这种最难自证的组合。

- `tmdb.SetUpstream(api, image)` 用 `atomic.Value` 存两个根，`NewClient` 读 api、
  包级 `imageURL()` 读 image。后者覆盖海报 / 背景 / 演员头像 —— 全项目的 TMDB
  图片地址只有这一个出口，改它一处就够。
- **启动和 `PUT /api/settings` 保存后各调一次 `applyTMDBUpstream`**。只写库不刷新，
  得重启容器才生效；`scrape/cast.go` 那个缓存 Client 的复用 key 必须带上 api 根，
  否则改完设置它还指着旧地址。
- **`tmdb_base` 留空时图片根必须回到 `image.tmdb.org/t/p`**，不能按 `/3` 后缀推导：
  官方接口域和图片域是两个域名，硬推会拼出一个不存在的地址。`TestSetUpstream` 盯着。
- 反代地址校验 `validTMDBBase` 要**收下空串**、拒绝没 scheme 的地址（拼成相对路径后
  `http.NewRequest` 不报错、发出去才失败）。带 `?` / `#` 的也拒掉。
- `netcheck.Targets()` 的 TMDB 两条从 `CurrentUpstream()` 取，别写死官方域名。
  判定标准是「拿到 HTTP 响应就算通」，反代给的 401 / 404 也照样绿。
- `GET /api/settings` 额外回显 `tmdb_upstream{api,image}`（不是设置项，是生效值）：
  图片根可能是推导出来的，界面上得能核对一次。

### 5.19 资源搜索：来源切换只做本地过滤，缓存按「源」而不是按「页面」

老 bug：在「全部」里搜完，点一下「种子站」chip，前端又发一次
`GET /api/res/search?kind=torrent`，七个种子站 + 三个频道全部重打，
`t.me` 还容易被自己限流成超时。

- **一轮把所有源都搜回来**，`kind` 参数留给后端兼容（订阅仍会传 `torrent`），
  但网页和机器人都不靠它切来源。`useResSearch` 里 `kind` / `minSize` 都是
  `computed` 上的本地过滤，`run()` 不再接收 `kind`。
- 后端 `Service.src` 是**源级缓存**，key = 源标识 + 该源真正用的查询词 + limit + 代理。
  按源缓存而不是缓存整个 Result，才能让「全部 → 只搜 TG」也复用已有结果。
  TG 60 秒（t.me 限流窗口），种子站 5 分钟；**失败写 30 秒负缓存** —— 否则
  只要有一个源持续挂着（比如 UIndex 被 Cloudflare 挡），每次重复搜索都要重等
  它一轮超时，`Result.Cached` 也永远是 false（`TestCachedSourceFailureHasShortTTL` 盯着）。
- `Options.Refresh` 要一路穿透到 `telegram()` 的内层页面缓存。只绕外层的话，
  「重搜」会拿到 60 秒前的频道页，用户以为按钮坏了（`TestSearchChannelQueryAndCache` 盯着）。
- `Status.Cached` / `Result.Cached` 必须回显，否则 0ms 会被当成某个站特别快。

### 5.20 换英文名走 TMDB，TG 频道也要拿英文名再搜一遍

- `tmdb.SearchOriginal` 用 `/search/multi` 拿 `original_title` / `original_name`，
  比 IMDb 自动建议接口准；**查不到要退回 IMDb**，不能让冷门片直接没英文名。
  两处都有超时（3s / 4s），失败一律返回空串，只影响搜索质量不影响搜索本身。
- `TitleHit{Name, Year}` 分开：**种子站用带年份的词**（压同名重拍片），
  **TG 频道用 `stripYear()` 去掉年份的纯名字**（帖子标题通常不写年份，
  带年份的 `?q=` 会把本来命中的帖子全过滤掉）。只切末尾四位年份，
  「2001: A Space Odyssey」这种中间的数字不能动。
- 一个频道中文、英文两次 `?q=` 必须**在同一个 goroutine 里串行**：
  t.me 对同 IP 并发敏感，丢进全局 sem 会自己把自己测成超时。
  结果按 URL → 磁力 → 标题去重；`sortItems` 要对多个查询词取最高分，
  否则英文标题的结果会被中文词压到列表底部。
- 新站 `uindex` / `therarbg` 是 HTML 页解析，不引 DOM 库：按内容特征
  （`magnet:` href、`/details.php`、`td class`）抓，比按列序号抗改版。
  UIndex 的 `search.php` 可能被 Cloudflare 整条路径拦下（`index.php` 反而能开），
  `isCloudflare()` 要明确报错，别伪装成「0 条结果」。
  TheRarBG 列表没磁力，必须翻 `/post-detail/`，限 12 条 × 4 并发。

### 5.21 播出日历：TMDB 出剧单与日期，TVDB 只能做增强

> 本节描述的是 `calendar_source=tvdb` 这条老路径的约束，规则仍然成立；
> **默认增强源已在 §5.24 换成免 Key 的 TVMaze**，TVDB 现在只是可选项。

- TVDB v4 的 OpenAPI 里**没有 `/schedule`**，不能「按日期反查这周播什么」。
  剧单必须来自 TMDB `/tv/on_the_air` + `/tv/airing_today`；分集日期靠
  `/tv/{id}/season/{n}`；电影靠 `/discover/movie` 的 `primary_release_date.gte/lte`。
- `candidateSeasons()` 只对「最新/下一集落在本周」或「季首播落在本周」的剧翻分季，
  每部最多 2 季。别改成「所有季都翻一遍」：48 部剧会变成上百次请求，冷启动直接超时。
- TVDB 的 `airsTime` / `airsDays` / `originalNetwork` 是 TMDB 没有的字段；
  `episodes[].aired` 只能补 TMDB 缺的集，**同集一律以 TMDB 的 SxxExx 和中文集标题为准**
  （两边季集编号常不一致）。TVDB 失败只写 `sources.tvdb_error` + `notice`，
  绝不把整张日历弄成 502；没配 Key 时连客户端都不必构造。
- 整周缓存落在 `meta_cache`，key = `calendar:<YYYY-MM-DD>`，`payload` 自带 `cached_at`
  用来判 TTL（本周 30 分钟、其它周 12 小时）。`refresh=1` 跳过读但仍写回。
  `calendar.Service` 必须常驻在 `Server` 上，和资源搜索的 `s.res` 同理。
- `tvdb_api_key` / `tvdb_pin` 属于 secret：留空 = 不修改、只回 `_set`/`_masked`。
  测试连接要**优先用请求体里刚填的凭据**，否则用户填完点测试测的还是库里旧的那份。
- 前端筛选（我的订阅 / 剧集 / 首播 / 新剧 / 电影）只在本地重画，
  一次接口拿满七天；只有翻周、回本周、重抓会带新的 `start` / `refresh`。

### 5.22 资源搜索：展示条数 ≠ 每源条数，频道清单只收 username

真机回归：用户问「怎么没有显示种子站结果」，截图里机器人 8 条全是 `@QukanMovie`。
查下来不是一个 bug，是三件事叠在一起：

- **`maxResults=8` 以前同时当「每源 limit」和「展示上限」，再 `res.Items[:8]` 硬截**。
  一个 TG 频道一次就能给 8 条 115 分享，把磁力全挤到列表外。现在拆成
  `searchLimit=20`（每源）+ `pageSize=8`（一屏），中间用 `balanceOrder()` 按
  `kind` 分两队**交错排**（种子站先），队内保持相关性排序；整轮结果留在内存里翻页。
- **`Status.OK=true` 但 `Count=0` 的源不等于「没搜」**。以前机器人只列 `!OK` 的源，
  于是 Nyaa / 动漫花园 / Mikan 对一部美剧英文名返回 0 条时一个字都不提。
  `siteSummary()` 必须把「N/M 个站有结果」或「都没匹配」写进消息。
- **`tg_channels` 里混进了频道显示名**：「Torrent Search Bot 🔎」不是 username，
  按空格拆成 `Torrent` / `Search` / `Bot` / `🔎` 四个不存在的频道，每个真打一次 t.me，
  白占 `sem` 的 6 个并发槽并把种子站拖慢。`NormalizeChannels()` 只丢「含空格且带非 ASCII」
  的整条 —— 纯 ASCII 的空格分隔仍要支持；前端 `parseChannelSetting()` / `addChannel()`
  用同一套规则，别只改一边。`t.me/s/xxx` 要去掉 `s/` 前缀。
- **`res_sites` 存过值之后，新增内置站永远不会自动生效**。启动时的 `migrate()` 用
  `res_sites_version` 逐步迁移：四站 → 六站（UIndex / TheRarBG）→ 七站（磁力猫），
  每步只改「精确等于上一版默认值」的行；用户自己勾过的配置一个字都不能动，
  特意取消的站也不能被下一个版本塞回来（`TestMigrateAppendsClmOnly` 盯着）。
- 「如何投稿：」这类说明帖会命中 `?q=`，但没磁力 / 没 115 分享 / 没网盘链接，
  解析层按标题前缀 + 三条全空才丢，别改成只看标题（会误伤真资源帖）。

### 5.23 设置页的「获取地址」与日历抽屉里的资源搜索

- 「获取地址」是给 Key 申请页用的，不是给接口根用的：TMDB
  `themoviedb.org/settings/api` / `/signup` / `developer.themoviedb.org/docs` /
  `github.com/imaliang/tmdb-proxy`；TVDB `thetvdb.com/dashboard/account/apikeys` /
  `support.thetvdb.com/kb/faq.php?id=81` / `thetvdb.com/subscribe` /
  `thetvdb.github.io/v4-api/`。每个地址都带复制按钮 —— 有些环境禁外链跳转。
- `tvdb_base` 是**接口根**（不是密钥）：留空保存 = 恢复官方 `api4.thetvdb.com/v4`，
  和 `tvdb_api_key` / `tvdb_pin` 的「留空不修改」相反。校验复用 `validAPIBase`
  （`validTMDBBase` / `validTVDBBase` 只是换了示例串）。
- `calendar.Deps.TVDBBaseFunc` 每轮现读设置，接口根变了**必须重建 Client**：
  Bearer Token 是那个域签发的，沿用旧 Client 会一路 401。静态 `TVDBBase`
  仍保留给单测指 httptest 假服务器。
- `POST /api/calendar/tvdb/test` 的 `base` 用 `*string`：要区分「没传（读库）」
  和「传了空串（按官方测）」。只改地址不重填 Key 时，也要用「新地址 + 库里
  已存的凭据」测，不能走 `s.cal.Ping` 的旧缓存。
- `GET /api/settings` 回显 `tvdb_upstream.api`（留空时是官方默认），别让界面
  显示一个空串，用户会以为「没在用」。
- 播出日历抽屉的「搜索资源」**不许再 `router.push('/media-transfer')`**：
  结果就地展开（`useResSearch` + `use115Share`，和电影墙抽屉同一套），
  带关键词输入、来源/体积本地筛选、存到 115 / 离线下载 / 复制磁力。
  点的是某一集但按**整部剧名**搜，带 SxxExx 反而常常搜不到；需要限定集数
  让用户在结果区输入框里自己补。换卡片要 `res.reset()`，别把上一部片的结果
  挂在新标题下面。完整资源搜索页只保留为一个次要文字链。

### 5.24 播出日历的默认时刻源是 TVMaze，TVDB 只留作可选

真机回归：用户反馈「tvdb 已经获取不了 api 了」。核对 TheTVDB v4 现状 ——
Key 按项目授权 / 用户订阅 PIN 发放，个人免费申请已不可用，
设置页原来那几条「申请 v4 Key」链接对用户不再实用。
于是把日历增强源做成可插拔，默认换成 **TVMaze 公共 API**（免 Key）：

- `calendar_source` 只能是 `tvmaze` / `tvdb` / `off`。`calendar.source()` 对空值
  和拼写错误一律兜成 `tvmaze`（老库没有这一行时自动走新默认）；
  `PUT /api/settings` 白名单校验非法值直接 400，别静默少一列时刻。
- TVMaze 有**两种 JSON 形状**：`/schedule` 是顶层 `show`，`/schedule/web` 是
  `_embedded.show`；`rawEpisode.show()` 必须同时兼容，只解析一种会丢掉流媒体排期。
  公共 API 限流至少 20 次 / 10 秒，`Week()` 并发固定 3、429/5xx 最多重试 3 次、
  按天缓存 30 分钟；单天失败只丢那一天，其余天数照常返回并带 `firstErr`。
- **只补字段、不新增条目**：TVMaze 不能反查剧单，剧单 / 日期 / `SxxExx` 永远以
  TMDB 为准。对齐三级 `tvdb_id → imdb_id → NormalizeShow(剧名)+S/E`，只填空的
  `AirTime` / `Network` / `EpisodeTitle` / `Runtime`，中文集标题优先 TMDB。
  别改成「按 TVMaze 排期新增卡片」——两边季集编号不一致会直接错集。
- `Entry.IMDbID` 是这轮为对齐新加的，别拿它当主键（主键仍是 TMDB ID + SxxExx）。
- 数据 **CC BY-SA**：`Sources.Attribution` / `AttributionURL` 只在 TVMaze 真正
  成功时才写，日历页必须渲染成可点外链；失败或 `off` 时不许挂署名。
- `tvmaze_base` 是**接口根**（不是密钥）：留空保存 = 恢复官方，和 `tvdb_api_key`
  的「留空不修改」相反。Client 按「反代根 + 代理」缓存，别每次请求都新建，
  否则按天缓存全丢。`POST /api/calendar/tvmaze/test` 的 `base` 用 `*string`，
  区分「没传读库」和「传空串按官方测」，与 TVDB 测试按钮同一套语义。
- 整周结果连 `sources` 一起进 `meta_cache`。**切来源后缓存没到期时返回的仍是
  旧来源标记**，这是预期行为；要立刻看差异点「重抓」（`refresh=1`），
  别误判成设置没保存。
- 增强源失败只写 `sources.*_error` + `Week.Notice`，绝不 502。
  真机：把 `tvmaze_base` 指向连不上的地址，日历仍返回完整 110 条。

### 5.25 刮削与海报墙是纯 TMDB，IMDb ID 只作标识

用户明确要求「海报墙和刮削改为 TMDB」，选的是**全量切换 + 纯 TMDB**：
不再请求 Cinemeta / IMDb 任何服务（IMDb 客户端只留给资源搜索换英文名兜底
和设置页「检测 IMDb」）。改动前先确认了 `internal/imdb` 还有别处调用，
不能顺手删包。

- **IMDb ID 仍然是主键之一**：文件名里的 `tt…`、`meta_cache` 键、
  nfo 的 `<uniqueid type="imdb">`、海报墙的 IMDb 链接都靠它。
  现在它由 TMDB 详情的 `append_to_response=external_ids` 顺带带回，
  TMDB 没登记时可能为空 —— 所以 `Meta.TMDbID` 才是真正的定位主键，
  `uniqueIDs()` 会把 `default="true"` 从 IMDb 让给 TMDB。
- **旧缓存必须失效**：`resolveOne` 只接受 `m.TMDbID > 0` 的缓存，
  其余一律重查。`media_meta` 用 `ensureColumn` 补了 `tmdb_id` 列，
  `scanMedia` / `UpsertMedia` 的列顺序与占位符必须一一对应，
  改列时最容易漏这个（列数不对会全库写不进去）。
- **nfoMeta 必须透传 `TMDbID`**：`nfoMeta()` 从台账行重建 `Meta`，
  漏一个字段不会报错，只会让 nfo 里的 `<uniqueid type="tmdb">` 凭空消失。
  线上排查过同类问题（`CastDetail` 漏传导致演员头像全灰），别再犯。
- **`enrichCast` 的类型由 `Meta.Kind` 直推**：见 5.11，`pickCreditsID`
  只在「老缓存走 /find」那条分支里用，用来对齐 /find 返回的 ID 属于哪个端点。
- **海报墙默认 `source=tmdb`**：卡片骨架永远来自本地台账，TMDB 只覆盖
  展示字段。只为当前页富集、并发 4、详情缓存 30 分钟；查不到**保留本地卡片**
  并写 `tmdb_missing` + `hint`，绝不让整面墙变空或 502。
  `Key` 保持 `local:<media id>`，编辑 / 删除 / 补字幕链路不受影响。
- **TMDB 模式不查 Emby**：响应里 `emby_checked=false`，前端据此**隐藏**
  「Emby 已收录 / 未收录」徽标 —— 没查过就不能假装知道。
  想看媒体服务器状态要显式切 `source=emby`。
- **改 TMDB ID 是认错片的自助修正入口**：编辑弹窗新增 `tmdb_id` 输入，
  后端 `catalogUpdateReq.TMDbID` 优先于 `imdb_id`，走
  `RescrapeOneByTMDbID` → `groupKey.tmdb` → `c.MetaByID`，
  一次详情请求完成重刮。两个重刮入口共用 `rescrapeWith`。
- `internal/httpapi` 测试里的 TMDB 上游是**进程级配置**：`New()` 会按
  settings 重置成官方地址。假服务器必须像真实设置页那样先写
  `tmdb_base` / `tmdb_image_base` 再 `applyTMDBUpstream()`，
  否则单测会打到真 TMDB（订阅测试里已经踩过这个坑）。

### 5.26 磁力猫（clm）：关键词和结果都要先解一层编码

用户给的是 `http://clm10.xyz/search?word=56m65p6q` —— 直接把这个地址当搜索
入口是行不通的，那个 `56m65p6q` 不是随机串，是 `base64("空枪")` 去掉 `=` 填充。

- **关键词必须先编码**：`?word=` 收的是 base64（`RawStdEncoding`）。
  直接传中文 / URL 编码的中文，站点回一张「keyword error」跳转页（HTTP 200），
  看起来像站点挂了。`clmWord()` 只做这一件事，别在后面对着字面量调试。
- **结果不在 HTML 里**：服务端把整段列表先 URL 编码、再 base64，塞进
  `window.atob("…")` 由前端 `innerHTML` 进去。所以 `decodeClmPayload()` 是
  解析的前置步骤；解不出来时原样返回（站点哪天改成服务端直出，正则还能用）。
  **URL 解码要用 `url.PathUnescape` 而不是 `QueryUnescape`** —— 标题里有
  「国粤多音轨+中文字幕」这种加号，`QueryUnescape` 会变成空格，
  前端用的是 `decodeURIComponent`，两者不等价（`TestParseClmKeepsPlus` 盯着）。
- **slug 就是 infohash 的 base32**：`/information/savdhddo655biz…` 解 base32
  正好是 20 字节 `902A338C…`，所以磁力能直接拼（带 `dn` 和 tracker，115 更认），
  不用像 TheRarBG 那样逐条翻详情页 —— 一次搜索只打一个请求。
  站点若把 slug 换成随机串，`base32InfoHash()` 返回空串，
  由 `fillMagnetsFromDetails()` 退回翻详情页兜底（它现在会跳过已有磁力的条目，
  TheRarBG 也共用这个函数，别改回「按索引前 12 条」）。
- **入口域名会换**：`clm10.xyz` / `clm02.xyz` 这类兄弟域名轮换，只钉在
  `cilimao.go` 的 `clmBase`，换域名改一行。它**只有 HTTP**，别改成 https。
- 它给的是 DHT 全量索引，英文泛词（比如 `The Odyssey`）会混进不少无关/成人
  条目 —— 排序仍按「标题命中关键词 → 有磁力 → 做种 → 时间」，真搜片名时
  相关条目会浮上来；这不是解析 bug。
- 站点本身响应约 2.3s（换任何客户端都一样），比其它站慢，但和其它源并发，
  整体搜索耗时不看它。缓存 5 分钟照旧生效。

### 5.27 搜索结果按钮带标题 + 翻页；入库后自动整理接上刮削

两件事其实是同一个抱怨的两面：「列表和按钮对不上，而且只能看 8 条」。

**按钮带标题、一行一个。** 以前按钮只有 `🧲 1 · 站点 · 体积`，标题在消息里，
手机上要来回对照；翻到第 3 屏更分不清。现在 `pickLabel()` 拼
`图标 + 序号 + 标题(截断) + 体积`，整标签压到 **52 字**以内（Telegram 单按钮 64，
但 emoji 在部分客户端算两份）。砍标题不砍体积 —— 同一部片的高清/枪版就是靠体积区分的。
消息那侧相应精简成**一行一条**：标题 + `站点 · 💾 · 🟢 · 📅`，去掉 🔴 和「磁力/仅链接」
两个标签（图标已经在按钮上）。

**翻页。** `pick` 不再是 `[]resource.Item`，而是 `{query, res, items, page, msgID}`：

- `items` 是 `balanceOrder()` 排好的**整轮**结果，`view()` 按 8 条切一屏；
- 回调 `p:<n>` 里的 `n` 是**全局序号**，不是页内下标 —— 否则第 3 屏第一个按钮
  会去点第 1 条；
- `pg:<page>` 原地 `editMessageText` 换一屏，**不消耗状态**（还能翻回来接着点）；
- 状态按 `(chat, messageID)` 存：同一个会话里躺着两次搜索时，点旧消息的按钮
  仍然按「当时那一屏」入库，不会拿新结果顶包（`TestPickIsScopedToMessage`）。
  这也顺手修了老实现「只按 chat 存、后一次搜索覆盖前一次」的错位。
- 键盘末尾：`◀ 上一页 | 2/3 | 下一页 ▶` + `❌ 都不是，重新搜`；第一页没有「上一页」，
  中间那个按钮只回一句「当前就是这一页」（`pgnow`）。

**入库后自动整理（真机报的 bug）。** 用户问「TG 通知入库后怎么没有刮削、自动整理」，
查下来是三件事：

1. 库里 `organize_config.enabled=false` —— 自动整理根本没开，而且代码里整理**只有**
   网页面板那一个入口，从来没在入库后跑过；
2. 整理页配的「待整理目录」是 `…/Season 1`，而 TG 转存落在**资源保存目录**
   （`res_save_cid`，这里是 `/StrmStation/冗余`），两个目录根本不是一回事；
3. 刮削只在**同步完成**之后自动触发（`autoScrapeAfterSync`），而同步又只在下到
   媒体库目录时触发 —— 落在资源目录的转存永远等不到这一环。

接法：

- `handleOrganizeRun` 的执行体抽成 `runOrganize(ctx, cfg, sourceCID, fileIDs)`，
  网页和自动链路共用；`Server.organizeMu.TryLock()` 撞车直接返回 `errOrganizeBusy`（409），
  **不排队**：两轮同时移动同一批文件很容易在 115 侧挪到一半。
- `autoOrganizeAfterIngest` 的源目录**固定用 `res_save_cid`**，不看整理页那个
  「待整理目录」：入库的东西按定义就在资源目录，而媒体库目录里的文件不该被搬走。
  整理完 `runOrganize` 自己 `beginSync` → `watchSyncAndNotify` → `autoScrapeAfterSync`
  → `chainSubtitlesAfterScrape`，通知依次是「整理完成 → 同步完成 → 刮削完成 → 🆕 入库」。
- 触发点四个：TG 转存（`botDeps().Transfer`，等 3s 让 115 落盘）、网页转存
  （`handleSaveShare`，仅当目标是默认资源目录）、机器人/网页离线（`onOfflineDone`，
  下到的目录**没有**对应媒体库时）、订阅（走离线，同一个回调）。
  下到媒体库目录的离线任务仍走原来的「直接同步」，不经过整理。
- `organize.Config` 加 `AutoIngest *bool`（`auto_ingest`）：指针是为了区分
  「老配置没这个字段」（`nil` → 默认开）和「用户主动关掉」。`Filled()` 里不许把
  `false` 掰回 `true`（`TestOrganizeAutoIngestDefaultsOn` 盯着）。

⚠️ 生产库里整理页的「待整理目录」和 `res_save_cid` 不一致时，**自动整理只认后者**；
手动「执行整理」仍按页面配的那个目录走。想在面板里也整理新下载的东西，把待整理目录
选成资源保存目录。

### 5.28 入库卡片没封面：扇出层根本没走「富媒体」这条路

用户反馈「入库通知有片名有体积，就是没有封面」。海报其实一直在磁盘上
（`<片名 (年份)>/<发布组>/poster.jpg`），`posterPath()` 也把它找出来了，问题
出在 `notify.Client.send()`：

```go
if rc, ok := ch.(RichChannel); ok && m.Image != "" {   // ← 只认公网 URL
    return rc.SendRich(ctx, &cfg, do, m)
}
return ch.Send(ctx, &cfg, do, m)                        // ← 永远走这里
```

入库卡片只填 `ImageFile`（本地路径，家用部署海报不可能有公网地址），`Image`
恒为空 —— 于是 `SendRich` 一次都没被调用过，`sendPhotoFile()` 成了死代码。
条件改成 `m.Image != "" || m.ImageFile != ""` 即可（`TestClientRichUsesLocalImageFile`
盯着，测的是扇出层而不是渠道层）。

顺带修两件让这个 bug 藏了这么久的事：

1. **降级不再静默**。`telegramChannel.SendRich` 传图失败时会退回纯文本并
   `return nil`，日志里只有一条 `✓`，看起来一切正常。现在返回
   `*FallbackError`，`fanOut` 认这个类型：`Result.OK = true`（正文确实发到了）
   但日志补一句「已退回纯文本：<原因>」。别把它当失败处理 —— 那会让
   「图没传上去」和「消息没发出去」混成一件事。
2. **传图时限放宽到 45 秒**（`withTimeout`，只作用于 `sendPhotoFile`）。
   实测这台机器上 31KB 的海报也会撞 15 秒超时，于是卡片时有时无。文本消息
   仍用配置里的 15 秒，快速失败对文本更有价值。

### 5.29 资源保存目录巡检：手动转存没人管

「入库后自动整理」原来只挂在四个事件上（TG 转存、网页转存、离线完成、订阅），
全都是**程序自己发起**的动作。用户在 115 App 里手动转存到 `/StrmStation/冗余`
时没有任何事件，文件就一直躺着 —— 这正是「转存进去以后到底有没有后续流程」
的答案：程序内转存有，手动转存没有。

`watchIngestFolder()`（`server.go` 里和 `janitor` 一起起）补上这个缺口：

- 启动后 **90 秒**先看一轮（重启前积压的东西不用再等 5 分钟），之后每 **5 分钟**一次；
- 开关复用 `auto_ingest`，不新增配置项 —— 它表达的是「落进资源目录的东西就该被收走」，
  至于怎么进去的用户并不关心；
- 有离线任务在下载时跳过（`offline.Watching() > 0`）：115 正在写文件，这时候搬等于搬一半；
- 撞上网页正在跑的那一轮不报错，`errOrganizeBusy` 降为 debug 后等下次巡检；
- 没东西可搬时只记 debug，不会每 5 分钟刷一条 Info。

另外修了 `runOrganize` 的一个隐患：**batch 名额被「跳过」的条目吃掉**。
`handled++` 原来在识别之前，资源目录里攒了 30 个 `.nfo/.srt` 之后，同批里真正的
电影永远轮不到（跳过是在本地算的、不花 115 请求，却占了一格）。现在只有
真要搬的才 `handled++`。

---

### 5.30 「Emby 没同步进来」：镜像目录和库扫描路径对不上

用户报的现象：整理跑完、同步报「完成」、TG 卡片里也写了「已同步」，Emby 里却
一条新片都没有。

**根因不在同步，而在两个路径被当成同一件事**：

- 应用的库 `电影`：`remote_cid` = 115 上的 `电影` 目录，`local_path` = `/media/movies`，
  所以 `.strm` 写的是 `/media/movies/<片名>/`；
- Emby 的库「电影」：扫描路径是 `/media/movies/电影`（当年建库时按 `remote_path` 拼的）。

也就是说本地镜像目录**比库的扫描根更浅**，整理把新片放到 `/media/movies/空枪 (2026)`，
正好落在两个库的扫描范围之外。Emby 日志里只有一句
`DirectoryNotFoundException: /media/movies/电影`，页面上完全看不出来。

同一时期还有两个放大器：

1. `organize_config` 里 `movie_lib_id == tv_lib_id == 1`：剧集会被搬进电影库根目录，
   电影库里混进 Season 目录，Emby 里既不分类也刮不好；
2. 体积门槛只作用于文件：`空枪 (2026)` 这种只有一个 636 KB 广告壳的目录照样被
   当成电影搬进库，变成一张没有海报的灰卡片。

**改了什么**

- `sync.mirrorWarning()`：同步末尾核对 `lib.LocalPath` 是否被 Emby 那个库的扫描路径
  覆盖，不覆盖就把人话写进同步状态（「库扫的是 X，而 .strm 写在 Y」）并发一条警告日志。
  这是诊断，不是错误 —— 拿不到库列表时什么都不说，不能让一轮成功的同步变成失败。
  纯函数 `underAnyRoot()` 负责判断，`TestUnderAnyRoot` 盯着「库比镜像更细」这个方向。
- `organize.Recognize()`：体积门槛对目录同样生效（`Size == 0` 表示网盘没给值，放行）。
- `organize.Config.VerifyTMDB`（`verify_tmdb`，缺省 = 开）：搬之前先问一次 TMDB，
  `scrape.Pick` 打分过阈值才动；对不上的原样留在暂存目录（`/StrmStation`），
  台账里记「TMDB 里没找到（片名 / 年份对不上）」。网络 / 限流失败**不拦**——
  基础设施的锅不该让用户的文件留在原地，刮削阶段还有一次机会。
- `handleOrganizePutConfig`：电影库和剧集库选了同一个非 mixed 库时直接 400，
  不再让「整理说成功、Emby 里剧集跑到电影库」这种现场出现。
- 番剧归剧集（`reAnime`）：`新番 / 日番 / 番剧 / 连载 / OVA / OAD / anime` 当剧集信号，
  并从片名里剥掉（带着「新番」去 TMDB 搜不到任何东西）。刻意不收「动画 / 动漫」：
  「动画电影 你的名字」被当成剧集搬进 Season 目录就再也认不回来了。

**目录约定（这次一并落地的结构）**

```
115 /影视/电影        → 应用库「电影」→ 本地 /media/movies → Emby 库「电影」(movies)
115 /影视/剧集        → 应用库「剧集」→ 本地 /media/tv     → Emby 库「剧集」(tvshows)
115 /StrmStation/冗余 → 「资源保存目录」：下载 / 转存落点，也是自动整理盯的目录
```

动漫没有独立的库：番剧在 TMDB 里就是 tv，和剧集落在同一个 `Season N` 结构里。
认不出 / 刮不到的东西**留在暂存目录**，不搬、不删 —— 库里只放能刮到元数据的。

**这次迁移踩到的两个 Emby 坑（下次别再走一遍）**

- 删库接口 `DELETE /Library/VirtualFolders?Name=...` 在这套库里会抛
  `NullReferenceException`（`RemoveVirtualFolder` 里 `GetItemById` 返回 null）。
  原因是库的物理目录行（`MediaItems.type=3`，`Path=/media/movies/电影`）**父节点丢了**：
  它的 `ParentId` 是 `1`（root），不是库的 `3`。也就是说 Emby 的库一个物理目录都没有，
  扫描自然什么也扫不到 —— 这才是「DirectoryNotFoundException」背后的真因。
  修法是直接改 `emby-config/data/library.db`：把目录行的 `ParentId` 挂回对应的
  CollectionFolder、`Path` 改成真实的扫描根，重启后 Emby 自己会剪掉失效条目、补上新条目。
  （`POST /Library/VirtualFolders/Paths` 在 4.10.2 上会报 `Unrecognized Guid format`，
  `LibraryOptions` 更新返回 204 但 `PathInfos` 不生效，都别浪费时间。）
- 改 `library.db` 前必须 `docker stop emby` 并备份；改完 `docker start`，
  再 `POST /Emby/Items/{id}/Refresh?Recursive=true` 触发重扫。

**结果（真机）**：24 条 `.strm`（4 部电影 + 20 集），两个库全部入库、海报与简介齐全，
同步日志里不再有路径警告。

---

### 5.31 总览最下面的卡片错位：`auto-fit` 会排出 3+1

用户发了一张截图：总览四张小卡一排，最后一张被挤到第二行、只占一列宽，
下面那块「媒体库状态」大卡跟它左对齐 —— 看起来像「右边空了一格」。

根因在 `.grid-4` 用了 `repeat(auto-fit, minmax(200px, 1fr))`。`auto-fit` 只保证
「至少 200px」，**能塞几个算几个**：视口窄一点就变成 3 列，第四张掉到第二行；
而它继承的 `1fr` 是那一行的宽度，于是被人看成「错位」。这不是浏览器 bug，
是 `auto-fit` 的正常行为 —— 想要「等宽且排满整行」就不能让它自己决定列数。

**改法**：把列数写死成阶梯，让每一档都能整除。

```css
.grid-4 { grid-template-columns: repeat(4, minmax(0, 1fr)); }
@media (max-width: 1180px) { .grid-4 { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 560px)  { .grid-4 { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 380px)  { .grid-4 { grid-template-columns: 1fr; } }
```

顺带一个同类坑：栅格子项默认 `min-width: auto`，长表格 / 长片名会把整列撑宽，
在 `1fr` 的父栅格里表现为「整页横向滚动」。`minmax(0, 1fr)` 和
`.cols > * { min-width: 0 }` 是同一件事的两道保险，**页面上所有 `1fr` 都别裸写**
（这一轮把九个页面的 `grid-template-columns: 1fr` 全换成了 `minmax(0, 1fr)`）。

---

### 5.32 整理记录卡片重叠：六列表格在窄屏互挤

用户报「自动整理下面的整理记录卡片重叠」。实际不是卡片，是一张六列表
（状态 / 原文件名 / 识别结果 / 目标目录 / 时间 / 操作）：
窄屏下每列被压到几十像素，长片名换行、状态徽章和操作按钮互相叠在一起；
更难受的是表头 `thead` 有 `position: sticky`，滚动时浮在筛选条上面，
看起来就是「两块内容盖在一起」。

**改法**：这一页的记录条数不多、字段却很长，本来就不适合表格 —— 改成卡片行。

- 一行一条 `.rec`：左边 `.rec-main`（状态徽章 + 标题 + 置信度 / 原始名 / 目标目录 / 时间），
  右边 `.rec-side`（重试 / 忽略）；
- `≤720px` 时 `.rec { flex-direction: column; align-items: stretch }` ——
  **`stretch` 不能省**，column 方向下默认 `flex-start` 会让子项按内容宽度排，
  长片名直接顶出卡片右边；
- 标题 `text-overflow: ellipsis`，窄屏下截断在卡内而不是撑宽整页。

台账里「忽略 / 重试 / 清空」三个动作语义没变，只是从表格最后两列挪到了卡片右侧。

---

### 5.33 本地文件管理：和「网盘文件」是反着看的两件事

新页面 `web/src/pages/LocalFilesPage.vue` + `internal/httpapi/local_files.go`。

「网盘文件」读的是你登录的 115，回答「云端有什么」；「本地文件」读的是宿主
`MEDIA_ROOT`，回答「磁盘上真实落了什么」。**两者不能合成一页**：网盘里有的本地不一定有
（没同步），本地有的网盘里也不一定有（手放的海报、删完没跟上同步的 `.strm`、
发布组塞进来的广告壳）。这一页刻意不做重命名 / 移动 —— 本地镜像是同步生成的，
改个名字下一轮同步就还原，等于骗自己。

四个接口：

| 方法 | 路径 | 干什么 |
|---|---|---|
| GET | `/api/local/overview` | 每个库的落盘根、目录数 / 文件数 / 字节数 / 不在台账里的条数 |
| GET | `/api/local/browse?library_id=&path=` | 看一层，目录在前文件在后，每个文件带 `known` |
| POST | `/api/local/stat` | 真走 `filepath.WalkDir` 全量统计（60s 超时 / 5 万文件上限） |
| POST | `/api/local/remove` | 删单条或整目录 |

三个设计判断：

- **`known` 是这一页的灵魂**。台账里有这条 `rel_path` → 是同步写出来的；
  没有 → 本地多出来的。把「磁盘上有什么」和「媒体库该有什么」对上，
  用户一眼就知道哪些是真残留。
- **统计是显式动作**，不进首屏。遍历开销随库大小线性增长，进页面就 WalkDir
  会让「打开本地文件」这个动作在几十万文件的库上卡十几秒。
- **删除不改台账**，但要说清后果。台账是「应该有什么」、本地是「实际有什么」，
  删完下一轮同步会照台账再写回来 —— 所以台账内的条目删除后回执里明写：
  「已从本地删除；这条还在同步台账里，下一轮同步会重新写回来。要彻底不要，
  请先去 115 删掉，或把这个库的条目移除」。不说这句，用户会以为删了没用。

路径安全三道防线（`resolveLocal`）：拒绝绝对路径 → `path.Clean` 清掉 `..` →
`withinDir(root, p)` 复核结果仍在库根下。删库根本身直接 400；非空目录必须显式带
`recursive:true`，否则 409 并回一句「目录里还有 N 项」。
`TestLocalBrowseRejectsTraversal`（`../etc`、`..%2F..%2Fetc`、`/etc/passwd`）、
`TestLocalRemoveGuards`（400 / 409 / 台账内 hint）盯着这四条。

---

### 5.34 TG 搜索结果改成清单 + 回复序号

原来的实现是内联键盘：每页 8 个按钮，`◀ ▶` 翻页。问题有两个 ——
手机上一屏根本放不下 8 个按钮，而且用户想找的常常就是第 9 条之后那几个，
只能点翻页。

**改版**：不再挂键盘，整轮结果铺成一条清单，**回复序号入库**。

```
🎬 沙丘 2
<i>回复序号（比如 3）我就入库，30 分钟内有效。</i>

<b>1.</b> 沙丘2.Dune.Part.Two.2024.2160p...
🧲 12.0 GB · 🟢 328 · 🔴 12 · 📅 2024-03-01

<b>2.</b> ...
```

- `renderResults(query, res, items)` 取代了 `pageView` + `pageKeyboard`；
  每条两行（`<b>N.</b> 片名` / `🧲 体积 · 🟢 做种 · 🔴 下载 · 📅 日期`），
  行间空一行，Telegram 那边不用等宽字体也不会糊成一团；
- 超长截断：拼到 3400 字符就停（Telegram 上限 4096，留出头部和提示的余量），
  末尾追加「…还有 N 条没列出」；
- `parsePickNumber`（1–2 位纯数字）→ `selectByNumber` → `takeLast`。取走即失效，
  `pickTTL = 30min`；
- **清单没有按钮，序号只能靠「最近一次搜索」定位**，所以 `Bot` 加了
  `last map[int64]*pick`（按 chat 存）。先搜「沙丘 2」再搜「沙丘 3」，
  回 `1` 拿到的是沙丘 3 的第一条 —— `TestNumberReplyUsesLatestSearch` 钉住这个行为；
- 越界或过期的数字**退回当片名搜**，不报错。用户回「20」本来也可能是在搜片名；
- `allowed_updates` 收窄成 `["message"]`。老消息里的按钮点下去只剩一句
  「搜索已改成清单，重新发片名再搜一次」—— 回调查询还留着，不然 Telegram 会
  一直重投那批更新。

`page` 字段、`turnPage`、`expirePick`、`pick.view()/pages()` 全部删掉；
`TestPickLabelCarriesTitle`、`TestPageViewAndKeyboard` 一并删掉，换成
`TestNameSearchesThenLists`（断言没有 `reply_markup`、清单里第 1 条和第 9 条都在）、
`TestSearchListsAllAndNumberReply`（回 `9` 命中第 9 条）。

---

### 5.35 移动端：底部 tab + 全站断点收敛

原来只有「侧栏变抽屉」，内容区没动过：表格横向溢出、按钮小到点不准、
`<input>` 是 13px（iOS 聚焦时会自动放大整页，还不还原）。

- `MobileTabBar.vue`：`≤900px` 固定底部，五个 tab（总览 / 影视库 / 整理 / 任务 / 更多）。
  「更多」不另做一套移动端菜单，直接把侧栏抽屉拉开 —— 两套菜单迟早不同步。
  `padding-bottom: calc(6px + env(safe-area-inset-bottom))` 避开 iPhone 底部横条；
- `AppShell` 的 `.content` 在 `≤900px` 时加 `padding-bottom: calc(74px + safe-area)`，
  否则最后一屏内容永远被 tab 盖住；
- `ui.css` 末尾一段全局 `@media (max-width: 900px)`：panel 留白收紧、`.table-wrap`
  惯性滚动、`table.table { min-width: 560px }`（宁可横滑也别把六列压成六十像素）、
  `.btn { min-height: 34px }`、`.input / .select-native { font-size: 16px }`（防 iOS 缩放）。

验证方式：逐页在 500px 视口下量 `document.documentElement.scrollWidth` 与
`innerWidth`，十一个页面全部 `doc == iw`，没有横向溢出。

---

### 5.36 播不了：我们自己 302 得很开心，客户端拿到的却是 403 invalid signature

用户报「远程挂载 Emby 无法播放，网页也无法播放」。两个入口都播不了，
说明不是客户端的问题，是链路中间某一跳。

**定位过程（值得记，因为一开始全看错方向）**

1. `.strm` 里是 `http://192.168.1.17:6086/d/<pickcode>.mp4`，肉眼以为是
   「私网地址，远程当然访问不了」。**这个判断是错的**：Emby 处理 http 源的 .strm
   时是自己去抓，再转给客户端（日志里 `VideoService ... GET /emby/videos/195/original.mp4`
   紧跟着 `HttpClient: GET http://192.168.1.17:6086/...`），所以私网地址对
   Emby 完全没问题 —— 只要 **Emby 服务端** 能到。
2. 直接 curl 代理 → `302`，看起来一切正常。
3. curl 跟随跳转 → 部分成功部分 `403`，而 403 的响应体是
   `{"status":403,"message":"invalid signature"}` —— **是 115 CDN 拒的，不是我们**。
4. 对照实验：同一秒内，用 A 请求签名、A 去下载 → `206`；换成 B 去下载 → `403`。
   结论：**115 的直链签名绑 User-Agent**（代码注释里一直写着这句话，
   但没人把它和「空 UA」联系起来）。
5. Emby 日志里只有 `Http response 403 from http://192.168.1.17:6086/...`，
   看起来像我们的代理在拒。给播放代理加上访问日志后才看到真相：

   ```
   msg=播放请求 pickcode=bf4l2rr2j5vfa8s5l ua=Mozilla/5.0 ip=172.18.0.1 cache=false status=302
   ```

   `ua=Mozilla/5.0` 是**我们自己编的**。Emby 的 HttpClient 抓 .strm 时
   **压根不发 UA**，而 `handleDirect` 里有一段「UA 为空就兜底成 Mozilla/5.0」，
   于是我们用 Chrome 的 UA 签了张票，Emby 拿着空 UA 去 CDN 兑换 → invalid signature。

**改了什么**

- `play.handleDirect`：删掉空 UA 兜底，原样透传（空的也透传）。
- `driver115.DownloadURL`：删掉 `if userAgent == "" { userAgent = ua }`。
  这段兜底和 SDK 的能力正好相反 —— `115driver` 的
  `DownloadWithUA(pickCode, "")` 会走 `applyEmptyUAHandling`，
  **真的一个 UA 头都不发**（upstream issue #80，专门为这个场景做的），
  我们却在上一层把空值换成了 Chrome UA，等于把 SDK 的解法废掉。
- 顺带给播放代理补上访问日志（`pickcode / ua / ip / cache / status / ms`）。
  这一页原先一行日志都没有，播不出来时只能去翻 Emby 的日志，而 Emby 只记一个 403
   —— 这是这次排查绕远路的主要原因。空 UA 在日志里显示成 `<无>`，
  别让它看起来像漏了字段。

**三条链路都验过（真实字节，不只是状态码）**

| 客户端 | UA | 结果 |
|---|---|---|
| 浏览器直连 | Chrome 131 | `206` + `video/mp4` |
| Emby 服务端抓流 | 空 | `206` + `video/mp4` |
| Emby 的 ffmpeg 探测 | `Lavf/59.27.100` | `302`（各自独立签名、独立缓存） |

**为什么不做「服务端中转」兜底**：115 的 CDN 是客户端直连的，中转会把
NAS 上行带宽吃满（§包注释里的铁律）。而这次的根因是**签名错**，不是
「302 跑不通」，所以正确的修法是签对票，不是改走中转。

**踩坑提醒**：`ua` 常量和 `driver.UA(ua)` 仍然要保留 —— 那是**登录 / 列目录**用的
客户端身份，和取直链时透传的 UA 是两件事，别一起删了。

---

### 5.37 一次报四件事：海报墙慢、取消不掉订阅、想知道今天调了几次 115

用户一口气报了四件，其中三件有共用的底色：**界面在骗人**（慢得像在联网、
按钮点了没反应、数字不在该在的地方）。

#### ① `/library` 首屏 2-3 秒：不是读本地慢，是每张卡都在现查 TMDB

看到「本地文件为什么慢」时，第一反应容易去看 sqlite。实际路径是：
`GET /api/catalog?source=tmdb` → 先读本地台账出卡片骨架（毫秒级）→ 然后
**无条件**对整页卡片调 `enrichCatalogTMDB` 现查 TMDB 详情。

测一下就知道瓶颈在哪：

| 测什么 | 结果 |
|---|---|
| 用户的反代 `https://tmdb.example.com/3` 单次剧详情 | 1.6 – 2.6 s（3 次） |
| `catalogTMDBConcurrency` | 4 |
| 六张卡 | 两轮 ≈ 2-3 s |

而这些字段**刮削时早就落进 `media_meta` 了**（`meta_title / rating / overview /
poster_url / tmdb_id`），现查等于把已知的东西再问一遍。

**改法**：`enrichCatalogTMDB(ctx, cards, force)` —— 有图（`Poster != ""`）的卡片
直接 `continue`，只有「连一张图都没有」的才联网；`?refresh=1` 是唯一强制重查入口。
海报做成三级兜底，前两级全不联网：

1. 磁盘海报 `/api/media/{id}/image`（`poster_file` 存在）
2. 刮削时存下的 `poster_url`，让浏览器自己去拉
3. 都没有才问 TMDB

**剧集海报只落在剧根那一行**（分集行 `PosterFile` 为空），所以 `catalogFromLocal`
不能只看「这一行有没有图」，要 `catalogImageOwner(items)` 在整组里找「谁存了图」，
否则整部剧都被判成无图、照样联网。`applyCatalogTMDB` 也改成**只补空位**，
不再用远端 URL 覆盖磁盘海报。

同一份库、同一个接口的前后对比：

```
普通请求     0.0015s 0.0013s 0.0012s     ← 改完
refresh=1    2.519s                      ← 改之前每次都是这个量级
```

回归测试 `TestCatalogLocalPosterSkipsTMDB` 用一个假上游只数请求次数，
钉住「三张卡都有图时 hits == 0」「磁盘海报走 /api/media/」「有 poster_url 就用它」
「refresh=1 才发请求」。写完先把跳过逻辑删掉跑一遍，确认它会红（报「实际问了 3 次」），
再放回去 —— 否则这个测试只是在描述自己。

#### ② 「点订阅无法取消订阅」：三个入口三种毛病，根子是没人记「订过没有」

| 入口 | 改之前 | 现象 |
|---|---|---|
| 订阅页签的垃圾桶 | 正常 `DELETE` | 唯一好用的路 |
| 海报墙详情按钮 | **永远只发 POST** | 订过的再点吃 409「已经在订阅里了」 |
| 日历抽屉按钮 | `sel.subscribed` 时 **`disabled`** | 点了完全没反应 ← 用户报的就是这个 |

而 `sel.subscribed` 这个字段**后端根本不发**：`/api/calendar` 只回播出信息，
没有 `subscribed`。所以：

- 日历抽屉的按钮**永远**是「已订阅」+ disabled（`sel.subscribed` 为假时又会
  变成可点的「订阅」，点下去 POST，看起来「点了没用」）；
- 日历的「我的订阅」筛选（`matches('mine')` 只看 `e.subscribed`）**永远是空的**；
- 卡片上的「★ 已订阅」角标从来不会出现。

**改法**：新增 `web/src/stores/subscriptions.ts`，把「订过没有」变成一份共享索引。

- 键是 `tv:1396` / `movie:693134` —— **必须带类型**，电影和剧的 `tmdb_id` 会撞号。
- `loadSubsIndex()` 一次列表请求建表（`inflight` 去重，`loaded` 挡重复），
  墙上几十个按钮不再各查一次。
- `subIdOf / rememberSub / forgetSub`，索引本体是 `ref`，
  所以日历的筛选和角标会随索引加载**自动重算**（普通对象就不会，计算属性只认响应式依赖）。
- 一个按钮两态：有 id 就 `DELETE` + `forgetSub`，文案「已订阅 · 点此取消」（红色）；
  没有就 `POST` + `rememberSub`；409 视为「别处订过」，`loadSubsIndex(true)` 对齐。
- 日历按钮**不再因为已订阅而 disabled**。
- 订阅页签删除成功后也 `forgetSub` —— 三个入口共用一份索引，别处删了这边要跟着变。

**真浏览器验证**（puppeteer-core + Chrome headless，`page.setCookie({name:'sid'})` 直接登入，
监听 `request / response` 看它到底发了什么方法）：

```
日历：BUTTON BEFORE: 已订阅 · 点此取消
      RESP 200 DELETE /api/subscriptions/3
      BUTTON AFTER: 订阅追更
墙  ：BUTTON: 订阅 → RESP 200 POST /api/subscriptions → BUTTON: 已订阅 · 点此取消
      → 再点 → RESP 200 DELETE /api/subscriptions/7 → BUTTON: 订阅
```

**踩坑提醒（两次都栽在这）**

1. 第一次「点取消没删掉」其实是**订阅在点之前就已经被上一次脚本删掉**，
   索引里没有 id，于是走了「新建」分支，POST 成功、按钮立刻变「已订阅 · 点此取消」——
   看日志会以为「取消订阅发出了 POST」。验证这类两态按钮，**必须先把前置状态造出来**
   （脚本里先查一次列表，或直接插一行），否则你测的是另一条分支。
2. 页面上的 `/library` 是台账（LibraryPage），海报墙（WallBrowser）在 `/media-transfer`；
   两边卡片都有 `.poster` 类，点错页面会打开「编辑 / 删除」那套台账抽屉，白找一轮按钮。

#### ③ 账户信息加「今日 115 调用」

115 的风控额度**按天算**，「本次运行以来调了多少次」没有意义（重启就归零）。

- `pan.Stats` 加 `Today atomic.Int64` + `todayDay atomic.Value`，`enter()` 里
  `bumpToday()`；**日期用本地时区** `now().Format("2006-01-02")` —— 用 UTC 会让
  晚 8 点之后的调用记到明天，用户会觉得「晚上调的不算数」。
- 计数跨天从 1 重新数（不是清零后 +1 再落库，直接覆盖成 1）。
- 新增 `api_calls(day, account_id, n)`，`panpool.Start(ctx)` 每分钟 flush 一次
  （退出时补最后一次），写库用 `SetAPICalls` **UPSERT 覆盖**，不是累加。
- 重启后 `SeedToday(day, n)` 把当天的数接回来；只接**同一天**的，昨天的账不接，
  重复 seed 不覆盖已有计数。
- `Snapshot` 增加 `requests_today` / `day`；账号今天还没被调用时（内存里没有
  Snapshot），`handleAccountStats` 从 `APICallsOn(ctx, today)` 兜底，否则重启后
  界面显示 0，用户以为计数坏了。

单测：`TestAPICallsRoundTrip`（覆盖而非累加、账号与天互不串、非法入参忽略）、
`TestPruneAPICalls`、`TestSnapshotCountsToday`（11-08 23:30 两次 → today=2；
跨到 11-09 → 0；再调一次 → today=1 / total=3）、`TestSeedToday`。
第二个用例专门钉「时区」这件事：如果实现里用了 UTC，23:30 那两次会算到 11-09，测试立刻红。

#### ④ 顺手补的观测

- **订阅轮询**：以前「轮询停了」和「轮询正常但这轮没到期」在日志里长得一样
  （都是什么都没有）。现在这一轮真有到期条目才记一行
  `订阅：这一轮有到期的`（带 `count` / `ids`），`CheckOne` 成功/失败各一条。
- **播放代理访问日志**：命中缓存降到 `Debug` —— 一次播放几十个 Range 探测
  会把真正的故障淹掉。

---

### 5.38 「奥德赛字幕错误」：字幕挑到了同名另一部片；顺带删掉旧页面

用户报「奥德赛字幕错误」，同时要求两件事：**字幕优先下载中文、实在找不到才 AI 翻译**，
以及**删除旧页面**。

#### 现场：库里显示一切正常，字幕内容却是错的

台账里 `sub_state=ok`、`sub_lang=zh`、字幕文件 96KB 都在 —— 从状态看完全没问题。
打开文件才是问题：

```
1
00:00:26,728 --> 00:00:30,264
<i>旁白：</i>
<i>现在听听特洛伊之歌，</i>
```

读起来像机翻（「落地啦！」「墙高于任何攻城塔」「一座应该的城市从未跌倒」），
但**不是我们翻的**：拿字幕每行文本去比 `translation_cache`，2016 行**零命中**；
而且如果是我们翻的，缓存里会多出两千行（当时整张表只有 469 行）。
所以这是**下载来的中文**，只是上传者自己机翻过。

#### 真因：挑中了 The Asylum 的同名山寨片

这个 IMDb（`tt33764258`）下挂了 6 条中文，其中有：

| 候选 | 片源名 | 相似度 | 字幕覆盖时长 |
|---|---|---|---|
| A | `The.Asylums.The.Odyssey.2026.1080p.AMZN.WEB-DL…` | **36** | **82.7 分钟** |
| B | `The.Odyssey.2026.1080p.CAM.x264-OnlyFlix.zh-CN` | 30 | 164.2 分钟 |
| C | `Odyssey_简体中文版` | 8 | 165.2 分钟 |

奥德赛是 173 分钟。A 是 The Asylum（专拍山寨片的那家）2026 年的另一部《The Odyssey》，
字幕站把它挂在了同一个 IMDb 下。**它的相似度反而最高**：`similarity()` 用字符 bigram
重合度，`The.Asylums.The.Odyssey.2026` 比 `The.Odyssey.2026` 多出的 bigram 恰好和
视频名里的 `the.odyssey.2026` 大量重合，分母又只按最长的那串算。

于是旧代码 `Pick()` 取中文第一条 → A → 一条 83 分钟的字幕配 173 分钟的电影，
播放器里就是满屏对不上的台词。

#### 改法：多候选 + 时长校验，中文仍是第一优先

1. `source.Rank(list, videoName)` 取代「只挑一条」：中文在前、英文在后，各自按相似度
   降序，并把分数带出来。`Pick` 保留（内部走 Rank），只给「只要一条」的调用方用。
2. `fetch.tryCandidates()` 按顺序最多试 3 条（`MaxTry`）：下载 → 解析 → **时长校验**，
   第一份过关的才用。被放弃的连同原因记进 `Result.Tried`，直接显示给用户看。
3. `fitsRuntime(file, runtimeMin)`：字幕最后一条对白的结束时间要落在片长的
   `0.75 ~ 1.15` 倍之间。0.75 是留给「片尾几分钟没人说话」的（173 分钟的片子，
   最后一条对白常在 164 分钟）；1.15 挡住整季/加长版字幕。**片长未知（0）一律放行** ——
   宁可错用，也不要因为 TMDB 没给 runtime 就一集都补不上。
4. 顺序仍是「中文 → 英文 + 翻译」：中文候选全部被判「不是这部片」时才走翻译；
   没开翻译就返回 `ErrMismatch`（有字幕，但对不上），不再是「成功后落一份错字幕」。
5. `runtimeMinutes()` 解析台账里的 `173 min` / `2h 53m` / `173`。
   **别用 `fmt.Sscanf(s, "%dh %dm", &h, &m)`**：它在 `"173 min"` 上会部分匹配成功、
   先把 173 写进小时位，于是 173 分钟变成 10380 分钟，校验永远不过。
6. 台账新增 `sub_origin`（`zh` 下载 / `translated` 机翻）。以前只有 `sub_lang=zh`，
   机翻和官方中文在界面上长得一模一样 —— 用户只能「听出来不对」，查不到是哪一步给的。
   界面徽标现在是「有中字 / 机翻中字 / 无中字 / 字幕不符 / 字幕失败」。

#### 验证

- 单测：`TestRankChineseFirstThenScore`、`TestFitsRuntime`（8 组边界）、
  `TestFetchSkipsWrongFilm`（山寨片排第一 → 跳过 → 选 CAM 版）、
  `TestFetchMismatchNoTranslate`、`TestFetchMismatchThenTranslate`、
  `TestRuntimeMinutes`。反向验过：把 `fitsRuntime` 改成恒真，
  前三条立刻红（会把 83 分钟的字幕当成功），说明测试不是自说自话。
- 联网用例 `TestLiveOdysseyPicksRightFilm`（`SUBTITLE_LIVE=1` 才跑）实测：
  ```
  选中: The.Odyssey.2026.1080p.CAM.x264-OnlyFlix.zh-CN（相似度 30，1699 条，来源 zh）
  跳过: The.Asylums.The.Odyssey.2026.1080p.AMZN.WEB-DL…（只有 83 分钟，片长 173 分钟）
  ```
- 线上这条用一次性程序（`_tmpfix`，用完即删）按新逻辑重下并写了台账，
  字幕文件从 96KB/山寨片换成 113KB/1699 条，首行是片头卡 `《奥德赛》`。

> **踩坑**：媒体库根是 `media/movies`（`libraries.local_path=/media/movies`），
> 不是 `media`。第一次修数据时把根传成了 `MEDIA_ROOT`，于是在
> `media/The Odyssey (2026)/` 下写了一整套新目录，真正的
> `media/movies/...` 一个字没动 —— 而且程序照样打印「完成: saved=…」。
> 要落盘的路径一律走 `scrape.ResolveRoot(lib.LocalPath, cfg.MediaRoot)`，
> 别凭 `MEDIA_ROOT` 猜。

#### 顺带：旧页面删除

`internal/httpapi/index.html`（1,700 行单文件界面）、`GET /legacy` 两条路由、
`WEB_DEV_FILE` 环境变量、`handleLegacy`、`go:embed index.html` 全部删掉。

删的原因不是「不好看」：它和 Vue 版**共用同一批接口**，每个改动都要改两遍；
更麻烦的是排查时容易看错页面（海报墙在 `/media-transfer`，台账在 `/library`，
两边卡片还都有 `.poster` 类）。现在 `webdist` 缺失时 `GET /` 回 **503 说明页**
并打印 `scripts/web-build.sh`，不再静默退回老界面。开发期改前端走
`scripts/web-build.sh --dev`（vite HMR）。

#### 部署坑：镜像里的前端可能是旧的

`docker compose build` 之后一定要确认镜像里的前端是新构建的 —— 有一次 Go 代码已经是新的
（二进制里能 grep 到新加的日志文案），但页面还是旧版：web 阶段的层被复用了，
`COPY --from=web` 又把**旧的** `webdist` 盖回了 `COPY . .` 拷进去的新产物上。

核对方法（不要看 `grep 二进制`，UTF-8 资源在 busybox grep 下不可靠）：

```bash
E=$(curl -s http://127.0.0.1:6060/ | grep -o 'index-[A-Za-z0-9_-]*\.js' | head -1)
curl -s "http://127.0.0.1:6060/assets/$E" | grep -o 'LibraryPage-[A-Za-z0-9_-]*\.js' | head -1
# 用上一步拿到的 chunk 名再取一次，确认里面的新文案
curl -s "http://127.0.0.1:6060/assets/<chunk>" | grep -c '机翻中字'
```

拿到的 chunk 名取不到（返回 `text/html` 且 1.7KB）就说明是 SPA 兜底 = 该 chunk 不在镜像里。
这时 `docker compose build --no-cache`。


---

### 5.39 三处「界面看着像一回事、其实各有各的数据」

用户一句话报了三件：「发现与订阅中订阅和播出日历中的订阅没有联动」「所有页面点击影视名称显示影视详情」
「订阅水位线在订阅设置里显示设置」。表面是三个功能，根子是同一个：**同一个概念在前端有好几份互不相认的副本**。

**① 联动：索引有了，列表还是进页面时那一份。**

上一轮已经建了 `web/src/stores/subscriptions.ts`（`tv:1396 → 订阅 id`），当时解决的是「按钮不知道自己订过」。
但订阅页签的表格是 `onMounted` 拉一次的 `subs.value`：在日历里订完再切回订阅页，页面没重挂载，
表格还是旧的那份 —— 用户看到的就是「两个页面的订阅对不上」。日历的「我的订阅」筛选更早还有个坑：
`/api/calendar` 的条目里根本没有 `subscribed` 字段，筛选恒空。

- `subsVersion`：索引里每次真实增删都 +1（**值没变不 bump**，否则光是打开详情抽屉就会让订阅页签白拉一次列表）。
- 订阅页签 `watch(subsVersion)` → 重拉表格；`watch(tab)` 切到「订阅」→ 也重拉。
  本页自己删的那一次用 `skipSelfBump` 吃掉，不然同一个动作拉两遍。
- 日历页 `onMounted` 用 `loadSubsIndex(true)` **强制**刷新（路由切回来就是新组件），
  筛选 / 角标 / 徽章一律走 `isSubscribed()`，不再碰那个不存在的 `e.subscribed`。
- 详情抽屉的 `load()` 拿接口返回的 `sub` 对齐本地索引 —— 别的设备上订的也能对上。

**② 片名点不开：详情抽屉写了三份，订阅表一份都没有。**

`/api/catalog/item` 只认 `emby:` / `local:` key，因为它服务的是「墙上已经有的片」。
订阅列表、播出日历手里只有 `tmdb_id`（多数还没入库），整理记录干脆只有片名 —— 这些片名只能是死文本。

新增 `GET /api/title?type=&tmdb_id=&media_id=&q=&year=`，一次把三处数据拼齐：

| 来源 | 给什么 | 没有时 |
| --- | --- | --- |
| TMDB | 标题 / 原名 / 评分 / 简介 / 海报 / 季清单 | 有台账行就用台账字段，附 `note` 说明 |
| 本地台账 | 库名 / 路径 / 已有集数 / 字幕状态与来源 | `local.exists=false`，不假装入库 |
| 订阅表 | id / 状态 / 画质 / 水位线 / 最近一轮 | 抽屉给「订阅」按钮 |

`q` + `year` 是「只有片名」的兜底：先按名字问 TMDB 换 id，**之后走和直接给 id 完全相同的一条路**，
不为首页最近入库或整理记录单开一套逻辑。优先级 `media_id > tmdb_id > q`；什么标识都没给回 400，
绝不「猜一部片出来」返回 200。

前端：`stores/detail.ts`（一个全局 `detailTarget`）+ `components/MediaDetailDrawer.vue`（挂在 `App.vue`，
全站唯一一份）+ `components/TitleLink.vue`。`TitleLink` 用 `span[role=link]` 而不是 `<a>`：
首页最近入库的卡片包在 `RouterLink` 里、海报墙卡片是 `<button>`，a 套 a / button 套交互元素都会坏结构；
点标题默认 `stopPropagation`，不会连带打开卡片自己的操作抽屉。
已接入：订阅表格、追更进度、播出日历卡片与抽屉标题、TMDB 榜单、我的海报墙、首页最近入库、任务整理记录、自动整理（预览 / 台账 / 试算）。

> 两个抽屉不冲突：WallBrowser / LibraryWall / 日历各自的抽屉是**操作面板**（搜资源、改元数据、字幕），
> 点海报开；点**片名**开的是统一影视详情。

**③ 水位线：以前只能动库，现在在订阅设置里。**

`PUT /api/subscriptions/{id}` 新增 `watermark_season` + `watermark_episode`（必须成对、非负）。
语义是**强制设置**而非自动推进：新增 `ForceSubscriptionWatermark`，不带 `SetSubscriptionWatermark` 的
单调条件（那个是防并发乱序把水位线拉回去的），并把 `next_check_at` 一起推到当下，下一轮轮询立刻按新值跑。
往回拨是必须的 —— S01E03 搜不到资源会把水位线永久卡死，用户得能跳到 S02E08。

界面在「发现与订阅 → 订阅设置 → 追更进度」，逐条列出剧集订阅，改动往回拨时二次确认
「前面的集会重新提交」。`TestSubscriptionWatermarkPUT` 钉住「可后退」这条，反向验过：
把强制设置换成带单调条件的 SQL，测试立刻报 `后退没生效（被单调条件吃掉了？）: S2E8`。

顺带：`TestTitleDetailJoinsLocalAndSubscription` 把「TMDB 展示字段 + 本地台账 + 订阅水位线」
三份数据在一个响应里一起断言，避免以后某一块被悄悄摘掉。

**④ 顺手修：日历会把「空白的一周」缓存 30 分钟。**

真浏览器验证时发现日历 110 条片子一条都不显示，`/api/calendar` 回 `total=0 / cached=true`、
`sources.shows=48`、`notice: TVMaze 增强未完成：context canceled`。原因是上一轮测试脚本
关页面把请求 `ctx` 取消了：`build` 里各增强源都按「失败只记原因、不把日历弄没」处理，
并发任务跳过后剧单有了、条目却是空的，于是不报错地返回一份空周历；`Week()` 无条件把它
写进缓存。而 `saveCache` **先写内存再写库**、`cached()` 又优先读内存 —— 就算 DB 那次写入
同样因 ctx 取消失败，内存里仍然留着空白，接下来 30 分钟所有人看到的都是空日历，
「我的订阅」筛选自然也恒空。表现和「订阅没联动」一模一样，排查时很容易被骗。

改成只有 `w.Total > 0 && ctx.Err() == nil` 才写缓存；宁可下次重抓 12 秒，也不能让一份
残结果占住整周。`TestEmptyWeekIsNotCached` 反向验过：撤掉守卫会报 `空白周历被缓存了`。

### 5.40 字幕重下 Emby 不更新 / 详情加目录跳转 / 订阅加限制线

用户三句：「为什么字幕重新下载 emby 没有更新」「影视详情里加跳转网盘目录和本地目录的链接」
「加一条限制线，线之前的剧集不下载，跟水位线放一起」。

**① 字幕重下为什么没更新（两个原因叠在一起，都不是玄学）**

- 台账查到奥德赛那行 `sub_state=ok, sub_origin=existing` —— 这就是铁证：
  用户点「补中文字幕」时 `overwrite=false`，`fetch.go` 的 `findExistingChinese`
  命中已有 `.zh.srt` 就 `return res, nil`，**根本没下载、没写盘**。
  「重做全部字幕」更隐蔽：`handleLibrarySubtitles` 的 `redo` 只放宽
  `ListMediaForSubtitle` 的筛选，`runSubtitleJob` 里 `Overwrite` 却写死 `false`，
  于是每条都跳过、把旧路径重新写回台账，进度条「新增 N」纯属空转。
- 就算真的覆盖了，字幕链路**一行 Emby 刷新都没有**：`internal/httpapi/subtitle.go`
  全文不 import `mediaserver`；刷新只存在于 sync / scrape / catalog 三处。
  Emby 首次扫描把外挂字幕登记进自己的 MediaStreams（`emby-config/data/library.db`），
  之后覆盖 `.srt` 不会自己重探；而且条目刷新看的是**主媒体文件 `.strm` 的 mtime**，
  只改旁边的字幕很可能整段跳过 ffprobe。刮削链路的注释（§5.10 / `emby.go:180`）
  早就把这件事写明白了，字幕侧漏了这一层。

改法：`redo → Overwrite` 一路传下去；前端在海报墙抽屉 / 刮削表格加显式的
「重新下载」「重下」按钮传 `overwrite=true`（普通「补中文字幕」保持跳过语义）。
`finalizeSubtitle` 只在覆盖成功后做三件事：把其它 `.chs/.cht/...` 中文字幕改名
`.old`（不删除）、`os.Chtimes` bump `.strm` 的 mtime（**只改时间戳，不动直链内容**）、
再由 `refreshSubtitleServer` 发 `POST /Library/Media/Updated`
（`{"Updates":[{"Path":...,"UpdateType":"Modified"}]}`，Emby 官方给外部程序用的
文件变更通知，见 `emby.media` 社区帖）＋ `RefreshPaths(FullRefresh)`。
整库任务收尾统一刷一次（逐条刷会把 Emby 打疼），单条立刻刷；
开关 `subtitle_config.refresh_server` 默认开，老配置缺这个键时靠
`json.Unmarshal` 到默认值保住 `true`，**别写成 `cfg := subtitleConfig{}`**。
顺手让 `media_meta.updated_at` 真的跟着字幕变。

**② 详情抽屉的目录跳转**

`store.Media` 没有 `LocalPath/RemotePath/RemoteCID` —— 那三个在 `store.Library` 上，
通过 `media.library_id` 关联。`/api/title` 以前只取了 `lib.Name`，其余全丢。
现在补 `remote_cid / remote_path / local_root / local_dir / host_dir / local_rel_dir
/ remote_dir_cid / remote_dir_exact`。

`remote_dir_cid` 走 `GetFileByRelPath → pool.Get → mgr.Stat(file_id) → ParentID`
再沿 `ParentID` 上溯到「父目录 == 库根」那层（剧集因此会跳过 `Season 1` 落到剧根）。
**绝不能拿 `remote_path + rel_path` 拼**：§5.12 已经踩过同名层级的坑。
2.5 秒超时 + 失败返回空，前端退回 `115.com/?cid=<库根>`；
解析成功就跳到这部片自己的目录（真机验到 `3557806847253546598`，与库根不同）。

浏览器从 http 跳 `file://` 会被拦，Docker 里更没有宿主机桌面，所以给了四个入口：
115 网页版 `<a>`、`file://` `<a>`（标注可能被拦）、复制路径、
`POST /api/local/open`。后者只收**相对库根**的路径、走 `resolveLocal` 挡穿越、
在 Windows 本机跑时调 `explorer.exe`，检测到 `/.dockerenv` 就回
`opened:false` + 换算好的宿主机路径，前端自动塞进剪贴板。

**容器路径 ≠ 宿主机路径**：容器里 `local_dir=/media/movies/...`，在 Windows 宿主上
毫无意义。新增 `config.MediaHostRoot`（来自 `.env` 的 `MEDIA_HOST_PATH`，
compose 原样传进容器），`hostLocalPath()` 按「相对 `MEDIA_ROOT`」把
`/media/movies/X` 映射成 `D:/media/strm115/movies/X`。
相对值 `./media` 换算不出来就返回空，界面只显示容器内路径 ——
宁可少给一个字段，也不给一个看着能点其实不存在的地址。本机跑 exe 时
`libRoot` 本来就是 Windows 绝对路径，`host_dir` 留空即可。

**③ 限制线（独立下界，不合并进水位线）**

选独立字段而不是「把水位线当起点」：水位线会被 `SetSubscriptionWatermark` 自动推进、
用户还能往回拨，一旦拨回 `S01E01` 老集就会重新下载，界面上再也看不出最初的下限。
新增 `limit_season / limit_episode`（`INTEGER NOT NULL DEFAULT 0`，`0/0` = 不限），
建表 + `ensureColumn` 两条路径都要补；`subCols` / `scanSub` / INSERT 三处列顺序必须同步
（**踩了**：第一次少一个 `?`，`22 values for 23 columns` 直接让 4 个包的测试全红）。

过滤只有一处：`pendingEpisodes(wmS, wmE, limitS, limitE, show, max)` 里
`atOrBefore(s, e, limitS, limitE) → continue`，即取两条线里更靠后的那条。
`0/0` 时该判断恒假，老订阅行为一字不变；`checkMovie` 天然免疫（电影分流不进这函数）。
`SetSubscriptionLimit` 可前进可后退、不碰水位线、把 `next_check_at` 推到当下。
`PUT /api/subscriptions/{id}` 接受成对 `limit_*`，与 `watermark_*` 一次提交；
顺手修了「改不存在的 id 也回 `{ok:true}`」——`UPDATE` 影响 0 行不报错，
先在 `handleUpdateSub` 查一次，没有就 404。

前端在「订阅设置 → 追更进度」把两行输入（已交到 / 限制线）和一次保存放一起，
详情抽屉的订阅块多一行「限制线」。**两个方向都要二次确认**：
往后调 = 永久跳过更多集；往回退 = 之前被挡住的老集会开始补，花 115 配额。

**真机验证**：`docker compose build --no-cache`（Go 新前端旧的老坑）→ 容器 healthy。
`POST /api/media/980/subtitle {"overwrite":true}` 回 `overwritten:true,
server_refresh:true, cues:1699, origin:zh`，日志
`已让媒体服务器重读字幕 文件=2 目录=1 条目=1`，`.strm` mtime 变了、
`.zh.srt` 内容与备份一致（仍是正片 CAM 中文，山寨片 83 分钟那条被时长校验挡掉），
台账 `sub_origin` 从 `existing` 变回 `zh`。`/api/title?media_id=980` 带出全部目录字段。
Puppeteer 验：订阅设置两行输入 `[0,0,99,999]`、详情抽屉四个目录按钮、115 链接指向片目录 cid、
无页面异常。测试新增 7 组（迁移补列幂等、限制线读写/优先级、PUT 校验、MediaUpdated 请求体、
finalizeSubtitle、配置默认值等），`go test ./...` 全绿、`vue-tsc` 0 错。


---

### 5.41 发现页缓存 / 站内通知 / TMDB 候选搜索 / 国内平台 / 日历水位线 / TG 体积 / 详情删除

用户一口气提了七项：发现与订阅「默认读缓存、点刷新才爬」、右上角通知上线、
资源搜索「先出 TMDB 信息、选中再搜并同步 TG」、发现页加国内平台、
水位线进播出日历详情、TG 结果显示体积、资源详情页的删除入口「在哪儿看」。

**① 榜单缓存（wall.go 重写）**
`/api/wall` 的入参是 `kind(movie/tv) + genre + providers + sort`，缓存键必须把
`page` 之外的所有维度都算进去（`wallCacheKey`），否则用户切平台会拿到上一平台的
结果。缓存落 `settings` 表（`wall_cache_<hash>`），TTL 6 小时；
**只有 `refresh=1` 才穿透**，`handleWall` 默认先读缓存，命中直接回 `cached:true`
并带 `cached_at`，前端工具条显示「缓存 · 上次更新 xx」。缓存写入失败只告警不阻断。
注意：这是「用户主动刷新才现查」，不是 TTL 到期自动更新——用户要的是「不要进页就爬」。
`wall_test.go` 覆盖键隔离、缓存命中、refresh 穿透、空结果不落库。

**② 国内平台：真实 provider + 国产内容兜底**
TMDB 的 `watch/providers/movie?watch_region=CN` 返回空列表，**中国区没有流媒片单**。
全球 provider 表里能查到腾讯视频(623)、爱奇艺(581)、WeTV(509)，且用 `watch_region=US`
能查出真实片目；优酷/芒果TV/B站/咪咕在 TMDB 里没有 provider ID。
所以 `providerCodes` 分两组：`overseas` 正常走 `with_watch_providers`；
`cn` 组里 `TMDB>0` 的照样按 ID 查，`TMDB==0` 的走「国产内容 + 中文语言 + 按热度」兜底，
chip 上带 `note` 明写「TMDB 没有 XX 片单，按国产内容推荐」——
**宁可说清是兜底，也不假装是官方片单**。前端 `WallBrowser.vue` 把两组分组渲染。

**③ 站内通知中心（和出站推送分开）**
已有 `internal/notify` 是「怎么发」（8 个渠道），本项目外推扇出点只有
`httpapi/notify.go:sendMessage`。站内信是另一条旁路：`recordNotification`
在 `sendNotify/pushEvent/pushEventImage` 三个入口、`eventOn` 判断**之前**调用——
关掉外推只是不推送，站内仍留记录。`Force`（测试推送）和空标题不落库；
`notifications` 表保留最近 300 条，`read_at=''` 计未读。
`/api/notifications`、`/api/notifications/read`、`/api/notifications/clear`；
前端 `stores/notifications.ts` 30 秒轮询、`document.hidden` 时跳过、
**未登录不启动**（第一版抢跑会打 401），`AppTopbar.vue` 铃铛换成数字角标。
`handleTestNotify` 直接调 `s.notify.Test()` 不走 sendMessage，天然不会污染站内信箱。

**④ TMDB 候选 → 选中再搜（同步 TG）**
`GET /api/tmdb/search?q=&type=&year=` 走 `tmdb.SearchMeta`，按热度排序截断 12 条，
`overview` 按 rune 截 160。前端 `TitlePicker.vue` 是「找片」卡片列表，
选中后把 `hit.title` 当中文关键词、`hit.original` 当英文名，一次 `res.run({q, en})`
打 `/api/res/search`——**资源搜索是一轮把 TG + 种子站全搜回来**，
所以「同步 TG」不需要第二个接口，给准英文原名即可；来源/体积 chips 仍是本地过滤。
`useResSearch` 里「一次搜索只打一次后端」的约定别改。
「跳过，直接按关键词搜」保留旧行为；「重搜」与「搜索」（走候选）分开。

**⑤ 日历详情的水位线**
从共享订阅索引 `stores/subscriptions.ts` 取**整行**（`subRowOf`），
不给每张卡片多打一次 `/api/title`；订阅设置改完 `loadSubsIndex(true)`，
日历下次开抽屉就是新值。已订阅显示「水位线 · 已交到 SxxExx」+「限制线」，
并指向「发现与订阅 → 订阅设置 → 追更进度」。

**⑥ TG 体积 & 详情删除**
TG 频道帖里的 `[8.9GB]`、`大小: 2.1 GiB`、`📦 1.4G` 这类散文本以前没解析，
`applyHuntMeta` 现在统一交给 `sizeFromText` → 复用 `torrent.go` 已有的
`parseSizeText`（**全项目只有这一份尺寸解析，别再写第二份**），回填 `Item.Size`。
资源详情删除入口以前确实找不到：`/api/title` 只回目录字段，抽屉没法拼
`api.catalog.delete` 需要的 key。现在补 `catalog_key`，抽屉底部出「删除资源」
按钮 + `HModal` 二次确认，删完关抽屉并刷新列表。

**真机验证**（隔离实例 6071，用副本库清掉管理员凭据，不动生产账号）：
`/api/wall` 连续三次 `cached=true/false` 符合预期、`provider_options` 带 cn 组、
`tx/yku` 各自有结果；`/api/tmdb/search?q=沙丘` 返回候选；
`/api/notifications` 返回 `items:[] + unread`；
`/res/search?refresh=1` 的 TG 结果带 `size>0`；
Puppeteer 验日历 S01E05 / 限制线 S01E03、详情删除按钮与确认弹窗、通知铃铛。

---

### 5.42 代码精简 / 资源消耗 / 生产化准备

用户问：「功能能不能整合成 Python 那种订阅器、刮削器、通知器？顺便精简代码、
优化消耗，然后传私有仓库准备上生产」。

**① 先说结论：不用重新拆包。** 全量盘点 30 个 `internal/<能力>` 包，
依赖是单向的 `cmd → httpapi → service → store/pan/tmdb`，**零循环依赖**，
而且没有任何领域包 import httpapi（`subscribe/service.go` 的 `Deps` 甚至用
函数注入主动切断反向依赖）。用户点名的订阅器 / 刮削器 / 通知器 / 字幕器 /
资源搜索器五个包**都各只有一份实现**，比多数 Python 项目更接近理想划分。
真正的问题不是「没拆」，是两处：`httpapi` 塞了 2313 行本该属于能力包的编排
（字幕任务状态机 553 行、入库通知业务 329 行、扇出与事件订阅 445 行），
以及缺一个公共工具层（7 份 `firstNonEmpty`、4 份 `truncate`、3 份 `humanSize`，
前端连 `utils/` 目录都没有）。

**② 第一刀：只删生产零调用的东西**（约 300 行 Go + 20MB 构建上下文）。
`internal/notify/api.go` 整套 209 行 REST handler 只有自己的测试在用，
与 `httpapi` 的 `/api/notify*` 重复且没挂到主服务 —— 连测试一起删；
`sendNotify`、`SendAsync`、`cutHTML` 全仓库 grep 确认零调用；
`internal/web/` 是 legacy 空目录；`strm115.exe~` 是 20MB 备份，
`.dockerignore` 的 `*.exe` **不匹配结尾带 `~` 的文件**，会进 `COPY . .`
的构建上下文，补一行 `*.exe~` 并删文件。
`httpapi` 里 `subscribe.go:firstNonEmpty` 与 `catalog.go:firstNonEmptyLocal`
是**同一个包内两份一模一样的函数**，删一份改三处调用。
**没删** `Client.SendNews`：它连着 `NewsChannel` 接口和企微的图文实现，
只删 Client 方法会留下半套死接口，得不偿失。
`humanSize` 三版也没合并：`tgbot/render.go` 的 `n<=0 → ""` 是模板用来
决定「要不要显示体积」的**有意差异**，合并会改 UI 行为。

**③ 前端收敛 + 轮询降耗。** 新建 `web/src/utils/format.ts`，
把 3 份 `pad2`、3 份 `fmtSize`、5 份 `copy`、3 份日期格式化收敛进去
（`fmtDate` / `fmtClock` / `fmtDay` 各自保留语义，不硬抹平）。
`ScrapeConsole.vue` 原本两个 1500ms 定时器可能同时跑且都不看
`document.hidden`，合并成一个 2000ms ticker 在一轮里分别拉刮削/字幕状态，
后台标签页直接跳过 —— 轮询请求从约 80 次/分降到 30 次/分。
`TasksPage` / `LogsPage` 只补 hidden 判断，不动间隔。
注意 `useResSearch.ts` 的 `fmtSize` 是**对外导出的**，改成
`export { fmtSize } from '@/utils/format'` 保住导入方。

**④ 后端连接与缓存。** `tmdb.NewClient` 以前每次 `&http.Client{Timeout:15s}`，
电影墙/订阅/刮削/详情各自 new，等于每轮重新 TCP+TLS 握手；
改成包级 `sharedHTTP`（`http.Transport` 连接池，`ProxyFromEnvironment`
必须保留，否则会绕开环境代理）。**别顺手把 `Base` 也共享**：
上游反代地址是进程级设置，用户在设置页改完下一次 NewClient 就要生效。
`/api/title` 其实**早就有 30 分钟 TMDB 详情缓存**（走
`catalogTMDBMeta` → `catalog_tmdb.go` 的 TTL map），不需要再加一层；
盘点报告只看 `title.go` 文本里没有 cache 字样就判「无缓存」，是假阳性。

**⑤ 会话密钥是死代码，别再照着旧文档备份它。**
`config.loadSecret` 生成 `<DATA_DIR>/session.key` 并写注释「重启不掉线」，
但 `Config.Secret` 全项目**没有任何消费方**：真正的会话是
`server.go:sessionStore` 的纯内存 map。也就是说重启必然全员掉线，
旧 README「data/ 里有会话密钥、需要备份」是错的。
这次直接删掉 `Secret` / `loadSecret`，文档改成如实说明「重启需重新登录」。
要做持久化会话得改成签名 cookie（HMAC + 过期 + 改密码轮换密钥），
属于行为变更，没塞进这次「上线前精简」。

**⑥ 生产形态：加一层，不重写应用。**
新增 `docker-compose.prod.yml` + `deploy/Caddyfile` + `.env.production.example`
+ `scripts/backup.sh`。基础 compose 的 6060/6086/8096 全是 `0.0.0.0` 明文，
生产覆盖层把它们收敛到回环、由 Caddy 独占 80/443 并自动签证书。
**compose 的 `ports` 是按 `{ip,target,published,protocol}` 唯一键追加的**，
只写 `127.0.0.1:...` 会和基础文件的 `0.0.0.0:...` 两份并存、同端口冲突；
必须用 `!override`（`!reset` 只清空、带值会被忽略）。已用
`docker compose --env-file .env.production.example -f ... config -q` 验过语法。

应用侧补四件：`COOKIE_SECURE=1` 时会话 cookie 带 `Secure`（明文 HTTP 下
默认关，否则浏览器不回传、登录直接失效）；登录失败 5 次锁 5 分钟；
`crossSiteWrite` 只拦「带 Origin 且 Host 不匹配」的写请求 —— **不带 Origin
必须放行**，否则 curl / 脚本 / 服务端回调全被误伤；`X-Frame-Options`。
HSTS 交给 Caddy 加：裸跑是 HTTP，应用层加 HSTS 会把浏览器锁死在连不上的 https。

`strm115 backup <绝对路径>` 子命令走 `VACUUM INTO`：WAL 模式下
`cp strm115.db` 可能漏掉 `-wal` 里未 checkpoint 的事务，拿到半截库；
由 SQLite 自己写一致性视图就不用停服。目标文件已存在时拒绝覆盖
（backup.sh 同秒重跑会撞名）。`BackupTo` 失败会删掉半截文件。

**⑦ 明确没做（风险大于收益，留给下一轮）**
- 字幕任务状态机（553 行）下沉到 `internal/subtitle`：有并发锁、取消、
  Emby 刷新回调，上线前不动。
- 后台 6 组 goroutine 生命周期从 `httpapi.New()` 上移 `cmd/server`：
  主要收益是测试进程里 129 次 `Handler()` 不再各自起轮询，生产无感。
- `notify_ingest.go` 一轮入库通知读 3 次全表：要改就得先保住
  「必须 diff 台账，否则重刮老片会重推」的语义，风险中。
- 全面合并跨包重复小函数：净收益约 1%，要动 14 处定义，不值得在上生产前做。
- 播放代理 `/d/{pickcode}` 加签名 URL：会破坏库里所有已生成的 `.strm`，
  改用反代访问控制 / IP 白名单更稳，已写进 README 的 P0 清单。

---

## 5.43 播放地址随 DHCP 漂移导致整库无法播放

### 现象

「昨天还能放，今天 Emby 里点开全都没反应」。管理后台正常、播放代理也正常、
日志干净——因为请求根本没到服务：播放器在连一个已经不存在的地址。

### 根因

`.strm` 里写死的是 `play_domain`（例：`http://192.168.1.17:6086/d/xxx.mkv`）。
家用路由器默认 DHCP，宿主机网卡续租换了 IP（本例 `.17` → `.13`），
26 个 `.strm` 全部指向旧地址。Emby 侧 `MediaSources.Path` 同样是旧 URL，
所以它连 `ffprobe` 都过不去。

### 为什么之前没兜住

`internal/httpapi/netaddr.go` 的 `localIPv4()` 枚举的是**本进程所在网卡**。
生产跑在容器里，只能看到 `eth0 = 172.18.0.2`，而 172.16/12 被刻意过滤
（那是 docker 地址池，填进 `.strm` 只有同网络容器能访问）。
结果就是设置页的「候选地址」在生产环境**永远是空的**，用户当初只能手填一个 IP，
DHCP 一漂就整库播不了，而且没有任何提示。

### 修复

新增 `internal/httpapi/netfollow.go`：

- **判据改成探测**：直接 `GET {play_domain}/healthz`。能连上自己，播放器就
  大概率连得上；这在容器里也成立（实测容器探测宿主 IP 正常）。
- **候选来源改成「学到的 Host」**：`middleware` 里每个请求调
  `rememberHost(r.Host)`。用户能用这个地址打开后台，就说明它对本机可达。
  只保留最近 30 分钟、且不是回环/169.254/172.16-31 的 IP。
- **三重保险才动手**：
  1. 连续两次探测失败（隔 1 秒重试，避开 115 拉流占满带宽的抖动）；
  2. 当前地址必须是 IP（域名不通是 DNS/DDNS 问题，换内网 IP 只会更难查）；
  3. 候选必须与旧地址**同 /24**（DHCP 换号不换网段；跨网段说明网络环境变了）。
- 改完调用已有的 `rewritePlayDomain` + `SetSetting`，并落一条
  `play_domain_fixed` 站内通知，写清「改了哪些、为什么、建议 DHCP 保留」。
- 找不到候选时发 `play_domain_broken` 告警，同一地址只提醒一次（`playAlerted`），
  否则每 5 分钟刷一条。
- 节奏：启动后 45 秒首检（等容器网络就绪），之后每 5 分钟。

`handleNetAddress` 的 `candidates` 同步修好：把当前访问 Host 与
`seenCandidates()` 并入，容器部署下设置页终于有可选地址了。

### 本次线上处理

1. 定位：`ipconfig` 显示以太网是 `192.168.1.13`，26 个 `.strm` 全是 `.17`。
2. 备份后批量改写 `.strm`（备份在 `.local-tmp/strm-backup-20261109-105037`），
   `update settings set value='http://192.168.1.13:6086' where key='play_domain'`。
   `s.setting` 每次查库、无进程内缓存，所以改完立即生效，不用重启。
3. 验证：
   - Emby 容器 `wget` 新地址 → `302` → `206 Partial Content`；
   - Emby 条目 `MediaSources.Path` 已变成新地址（`.strm` 改动被文件监听捕获）；
   - `docker exec emby ffprobe` 从新地址解出 `hevc 3840x2160` + `dts` + 多条字幕，
     即「真的能播」。

### 踩到的坑：容器在跑时别用宿主进程直写 SQLite

第一次是**一边让容器跑着、一边用宿主机 python 改 `data/strm115.db`**。
写完立刻读是 `.13`，但 `docker compose up -d` 重建容器后又变回 `.17`——
容器内进程 mmap 的 `-shm` 看不到宿主机写入的 WAL 帧（Docker Desktop 的
bind mount 不保证 msync 可见），退出/checkpoint 时用自己那份页把值盖回去了。

正确姿势：`docker compose stop strm115` → 改库（`PRAGMA wal_checkpoint(TRUNCATE)`
收干净 WAL）→ `up -d`。停服只有几秒，且 `.strm` 已经是对的，
这段时间 Emby 播放不受影响（只有重新解析直链会失败）。

另外 `.env` 里的 `PLAY_DOMAIN` 也是旧 IP：bootstrap 会用它覆盖 `settings.play_domain`
（`bootstrap.go:236`，只要非空且不同就写）。只改库不改 `.env`，
下次跑 bootstrap 又会被打回旧地址——两处必须一起改。

### 测试

`internal/httpapi/netfollow_test.go`：候选过滤（回环/169.254/172.16-31/域名）、
自动改写 `.strm` 与设置并落通知、健康时零改动、无候选只告警一次、
跨网段候选不采用、域名型地址不猜。

### 仍然建议做的事（代码兜不住）

在路由器上给这台机做 **DHCP 保留**，或直接给播放代理一个固定域名。
自愈是补救，不是替代——它要等用户重新打开一次后台页面才能学到新地址；
只用 Emby、不开后台的话，第一轮仍然会「播不了 + 一条告警」。

---

### 5.44 一个库挂两个网盘：多来源（v0.2.7）

需求一句话：**一个媒体库同时挂 115 和夸克，获取资源时夸克的转进夸克，只有 `.strm` 里的播放地址不同。**

数据模型：

- 新增表 `library_sources(id, library_id, account_id, remote_cid, remote_path, last_sync_*, created_at)`，
  `UNIQUE(library_id, account_id, remote_cid)`。启动时 `backfillSources()` 从 `libraries` 的老列
  自动回填（`INSERT OR IGNORE`），老库升级不用手工动数据。
- `libraries.account_id / remote_cid / remote_path` 保留为「主来源」，等于 `sources[0]`。
  老代码、老 `.strm`、bootstrap 都还能用；新代码一律读 `library_sources`。
- `files.account_id` 从「整库一个」变成「一行一个」。`UpsertFiles` 优先用行里带的账号，
  播放代理靠这一列选凭证。

播放地址格式（**这是本次唯一的对外契约变化**）：

| 网盘 | `.strm` 内容 |
| --- | --- |
| 115 | `http://nas:6086/d/{pickcode}.mkv?/片名.mkv`（与原格式完全一致） |
| 夸克 | `http://nas:6086/d/q/{fid}.mkv?/片名.mkv` |

`q/` 前缀由 `strm.PlayIDForKind` 生成、`strm.SplitPlayKind` 解析。为什么不给 115 也加前缀：
历史 `.strm` 全是无前缀的，改了等于让整库失效。为什么不靠「台账反查」单独解决：
台账可能为空（接管旧媒体目录、被清过），那时只能靠前缀里的类型兜底。

`play.Proxy` 的解析顺序：**台账（pickcode → account）→ 前缀（kind → 该类型第一个账号）→ 兜底账号**。
台账优先是刻意的：文件被转过账号时，台账才是事实。

同步侧的三条规则（`internal/sync`）：

1. `run()` 遍历 `library_sources`，每个来源单独 `scanSource()`；`.strm` 的 `playID` 按来源类型生成。
2. 清理失效条目必须**按来源**（`IterStaleByAccount`）。用 115 那半边的 `seen` 去清全库，
   会把夸克半边删掉再写回来，Emby 每轮都看到条目消失又出现。
3. 跨来源同名目标只留一份：落到同一个 `.strm` 路径时留体积大的，同体积留先扫到的来源。
   同**来源**内部的同名冲突仍然走「加 `[pickcode]` 后缀」的老路子，不要混。

自动整理对夸克的支持见 §5.45：TV 接口没有移动/改名/删除，改走网页接口。

验收方式（都在单测里）：`internal/sync/multisource_test.go`（地址前缀、台账归属、
同名留大、按来源清理）、`internal/play/proxy_test.go`（q/ 前缀路由、台账优先）、
`internal/store/sources_test.go`（回填、重复挂载、按账号清理）。

### 5.45 自动整理：多规则 + 夸克网页接口（v0.2.9）

两个变化：

**一、整理配置从「一条」变成「多条」。** `organize.Config` 新增 `rules[]`：

```json
{"enabled":true,"batch":30,"rules":[
  {"id":"r1","account_id":1,"source_cid":"…","source_name":"/115strm/待整理","movie_lib_id":1,"tv_lib_id":2},
  {"id":"r2","account_id":2,"source_cid":"…","source_name":"/115strm/转存","movie_lib_id":1}
]}
```

- 老配置（只有同名字段、没有 `rules`）由 `Filled()` / `ActiveRules()` 现场合成一条，
  **不需要迁移数据**；老字段始终等于 `rules[0]`，入库后自动整理、重试这些老路径照旧可用。
- 空规则（前端加了行没填）会被丢掉，否则 `Ready()` 会误判「配好了」实际一条都跑不动。
- 目标库的根目录按**规则账号**取：`organizeTargetLib` 返回 `(lib, rootCID)`，夸克规则拿库里
  夸克来源的 `remote_cid`。以前所有规则都用 `lib.RemoteCID`（主来源），夸克规则会把文件
  往回搬进 115 的 cid。
- 接口：`POST /api/organize/run` 收 `rule_id`（只跑一条）；`POST /api/organize/scan` 返回
  `{rules:[{id,account_kind,source_name,items,stats,error}]}`，`?rule_id=` 可只扫一条。

**二、夸克的移动/改名/删除/建目录改为走网页接口**（`internal/pan/driverquarktv/web.go`）。

| 操作 | 接口 | 说明 |
| --- | --- | --- |
| 建目录 | `POST /file` | 同步返回 `data.fid` |
| 移动 | `POST /file/move` | body `{filelist, to_pdir_fid, action_type:1}`，返回 task_id |
| 改名 | `POST /file/rename` | body `{fid, file_name}` |
| 删除 | `POST /file/delete` | body `{filelist, action_type:2}`，返回 task_id |

- 根：`https://drive-pc.quark.cn/1/clouddrive?pr=ucpro&fr=pc`，要**网页 Cookie**
  （`driverquarktv.Credential.WebCookie`，和分享转存用的是同一个）。
- 任务完成判定只看 `status==2`：夸克完成时 `task_status` 可能是 `null`，要求两者都为 2
  会每次成功都干等到超时（v0.2.8 在转存链路上踩过一次）。
- 没有 Cookie 时抛 `pan.ErrAuth` 并提示「用 Cookie 方式重新添加账号」；保存整理配置时
  `validateOrganizeConfig` 会提前拦住，不会等跑起来才逐条失败。
- **列目录仍然走 TV 接口**：网页接口的 list 字段和 TV 不一致，且 TV 接口对频率更宽容；
  移动后的条目下一轮同步用 TV 列目录就能看到，不需要额外刷新。

一条实测结论：夸克网页接口的目录 `size` 常给 0（不递归统计），所以「最小体积」过滤对
夸克目录不生效 —— 这是接口限制，不是漏判。

## 6. 配置项

### 环境变量（启动期）

| 变量 | 默认 | 说明 |
|---|---|---|
| `ADDR` | `:6060` | 管理后台 |
| `PROXY_ADDR` | `:6086` | 播放代理（必须能被播放器访问） |
| `DATA_DIR` | `./data` | SQLite + 会话密钥 |
| `MEDIA_ROOT` | `./media` | `.strm` 写在这里 |
| `ADMIN_USER` | `admin` | 仅当数据库里还没有管理员账号时生效 |
| `ADMIN_PASSWORD` | 空 | 同上。留空 = 首次访问网页时设置（推荐） |

### 数据库 settings（运行期可改，界面里改的就是这些）

| key | 用途 |
|---|---|
| `play_domain` | 写进 `.strm` 的地址前缀，如 `http://192.168.1.10:6086` |
| `server_kind` | `emby`（jellyfin 共用同一实现） |
| `server_url` | 管理端访问媒体服务器的地址（容器内是 `http://emby:8096`） |
| `server_key` | Emby API Key |
| `server_path_from` / `server_path_to` | 路径映射：本地 `/media` ↔ Emby 看到的路径 |
| `sync_keep_ext` | `.strm` 文件名是否保留原扩展名 |
| `sync_delete_stale` | 网盘侧消失时是否删本地 `.strm` |
| `sync_min_size` | 小于该字节数的文件不生成 `.strm`（挡掉样片/字幕） |
| `admin_user` | 管理员账号（网页里设置，明文） |
| `admin_pass` | `pbkdf2-sha256$<iter>$<salt>$<hash>`，不存明文（见 `internal/httpapi/auth.go`） |
| `imdb_api_key` | 可选。IMDb 官方 GraphQL Key；没有就走免费 suggestion 接口。**属于 secret：读接口只回 `_set`/`_masked`**。检测入口在「电影墙数据源」卡片（`GET /api/imdb/check`）。**刮削与海报墙已不用**，只剩资源搜索换英文名兜底 |
| `tmdb_api_key` | 电影墙 / 刮削 / 海报墙 / 订阅 / 播出日历共用的 Key。留空时回落到 `STRM115_TMDB_KEY` 环境变量（源码不内置 Key，两者都为空时功能不可用）。**属于 secret** |
| `notify_config` | 8 个通知渠道的配置 JSON（含各渠道密钥） |
| `notify_events` | 逗号分隔的事件名。**这一行不存在 = 全开；存在但为空 = 全关**。共 10 个事件：`offline_add / offline_ok / offline_fail / organize_ok / sync_ok / sync_fail / scrape_ok / ingest / subtitle_ok / delete` |
| `notify_events_version` | 订阅清单的版本标记（当前 `2`）。只用来判断「要不要把新增事件补进去」，别当业务数据用 |
| `tg_bot_chat_ids` | 机器人白名单，逗号分隔的 chat id。**空 = 还没绑定**，下一个说话的会话会被写进来 |
| `tg_bot_offset` | `getUpdates` 游标。不存盘的话重启会把上一条分享再转存一遍 |
| `tg_bot_status` | 机器人最近一次状态（`运行中（@xxx）` / Token 无效 / 被 webhook 占了），界面直接显示 |
| `offline_auto_sync` | 离线任务完成后自动同步对应媒体库，`"0"` 关闭 |
| `sub_interval_minutes` | 订阅**追平后**的检查周期（15 分钟 ~ 7 天，默认 360）。新集没资源时另用 10 分钟短重试，不受它影响 |
| `tmdb_base` | TMDB v3 反代根（如 `https://你的域名/3`）。**留空 = 恢复官方直连**，与密钥的「留空不修改」相反。设置页 → 电影数据源里有输入框 |
| `tmdb_image_base` | 可选。留空时按 `/t/p` 从 `tmdb_base` 推导；显式填 `https://image.tmdb.org/t/p` 可只让接口走反代 |
| `calendar_source` | 播出日历的时刻增强源：`tvmaze`（默认，免 Key）/ `tvdb` / `off`。**老库没这一行时按 `tvmaze` 兜底**；非法值 `PUT` 直接 400。见 §5.24 |
| `tvmaze_base` | TVMaze 公共 API 的反代根（留空 = 官方 `https://api.tvmaze.com`）。**不是密钥**：留空保存 = 恢复官方。`GET /api/settings` 用 `tvmaze_upstream.api` 回显；测试入口 `POST /api/calendar/tvmaze/test` |
| `tvdb_api_key` | 可选。TVDB v4 项目 Key，**属于 secret**。个人免费申请已不可用，仅在 `calendar_source=tvdb` 时生效；测试入口 `POST /api/calendar/tvdb/test` |
| `tvdb_pin` | 可选。订阅型 Key 的 PIN，与 Key 一起 POST `/login` 换 Token；**属于 secret** |
| `tvdb_base` | TVDB v4 接口根（留空 = 官方 `https://api4.thetvdb.com/v4`）。**不是密钥**：留空保存 = 恢复官方；改完下一轮日历立即生效。`GET /api/settings` 用 `tvdb_upstream.api` 回显真正生效的地址 |
| `scrape_config` | 刮削偏好 JSON：`{auto, write_nfo, fetch_images, min_size_mb, rate_ms}` |
| `organize_config` | 自动整理配置 JSON：`{enabled, account_id, source_cid, source_name, movie_lib_id, tv_lib_id, batch, min_size_mb, sync_after, auto_ingest}`。`auto_ingest` **缺省 = 开**（入库后自动整理，源目录固定 `res_save_cid`，不看 `source_cid`）。见 §5.27 |
| `subtitle_config` | 字幕偏好 JSON：`{auto, translate, bilingual, api_base, model, workers}` |

### bootstrap 专用（只在 `docker compose run bootstrap` 时读）

`PLAY_DOMAIN`、`EMBY_URL`、`EMBY_SKIP`、`EMBY_ADMIN_USER`、`EMBY_ADMIN_PASSWORD`、
`EMBY_API_KEY`、`EMBY_APP_NAME`、`PAN115_COOKIE`、`PAN115_CID`、`PAN115_ACCOUNT_NAME`、
`LIBRARY_NAME`、`LIBRARY_TYPE`、`LIBRARY_KEEP_EXT`、`BOOTSTRAP_WAIT_EMBY`、
`BOOTSTRAP_WAIT_SYNC`、`BOOTSTRAP_TIMEOUT`、`BOOTSTRAP_STRM_URL`。
详见 `.env.example` 的注释。

---

## 7. HTTP API 契约

**鉴权**：除 `/`、`/api/session`、`/api/setup`、`/api/login`、`/api/logout` 外，
全部要求 `sid` cookie。

管理员凭据存在数据库里，**没有「不鉴权」模式**：
数据库里还没有账号时，除上述几个接口外一律 401（`{"error":"…","setup_needed":"1"}`），
前端据此弹出「首次设置」表单。`ADMIN_USER` / `ADMIN_PASSWORD` 只在
「数据库里还没有账号」时用于初始化，之后改密码走 `POST /api/credentials`。

```
GET    /api/session                      → {setup_needed, authed, user, version, uptime_sec}
POST   /api/setup                        {username,password} → {ok,user} + Set-Cookie（仅未初始化时）
POST   /api/login                        {username,password} → {ok,user} + Set-Cookie: sid
POST   /api/logout
POST   /api/credentials                  {current_password,username,password} → 改账号密码；旧会话全部失效
GET    /api/instance                     → {version, accounts[], libraries[], media_root, play_domain, kinds[]}
GET    /api/dashboard                    → {server, services[], emby{configured}, totals, libraries[],
                                            jobs[], runs[], recent[], last_sync_at, last_full_sync}
                                         总览面板首屏。totals 里 bytes 是台账 SUM(size)，不是 115 剩余空间。
                                         recent 按 media_meta.updated_at 倒序，剧集按剧根合并成一张卡
                                         （episodes 记集数）；poster 只在磁盘上真有那张图时才给地址。
                                         **刻意不探 Emby**：探活要走外部 HTTP，会拖住首屏；前端并行调
                                         /api/settings/server-status 再合并。
GET    /api/tasks                        → {running[], summary, libraries[], generated_at}
                                         running[] 一行 = 一个真的在跑的 Job：id 是 `kind:libraryID`。
                                         percent 为 **-1** 表示总量未知（同步：边扫边发现），
                                         前端据此画流动条而不是 0%；
                                         cancelable 为 false 时前端不渲染「取消」按钮。
                                         summary = {running, offline_watching, history_total,
                                         failed_24h, last_error}
GET    /api/tasks/history?limit=          → {runs[]}  ← 只有同步有流水账（sync_runs）
POST   /api/tasks/cancel  {kind,library_id} → {ok}
                                         没在跑 → 409；kind 不认 / 缺 library_id → 400
POST   /api/tasks/run     {kind,library_id,full?,refresh?} → 202
                                         先判 kind 再查库（参数写错要报「只支持 sync 和 scrape」，
                                         而不是「库不存在」）；full=全量、refresh=忽略元数据缓存。
                                         历史里的「重跑」打的是同一个接口，没有第二套入口
POST   /api/tasks/clear                  → {removed}  只删已结束的 sync_runs，
                                         正在跑的那行留着（否则用户以为任务凭空消失）
GET    /api/accounts                     → [{id,kind,name,created_at}]     ← credential 永不返回
DELETE /api/accounts/{id}
GET    /api/accounts/{id}/stats          → {live, stats{requests, requests_today, day, ...}}
                                         ← requests_today / day 是后加的（5.37③）：本地时区当天次数，
                                           每分钟由 panpool 落进 api_calls 表，重启后 seed 回来；
                                           今天还没被调用过（内存里没有 Snapshot）时从表里兜底
GET    /api/accounts/{id}/browse?cid=    → {cid, entries[{id,name,is_dir,size}]}
POST   /api/115/qr                       → {id, png(dataURL), expires_in}
GET    /api/115/qr/{id}                  → {status: waiting/scanned/allowed/expired, account?}
                                         ← **是长轮询**：没扫码时会挂到 ~30s 才回 waiting。
                                           前端必须串行轮询（响应回来再排下一次），否则会同时压十几个在途请求。
POST   /api/115/cookie                   {cookie,name} → account
POST   /api/115/share                    {url?|share_code+password,cid?} → {ok,title,files,already,cid,account_id}
                                         把一条 115 分享转存进自己的网盘（资源搜索的「存到 115」）
                                         url 支持 115.com / 115cdn.com / anxia.com 的 /s/<code>[?password=]，
                                         后端自己从里面认分享码与提取码；也接受只给分享码
                                         cid 留空 → 设置项 res_save_cid，再空就是根目录 0
                                         already=true 表示 115 回 errno 4100024「已经转存过」，不算失败
                                         先 share/snap 校验（标题 / 封禁 / 过期）再 share/receive 转存
GET    /api/libraries                    → [library...]
POST   /api/libraries                    {account_id,name,remote_cid,collection_type,keep_ext,local_path?} → library
                                         local_path 留空 = 媒体根/英文名（见 localDirName）；非 ASCII 会被拒
DELETE /api/libraries/{id}
PUT    /api/libraries/{id}/keep-ext      {keep_ext} → 直链是否带扩展名，下一轮同步生效
GET    /api/libraries/{id}/reldir?cid=   → {rel_dir}  网盘目录相对库根的路径（ParentID 上溯）
POST   /api/libraries/{id}/sync          {force?,delete_stale?,min_size?} → 202 progress
                                         force=true 是全量（内容没变也重写），不传是增量
GET    /api/libraries/{id}/job           → progress{running,phase,dirs,files,written,removed,failed,err}
POST   /api/libraries/{id}/cancel
GET    /api/files/browse?account_id=&cid=&dirs_only= → {account_id,cid,entries[{id,name,is_dir,size,mtime,strm?,library?}]}
                                         strm 非空 = 台账里已有对应 .strm（文件页的「已入库」角标）
POST   /api/files/move                   {account_id,ids[],to_cid} → {moved,to_cid}  只动网盘
POST   /api/files/remove                 {account_id,ids[]} → {removed,hint}  115 回收站可恢复
GET    /api/strm/overview                → {libraries[],accounts[],runs[],totals,last_sync_at,last_full_sync,domain}
GET    /api/strm/runs?limit=             → {runs[]}
GET    /api/strm/orphans?library_id=     → {libraries[{library_id,name,root,scanned,extra[],missing[],err}],scanned_at}
POST   /api/strm/orphans/clean           {library_id,extra[],missing[],all?} → {removed,rows,skipped,failed}
                                         all=true 时服务端现场重扫再全清（扫描接口每类只回 200 条）
GET    /api/libraries/{id}/files?limit=&offset= → {files[], total}
POST   /api/libraries/{id}/server        → 在 Emby 建库并刷新（幂等；mixed 会降级成 movies）
GET    /api/settings                     → {所有 setting key；server_key 只给 server_key_masked/server_key_set}
PUT    /api/settings                     {k:v,...} 只更新传入的键
POST   /api/settings/test-server         {kind,url,api_key} → {ok, libraries}；字段留空则回落到已存设置
GET    /api/settings/server-status       → {kind,url,configured,connected,libraries,server_name,version,error}
POST   /api/settings/connect-server      {kind,url,username,password} → 走完向导/Authenticate/建 API Key 并落库

POST   /api/find                         {text} → {text,has_links,links[],media_name,imdb_id,imdb,watch,notes[]}
                                         纯查询，不碰网盘；文本上限 64KB；空输入 422
POST   /api/offline/add                  {account_id?,uris[],save_cid,label} → {ok,hashes[],count}
                                         一次最多 20 条；不传 account_id 时自动挑第一个支持离线的账号
                                         部分成功也会报错，但错误信息里带「已提交 N 条」
GET    /api/offline/tasks?account_id=&page= → {account,total,tasks[{hash,name,size,status,percent,dir_id,add_time}]}
                                         status ∈ waiting/running/done/failed
GET    /api/net/check                    → {proxy,results[{key,name,host,group,via,ok,status,ms,error}],ok,total}
                                         未配代理时只有 via=direct；404/5xx 也算可达（证明网络通）
                                         前端入口在「设置 → 网络检测」子页（#/settings/check，旧 #/net 仍兼容）

GET    /api/calendar?start=YYYY-MM-DD&refresh=1
                                       → {start,end,days[{date,weekday,label,items[]}],total,cached,
                                          cached_at,sources{tmdb,tvdb,tvdb_configured,tvdb_error,shows},notice}
                                         items[] 一条 = 一张卡：kind=episode|movie、code(SxxExx)、
                                         episode_title、air_time、network、poster、runtime、rating、
                                         subscribed、premiere、new_show、source=tmdb|tvdb、page_url
                                         TMDB 出剧单/分集/电影，TVDB 补时刻与电视网（可选，失败只写 notice）
                                         整周缓存：本周 30min、其它周 12h；refresh=1 跳过读仍写回
                                         冷抓并发几十次 TMDB，handler 超时 60s、前端 90s
POST   /api/calendar/tvdb/test         {} 或 {api_key,pin} → {ok,message|error}
                                         带凭据时测输入框里的那份（不必先保存）；恒返回 200

GET    /api/wall?type=movie|series&providers=nfx,amp&genre=scf&sort=popular|top|new&page=&limit=
                                       → {items[],source,page,has_more,provider_options,genre_options,sort}
                                         数据源只有 TMDB（JustWatch 已下线）：discover + 并发补 external_ids
                                         （30 部约 1~2s，进程内缓存）；source 恒为 "tmdb"
                                         genre_options 随 kind 变（剧集没有「恐怖/音乐」，有「真人秀」）
GET    /api/wall/watch?id=&type=&country= → {link,providers[]}
                                         只查 flatrate（订阅制）；租片商店不算「在哪儿能看」
                                         单条失败也只返回空 providers，不让详情弹窗报错
GET    /api/res/search?q=&en=&kind=tg|torrent&limit=&refresh=1
                                       → {items[],sources[{key,name,kind,ok,count,ms,error,cached}],
                                          query,site_query,channel_query,en_lookup,took_ms,cached}
                                         TG 走 t.me/s/<ch>?q= 公开预览（中文 + 英文各一次，合并去重）；
                                         站点走各自 RSS/JSON/HTML 搜索页（6 个内置 + 自定义源）
                                         q=主查询词；en=种子站查询词（英文原名），留空且 q 含中文时
                                         先用 TMDB /search/multi（3s）换名，失败退回 IMDb（4s）
                                         channel_query=TG 频道额外用的「不带年份」英文名
                                         en_lookup=ok|fail：fail 表示没换到英文名，种子站只能按中文搜
                                         refresh=1 才跳过源级缓存；来源/体积切换是前端本地过滤，不发请求
                                         kind 仍兼容（订阅会传 torrent），网页一律一次搜全部来源
                                         limit 是每个源取几条（默认 20，上限 50）；电影墙弹窗用 12
                                          部分源失败不影响整体；全部失败时 items 为空且带 error

GET    /api/subscriptions?state=         → {items[],counts{},builtin_tmdb,interval_minutes}
                                         items[] 带 watermark_season/episode（「已提交到第几集」）、
                                         total_episodes、last_result、next_check_at、fail_count
                                         builtin_tmdb=true 表示没填自己的 Key（在用环境变量兜底或根本没 Key）
POST   /api/subscriptions   {tmdb_id,media_type,title?,year?,library_id?,save_cid?,quality?,min_size?,max_size?}
                                         → {id}；重复 tmdb_id+type → 409
                                         剧集一律真查 /tv/{id} 校正标题/年份/IMDb ID/总集数，
                                         查不到就 502 —— 建一条判断不了集数的订阅只会永远停在 waiting
PUT    /api/subscriptions/{id}           → {ok}；只让改画质/体积/库/目录/状态，**水位线不给改**
DELETE /api/subscriptions/{id}           → {ok}；已经交出去的离线任务不受影响
POST   /api/subscriptions/{id}/run       → {ok,result}  同步跑一轮并返回结论
                                         （撞上自动轮询 → 409；最坏 3 分钟，前端超时给 200s）
GET    /api/subscriptions/seasons?tmdb_id= → {show,seasons[]}
                                         seasons[] 字段是 season / episode_count / name / air_date
GET|PUT /api/subscriptions/config        → {interval_minutes,builtin_tmdb}
                                         PUT 范围 15 分钟 ~ 7 天，落到 sub_interval_minutes
                                         TG 条目会带 share_code / share_pwd / share_url / share_host：
                                         正文或 href 里的 115 链接、或「分享码：xx」都被 linkparse 认出来，
                                         前端据此显示「存到 115」，不再需要点开 t.me 复制粘贴
                                         种子的 magnet / torrent 字段由前端直接提交 /api/offline/add（离线下载）
                                         源清单来自设置：tg_channels（逗号分隔，留空用 DefaultChannels）、
                                         res_sites（内置站 key）、res_sites_custom（多行 `名称 | 地址`）
                                         DefaultChannels 只放 115 / 夸克方向：QukanMovie、yunpanquark、quarkshare
                                         （按实际抓页统计：阿里/百度频道对本项目的转存没有意义）
                                         自定义源走通用 RSS 解析，认 Torznab 的 <torznab:attr name="infohash">，
                                         所以私有站 / Jackett / Prowlarr 的结果一样带磁力

POST   /api/res/test   {kind:tg|site|custom, target, query?}
                                       → {kind,name,ok,count,ms,query,url,error,notice,samples[]}
                                         测的是请求里的 target，不要求先保存设置
                                         ok 只表示「源能正常返回」，0 条也可能 ok=true（带 notice 说明原因）
                                         query 默认 1080p（资源站上几乎必然有结果，能验证到磁力解析）
                                         custom 的 target 传那一行原文（`名称 | 地址` 或只有地址）
                                         url 里的 apikey / passkey / token 一律打码成 ***
                                         参数错（kind 不认识、target 为空）才 400；源连不上是 200 + ok=false

GET    /api/imdb/check?key=            → {ok,mode:official|suggestion,key_set,ms,sample,english,error,note}
                                         key 留空则用设置里存的那份；都没配就走免 Key 接口
                                         真查一次「沙丘2」：能分出断网 / Key 无效 / 配额用光
                                         Key 无效但免 Key 通时 ok=true、mode=suggestion、note 说明已降级
                                         前端入口：设置 → 网络与资源 → 电影墙数据源 → 检测 IMDb

GET    /api/notify                       → {config,channels[],events[],event_labels[],auto_sync,registered[]}
                                         密钥永不回传明文，改由 secret_set{"tg.token":true} 标记「已存」
                                         event_labels 每项带 desc，界面直接渲染说明文字
PUT    /api/notify                       {config,events?,auto_sync?} → {ok,channels[]}
                                         密钥字段留空 = 不修改（唯一实现在 mergeNotifySecrets）
                                         events 不传 = 不动订阅；传 [] = 全关（两者不能混）
POST   /api/notify/test                  {channel?} → {ok,message,channel,results[]}
                                         传 channel 只测该渠道；渠道没启用返回 results=[]
                                         且 message="xxx 还没启用"，不当成功
POST   /api/notify/tg/discover           {token?} → {bot,chats[],webhook,hint}
                                         token 留空用已保存的那份；getMe 验 Token、getUpdates 挖会话
                                         Bot 设了 webhook 时 Telegram 返 409，这里翻译成
                                         webhook=true + hint「先删 webhook」，不算错误

GET    /api/notify/tg/bot                → {token_set,bound,allowed[{chat_id,label}],status,commands[]}
                                         双向机器人状态。token 从 notify 配置的 tg.token 读；
                                         bound=false 表示还没人给 Bot 说过话
                                         注意：机器人进程在跑的时候会消费 getUpdates，
                                         所以 tg/discover 大概率返回空 —— 会话已经自动绑定了
PUT    /api/notify/tg/bot                {chat_ids?|reset} → {ok,chat_ids}
                                         chat_ids 逗号分隔，群是负数；非数字直接 400
                                         reset=true 清空白名单，下一个说话的会话重新绑定
                                         白名单存 settings.tg_bot_chat_ids；
                                         空时还会回落到 notify 配置里的 tg.chat_id

GET    /api/organize/config               → {config,accounts[],libraries[],stats{}}
                                         配置存 settings.organize_config；stats 是台账各状态计数
PUT    /api/organize/config               {config} → {ok,config}
                                         源目录账号与目标库账号不一致直接 400（跨账号无法移动）
POST   /api/organize/scan                 {} → {source,items[],stats{batch,total,movie,series,skip,done}}
                                         只读：列源目录 + 逐条识别，不写库、不动文件
                                         items[].skip 非空表示这条不会整理（非视频/太小/认不出）
                                         items[].confidence < 70 的由前端默认不勾选
POST   /api/organize/run                  {file_ids?:[]} → {ok,moved,failed,skipped,synced[],errors[],requests}
                                         真移动。不传 file_ids 就按扫描顺序跑 batch 条
                                         逐级确保「片名 (年份)/Season N」存在（Mkdir 缓存在 dirCache）
                                         每条结果都写 organize_records；sync_after=true 时触发对应库增量同步
POST   /api/organize/preview              {name,is_dir?,size?} → {item}
                                         拿真实识别器试算，页面「识别规则」用它
GET    /api/organize/records?status=&limit=&offset= → {items[],total,stats{}}
POST   /api/organize/records/{id}/ignore  → {ok,status:"ignored"}；扫描不再出现该 file_id
POST   /api/organize/records/{id}/retry   → {ok,message}；重新识别 + 重新移动（不复用旧目标）
POST   /api/organize/records/clear        {status?} → {ok,removed}；只删台账，不动网盘文件

POST   /api/libraries/{id}/scrape        {overwrite?, refresh_cache?} → 202 + 进度对象
                                         整库刮削。同一库同时只允许一个任务（冲突 409）
                                         文件名里有 tt… 的条目走「按 ID 直查」（1 次 meta），
                                         没有才按片名搜索；结束后按 subtitle_config.auto 串补字幕
GET    /api/libraries/{id}/scrape        → 进度 {phase,total,done,matched,unmatched,failed,fetched,cached,…}
                                         phase: parse/meta/write/done/error/canceled
POST   /api/libraries/{id}/scrape/cancel {} → {ok}
GET    /api/libraries/{id}/media?limit=&offset=&status= → {total,stats{ok,unmatched,error},meta_cached,items[]}
                                         items 里是刮削结果（含海报预览地址 /api/media/{id}/image）
POST   /api/libraries/{id}/subtitles     {redo?} → 202 + 进度；redo=true 重做已有字幕的条目
GET    /api/libraries/{id}/subtitles/job → 字幕任务进度 {phase,total,done,saved,translated,missing,failed,…}
POST   /api/media/{id}/subtitle          单条补字幕 → 补字幕结果
POST   /api/media/{id}/scrape            {imdb_id,overwrite?,subtitles?} → {ok,media,subtitle?}
                                         按 IMDb ID 走 TMDB /find 重刮单条：先查本地缓存，
                                         未命中只打 /find + 详情；就地改写 nfo/海报/台账，
                                         可选顺手补中文字幕；imdb_id 接受整条 imdb.com 链接；
                                         IMDb ID 由 external_ids 带回来，不再无条件照抄输入；
                                         旧字段先清空再写，写完后后台刷新该目录
GET    /api/media/{id}/image?kind=backdrop → 本地海报/背景图（给前端预览；路径穿越有防护）
                                         剧集的单集行里 poster_file 是空的（图写在剧根，一季只下一次），
                                         所以 kind=series 且该字段为空时按剧根兜底到 poster.jpg / fanart.jpg

我的海报墙（key 形如 emby:121 / local:9）：
GET    /api/catalog?library_id=&source=&type=&genre=&q=&sort=&page=&limit=&refresh=1
                                         → {items[],total,has_more,source,genres[],libraries[],
                                            emby_ok,emby_checked,emby_total,local_total,
                                            tmdb_missing,hint,provider_note}
                                         source=tmdb（默认）/ emby / local；空值与非法值兜 tmdb
                                         tmdb：骨架取本地台账（海报按「磁盘 → 刮削存的
                                         poster_url → TMDB」三级兜底，**有图就不联网**）；
                                         只有连一张图都没有的卡片才去查 TMDB（≤4 并发、30 分钟
                                         缓存），refresh=1 才强制整页重查。以前是无条件现查，
                                         6 张卡要 2-3 秒（见 5.37①），改完 1.5ms
                                         查不到不删卡片，保留本地字段并写 tmdb_missing / hint
                                         emby：按 Path 反推「库 + rel_path」，连不上自动落 local
                                         local：纯台账，不联网；剧集一律按剧根合并成一张卡
GET    /api/catalog/item?key=&source=tmdb → {item,episodes[],local{...}} 详情 + 分集 + 台账
                                         source=tmdb 时用同一份 TMDB 详情覆盖展示字段，
                                         避免列表中文名、详情跳回台账旧名字
                                         item.cast[] = {name,role,image}：只取 Emby 的
                                         People 里 Type=Actor 的（中文名来自 nfo），
                                         image 是 /api/catalog/image?key=emby:{personID}&kind=poster&w=160；
                                         **没有 PrimaryImageTag 时给空串**，前端退回首字头像，
                                         不要拼一个必然 500 的地址
GET    /api/catalog/image?key=&kind=poster|backdrop&w=
                                         → 图片代理（同源，浏览器不接触 API Key / 内网地址）
POST   /api/catalog/item  {key,title,original,year,rating,genres[],overview,imdb_id,tmdb_id}
                                         → {ok,item,local}；只重写本地 nfo + 台账 + 元数据缓存，
                                         全程不联网；tmdb_id 变了优先走 RescrapeOneByTMDbID
                                         （1 次详情，连 /find 都省），否则 imdb_id 变了走
                                         RescrapeOne（TMDB /find）。两者都变以 tmdb_id 为准
DELETE /api/catalog/item?key=&purge=1    → {ok,removed_files,removed_rows,remote_files,
                                         purged,emby_removed,purge,note}
                                         默认只移索引；purge=1 才把 115 源文件删进回收站。
                                         purge 放最前面，网盘删失败就整体中止、本地不动
POST   /api/catalog/scan  {library_id,cid,rel_dir,remote_path}
                                         → {videos,will_add,will_skip,items[]} 预览，不写盘
POST   /api/catalog/add   同上           → 202 {added,skipped,paths,scrape_started}
                                         写 .strm + 登记台账 + 自动刮削 + 刷新 Emby
                                         rel_dir 留空时按库 remote_path 剥前缀（catalogRelDir），
                                         保证与同步生成的 .strm 路径一致、不产生重复条目
```

播放代理（独立端口）：
```
GET /healthz          → 200 ok
GET /d/{id}[.ext]?/name → 302 到 115 CDN；失败返回 404/429/503 + 纯文本原因
```

---

## 8. 部署形态

### 8.1 一条命令（推荐）

```bash
cp .env.example .env      # 只需要改 PLAY_DOMAIN
docker compose --profile emby up -d --build
```

`up` 之后启动的是 `strm115`（常驻）和 `emby`（常驻，profile `emby`）。
打开 `http://宿主机IP:6060`，**第一次访问会要求设置管理员账号和密码**
（写进 `./data/strm115.db` 的 `admin_user` / `admin_pass`，不进 `.env`）。
之后在网页里加 115 账号、建库、同步、接 Emby。

`strm115-bootstrap` 现在在 profile `auto` 里，默认不启动 —— 正常流程用网页。
需要无人值守时：

```bash
docker compose --profile auto run --rm bootstrap
```

它会自动做：等两个服务就绪 → 必要时走完 Emby 向导 → 建 API Key →
写入 `play_domain`/`server_*` → 加 115 账号（有 Cookie 时）→ 建媒体库（有 CID 时）
→ 全量同步（有播放域名时）→ 在 Emby 建库并扫描。

**`strm115-bootstrap` 显示 `Exited (0)` 是正常状态，不是崩溃。**
它需要 `ADMIN_USER` / `ADMIN_PASSWORD` 与网页里设置的一致（或数据库还没初始化时用它建号）。

### 8.2 不用 compose 手动跑

```bash
go build -o strm115 ./cmd/server
DATA_DIR=./data MEDIA_ROOT=/media ./strm115     # 首次访问网页设置账号密码
# 一次性引导（可以跨机执行，只要有网络能访问管理端和 Emby）
PLAY_DOMAIN=http://192.168.1.10:6086 EMBY_URL=http://192.168.1.10:8096 \
  PAN115_CID=电影 EMBY_ADMIN_PASSWORD=xxx \
  BOOTSTRAP_STRM_URL=http://127.0.0.1:6060 ADMIN_USER=admin ADMIN_PASSWORD='网页里设的密码' \
  ./strm115 bootstrap
```

### 8.3 Windows 一键装机

`setup/1-install.ps1`（管理员，装 WSL2 + Docker Desktop）→ 重启 →
`setup/2-deploy.ps1`（起服务、自动探测局域网 IP 写进 `.env`）。

---

## 9. 本地开发：**不需要每次构建镜像**

这是本项目最重要的开发效率约定。

### 9.1 改后端

```powershell
powershell -ExecutionPolicy Bypass -File scripts\dev.ps1
# 等价于：
#   $env:DATA_DIR=".dev\data"; $env:MEDIA_ROOT=".dev\media"
#   go run ./cmd/server
```

`go run` 是增量编译，改完到生效通常 2~5 秒。开发数据在 `.dev/`，
和容器用的 `./data` 完全隔离，不会污染真实账号。

> **别拿生产库的副本再起一个实例做验证 —— 除了 TG 机器人会打架。**
> 机器人 Token 存在 `settings` 里，副本里也有，于是本地实例会和容器**同时**
> `getUpdates` 同一个 Bot，服务端回 `409 Conflict: terminated by other getUpdates
> request`，两边的收消息都断（容器日志里那句「Bot 被 webhook 占着」就是这个，
> 别当成用户配错了 webhook）。复制库做 UI 验证时，先把副本里的
> `settings.notify_config` 里 Telegram 那段 Token 清掉（或整行删掉），
> 或者干脆只验证不经过机器人的页面。

### 9.2 改前端

两条路，按场景选：

**（一）构建进二进制**（发布、验证真实路径用）

```bash
scripts/web-build.sh      # npm install（首次）+ vite build → internal/httpapi/webdist
go build ./cmd/server     # 或 go run ./cmd/server
```

**（二）vite 开发服务器**（改样式/组件时用，有 HMR）

```bash
scripts/web-build.sh --dev
# http://127.0.0.1:5273，/api 反向代理到 127.0.0.1:6060
```

改 `.vue` / `.css` 即时热更新，不用重启 Go 进程。要调接口时先把后端起起来
（`scripts/dev.ps1` 或容器都行）。

> 上一版的单文件界面（`internal/httpapi/index.html` + `/legacy` 路由 + `WEB_DEV_FILE`）
> 已经删掉：两套 UI 共用同一批接口，改一处要改两遍，排查时还容易看错页面。
> 现在只跑 `go build` 而没构建前端时，`/` 会回一个 503 说明页并打印要跑的命令
> （以前是静默退回老界面，看起来像正常打开）。

### 9.3 什么时候才需要重建镜像

只有这三种：
1. 改了 `Dockerfile` / `entrypoint.sh`
2. 改了 `go.mod`（依赖变化）
3. 要验证容器内的权限、挂载、健康检查行为

其余情况一律 `go run`。

### 9.4 热重载（可选）

想连 `go run` 都省掉，装 [air](https://github.com/air-verse/air)：

```bash
go install github.com/air-verse/air@latest
air --build.cmd "go build -o .dev/strm115.exe ./cmd/server" --build.bin ".dev/strm115.exe"
```

保存 Go 文件自动重启；改前端配合 `scripts/web-build.sh --dev` 热更新，两边都是即时生效。

### 9.5 测试

```bash
go test ./...            # 全部 internal 包
go test ./... -race      # CI 里跑的版本
go vet ./...
gofmt -l .               # 应该无输出
```

---

## 10. 手工验证清单（怀疑哪里坏了按这个顺序查）

```bash
# 1. 服务活着吗
curl -s http://127.0.0.1:6060/api/session
curl -s http://127.0.0.1:6086/healthz

# 2. 播放代理在不在（没有账号时应当是 404/503，不是连接失败）
curl -s -o /dev/null -w '%{http_code}\n' -A 'Infuse/7' http://127.0.0.1:6086/d/nonexist.mkv

# 3. 115 账号有效吗（401/403 = Cookie 过期）
curl -s -b cookie.txt http://127.0.0.1:6060/api/accounts

# 4. 同步到底走到哪一步
curl -s -b cookie.txt http://127.0.0.1:6060/api/libraries/1/job

# 5. Emby 那边通不通
curl -s -H "X-Emby-Token: <key>" http://127.0.0.1:8096/System/Info
curl -s -H "X-Emby-Token: <key>" http://127.0.0.1:8096/Library/VirtualFolders

# 6. 容器里的真实身份（别用 docker exec id，那是镜像默认 user，不是主进程）
docker top strm115 -o pid,user,args

# 7. 识别接口（页面已下线）：一条磁力链应当认出片名 + IMDb + 平台（纯查询，不动网盘）
curl -s -b cookie.txt -X POST -H 'Content-Type: application/json' \
  -d '{"text":"magnet:?xt=urn:btih:def03a36d725ad7cae62444bbe8bb7c720254cfb"}' \
  http://127.0.0.1:6060/api/find

# 8. 离线提交：重复链接会得到 422「任务已存在」（这条能通就说明通道是活的）
curl -s -b cookie.txt -X POST -H 'Content-Type: application/json' \
  -d '{"uris":["magnet:?xt=urn:btih:def03a36d725ad7cae62444bbe8bb7c720254cfb"],"save_cid":"0"}' \
  http://127.0.0.1:6060/api/offline/add
#   → 502 + invalid character 'd'  = 又退回老 RSA 通道了，看约束 5.8
#   → 422「任务已存在 / 链接无效」 = 正常，115 确实收到了

# 9. 通知：先配一个 webhook 再点「发送测试」，或者直接
curl -s -b cookie.txt -X POST http://127.0.0.1:6060/api/notify/test
#   → {"error":"还没有启用的通知渠道"} 说明当前没配渠道（不是 bug）

# 10. 刮削：整库跑一遍，第二次的 fetched 必须是 0（全走缓存）
curl -s -b cookie.txt -X POST -H 'Content-Type: application/json' -d '{}' \
  http://127.0.0.1:6060/api/libraries/1/scrape
curl -s -b cookie.txt http://127.0.0.1:6060/api/libraries/1/scrape
#   → phase=done 后看 fetched/cached；fetched>0 且是第二次跑 = 缓存坏了
ls media/movies/... | grep -E 'nfo|jpg|srt'   # 产物要与 .strm 同目录

# 11. 字幕：redo=true 会把已有字幕的条目也重做一遍（真实请求 OpenSubtitles）
curl -s -b cookie.txt -X POST -H 'Content-Type: application/json' -d '{"redo":true}' \
  http://127.0.0.1:6060/api/libraries/1/subtitles
```

登录拿 cookie：
```bash
curl -c cookie.txt -X POST -H 'Content-Type: application/json' \
  -d '{"password":"你的密码"}' http://127.0.0.1:6060/api/login
```

---

## 11. 已经踩过的坑（改到相关代码前务必看）

### Emby 侧（全部在 4.10.2 真机实测）

1. **`/System/Info/Public` 不返回 `StartupWizardCompleted`。**
   4.8 时代返回，4.10 不返回了。判断向导是否完成只能看
   `GET /Startup/Configuration`：**200 = 未完成，401 = 已完成**。
   代码：`mediaserver/emby_admin.go` 的 `WizardCompleted`。

2. **`POST /Auth/Keys` 返回 204 且没有响应体。**
   拿不到 key 本身，必须再 `GET /Auth/Keys` 按 `AppName` 找回来。
   而且列表是 `{"Items":[...]}` 包装的，不是裸数组。
   按文档直觉写会得到「创建成功但 key 是空字符串」。

3. **建库时路径必须真实存在**，否则 400 `The specified path does not exist`。
   所以顺序必须是「先建本地目录（strm115 建库时会 MkdirAll）→ 再让 Emby 建库」。
   如果 Emby 容器和 strm115 没挂同一个卷，这一步必然失败。

4. **登录用 `X-Emby-Authorization`，不是 `X-Emby-Token`。**
   缺了这个头，`/Users/AuthenticateByName` 返回 400 而不是 401，报错很误导。
   日常接口两个头都发（`emby.go` 的 `do`），兼容 Emby/Jellyfin。

5. **建库 `POST /Library/VirtualFolders` 返回 204，不返回 ItemId。**
   必须回读 `/Library/VirtualFolders` 按名字匹配拿 ItemId。

### Docker / Windows 侧

6. **`docker exec <c> id` 显示 root ≠ 降权失败。**
   那是镜像默认 user。要看 `docker top <c> -o user` 才是主进程身份。

7. **Git Bash 会把 `/media` 这类参数改写成 Windows 路径。**
   `docker exec emby ls /media` 实际执行的是 `ls C:/Program Files/.../media`。
   调试容器内路径时前面加 `MSYS_NO_PATHCONV=1`。

8. **`.sh` 被 git 转成 CRLF 后，容器里报 `not found`**（看起来像缺 shell）。
   `.gitattributes` 已强制 LF，Dockerfile 里还有一道 `sed -i 's/\r$//'`。
   两道保险，别删。

9. **PowerShell 5.1 默认按 ANSI 读文件**，UTF-8 中文注释会乱码。
   脚本一律纯 ASCII；`.env` 必须 UTF-8 **无 BOM**（有 BOM 会让 compose 读坏第一行）。

10. **SQLite 用 WAL 模式，`/data` 不能放 NFS/SMB**，网络文件系统的锁语义不可靠。

### 设计层面

11. **`.strm` 的 URL 里带 `?/文件名带扩展名`。**
    扩展名是给播放器判断「能不能直放」用的；`?` 后面是文件名，
    让 Emby 不必额外请求一次 HEAD。改这个格式会同时影响播放和刮削。

12. **`CleanRel` 是唯一的安全边界。** 网盘里的目录名可能是别人起的
    （分享转存进来的也会走这条路）。`..`、盘符、多余斜杠都在这里被吃掉，
    任何新增的路径拼接都必须经过它。

---

## 12. 测试策略

原则：**凡是分支逻辑，一律抽成零 IO 的纯函数再测。**

| 包 | 测什么 |
|---|---|
| `strm` | 路径净化、URL 生成、穿越攻击、原子写 |
| `pan` | 节流间隔、重试分类、熔断冷却 |
| `store` | upsert 幂等、失效清理、分页 |
| `play` | UA 参与缓存 key、限流、错误码映射 |
| `sync` | 假网盘全量同步：写入/改名清理/失效删除/MinSize 过滤 |
| `httpapi` | 鉴权、参数校验、路径边界 |
| `mediaserver` | 假 Emby：向导状态判定、API Key 幂等、错误密码 |
| `bootstrap` | 假 strm115 + 假 Emby：完整链路、幂等重跑、缺料降级 |

写新功能时的要求：**至少给「判定逻辑」写表驱动测试**，
不要只测 happy path。参考 `internal/sync/sync_test.go` 的写法。

---

## 13. 路线图（按依赖顺序）

### v0.2 夸克网盘
1. 新建 `internal/pan/driverquark/driver.go`，实现 `pan.Driver` 的 9 个方法
2. 在 `init()` 里 `pan.Register("quark", Open)`
3. 业务代码**一行不用改**（这是抽象是否成立的检验标准）

### v0.3 增量同步
目前只有手工全量。需要：
1. 接入 115 的生活事件（或定时增量扫描）
2. 事件去重（115 会重放）
3. **抑制自己触发的变更**：整理操作会产生事件，不抑制会自我循环

### v0.4 双向一致（用户明确要求的下一个功能）
「影视的增删改查同步网盘和 Emby；Emby 的操作也影响这两个」：
1. Emby Webhook（`/emby/webhooks`）接收 `item.deleted` / `item.updated`
2. 反查 `files` 台账 → 找到对应 `pick_code`
3. 调 `pan.Manager` 删除/移动网盘文件
4. **必须做幂等**：Emby 的 webhook 会重发，重复删除要安全

### v0.5 播放兜底
目前只有 302。当 CDN 被限速或播放器不支持 302 时，需要服务端中转
（`play/proxy.go` 已预留位置，注意**不要设 `WriteTimeout`**，会切断长流）。

---

## 14. 接手第一步（给下一个 AI 的具体指令）

按这个顺序读，30 分钟能上手：

1. `internal/pan/manager.go` 顶部注释 —— 理解为什么节流是硬需求
2. `internal/sync/sync.go` 的 `run()` —— 理解同步的 5 个阶段
3. `internal/strm/strm.go` 的 `Content()` / `CleanRel()` —— 理解 `.strm` 格式
4. `internal/play/proxy.go` 的 `handleDirect` —— 理解 302 怎么来的
5. `internal/bootstrap/bootstrap.go` 的 `Run()` —— 理解「一键」覆盖了哪些步骤
6. 本文档 §5（约束）和 §11（坑）

然后跑一遍：

```bash
go test ./...              # 应当全部 internal 包通过
powershell -File scripts\dev.ps1   # 本地起服务，浏览器打开 6060
```

**改代码时的硬性要求**
- 不引入 npm/前端构建链（§5.6）
- 不绕过 `pan.Manager` 直接调 driver（§5.1）
- 不改小 `MinGap`（§5.1）
- 新分支逻辑必须配纯函数测试（§12）
- 提交前 `gofmt -w . && go vet ./... && go test ./...`

**遇到 115 接口变化**：先看 `github.com/SheltonZhu/115driver` 是否已更新，
不要在业务代码里手写 115 的私有接口（签名算法一旦变会全线崩）。
