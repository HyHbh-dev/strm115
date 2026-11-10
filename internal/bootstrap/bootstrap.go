// Package bootstrap 把「装好之后手动要点的那些步骤」变成一次自动执行。
//
// 覆盖的链路：
//
//	等 strm115 起来 → 登录 → 等 Emby 起来 → （必要时）走完 Emby 启动向导
//	→ 拿到 Emby API Key → 写入播放域名/服务器设置 → 加 115 账号（Cookie）
//	→ 建媒体库 → 全量同步 → 在 Emby 上建库并触发扫描
//
// 三条设计原则：
//  1. **幂等**。这个命令会被反复执行（改配置后重跑 docker compose up），
//     每一步都必须先查现状再动手，不能产生重复账号/重复库/重复 API Key。
//  2. **失败要说人话**。部署脚本的报错是给用户看的，不是给开发者看的，
//     所以每条错误都带「下一步该改哪个变量」。
//  3. **缺料不硬闯**。没有 Cookie 就跳过账号和同步，把 Emby 一侧先接通，
//     剩下的留给界面 —— 比整条链路一起失败有用得多。
package bootstrap

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"strm115/internal/mediaserver"
)

type Config struct {
	StrmURL    string // 管理后台地址（容器内用服务名）
	AdminUser  string // 管理员账号（首次初始化时也可由它决定，这里用于登录）
	AdminPass  string // 管理员密码
	PlayDomain string // 写进 .strm 的播放地址，必须是播放器能访问到的地址

	EmbyURL   string
	EmbyUser  string
	EmbyPass  string
	EmbyKey   string // 已手动建好 API Key 时可直接给，省掉管理员密码
	EmbyApp   string // API Key 的备注名，用来做幂等匹配
	WaitEmby  time.Duration
	WaitSync  time.Duration
	Timeout   time.Duration
	SkipEmby  bool
	UILocale  string
	Country   string
	Cookie    string
	AccountNm string
	LibName   string
	LibCID    string
	LibType   string
	KeepExt   bool
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// bootstrapPassword 取管理员密码：新变量名优先，其次兼容旧的 AUTH_PASSWORD。
func bootstrapPassword() string {
	if v := os.Getenv("ADMIN_PASSWORD"); v != "" {
		return v
	}
	return os.Getenv("AUTH_PASSWORD")
}

func LoadConfig() Config {
	libType := env("LIBRARY_TYPE", "movies")
	switch libType {
	case "movies", "tvshows", "mixed":
	default:
		libType = "movies"
	}
	return Config{
		StrmURL:    normalizeURL(env("BOOTSTRAP_STRM_URL", "http://strm115:6060")),
		AdminUser:  env("ADMIN_USER", "admin"),
		AdminPass:  bootstrapPassword(),
		PlayDomain: normalizeURL(os.Getenv("PLAY_DOMAIN")),

		EmbyURL:   env("EMBY_URL", "http://emby:8096"),
		EmbyUser:  env("EMBY_ADMIN_USER", "admin"),
		EmbyPass:  os.Getenv("EMBY_ADMIN_PASSWORD"),
		EmbyKey:   strings.TrimSpace(os.Getenv("EMBY_API_KEY")),
		EmbyApp:   env("EMBY_APP_NAME", "strm115"),
		WaitEmby:  envDur("BOOTSTRAP_WAIT_EMBY", 120*time.Second),
		WaitSync:  envDur("BOOTSTRAP_WAIT_SYNC", 180*time.Second),
		Timeout:   envDur("BOOTSTRAP_TIMEOUT", 20*time.Minute),
		SkipEmby:  os.Getenv("EMBY_SKIP") == "1",
		UILocale:  env("EMBY_UI_CULTURE", "zh-CN"),
		Country:   env("EMBY_COUNTRY", "CN"),
		Cookie:    strings.TrimSpace(os.Getenv("PAN115_COOKIE")),
		AccountNm: env("PAN115_ACCOUNT_NAME", "115"),
		LibName:   env("LIBRARY_NAME", "115"),
		LibCID:    strings.TrimSpace(os.Getenv("PAN115_CID")),
		LibType:   libType,
		KeepExt:   os.Getenv("LIBRARY_KEEP_EXT") != "0",
	}
}

// Report 是执行结果，用来在最后打一份「现在是什么状态」。
type Report struct {
	EmbyReady   bool
	EmbyKey     string
	WizardDone  bool
	AccountID   int64
	LibraryID   int64
	SyncStarted bool
	SyncDone    bool
	Attached    bool
	Warnings    []string
}

func (r *Report) warn(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

func step(n, total int, format string, a ...any) {
	args := make([]any, 0, len(a)+2)
	args = append(args, n, total)
	args = append(args, a...)
	slog.Info(fmt.Sprintf("[%d/%d] "+format, args...))
}

const totalSteps = 8

// Run 执行整条引导链。返回 error 只代表「有必须人介入的问题」，
// 可选步骤缺失（没 Cookie、没 CID）只记 warning。
func Run(ctx context.Context, cfg Config, log *slog.Logger) (*Report, error) {
	rep := &Report{}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	// ---------- 1. 管理后台 ----------
	step(1, totalSteps, "等待 strm115 管理后台 (%s)", cfg.StrmURL)
	api := newAPIClient(cfg.StrmURL, log)
	if err := api.WaitReady(ctx, 90*time.Second); err != nil {
		return rep, fmt.Errorf("strm115 没起来: %w", err)
	}
	if err := api.Login(ctx, cfg.AdminUser, cfg.AdminPass); err != nil {
		return rep, err
	}
	slog.Info("管理后台就绪")

	// ---------- 2. Emby 连通 ----------
	admin := mediaserver.NewAdmin(cfg.EmbyURL)
	if cfg.SkipEmby {
		rep.warn("EMBY_SKIP=1，跳过全部 Emby 步骤")
	} else {
		step(2, totalSteps, "等待 Emby (%s)", cfg.EmbyURL)
		info, err := admin.WaitReady(ctx, cfg.WaitEmby)
		if err != nil {
			// Emby 是可选组件（默认在 profile 里），连不上不该让整条链失败
			rep.warn("Emby 不可达（%s）。跳过接入，只配置网盘一侧。"+
				"启动 Emby 后重跑：docker compose run --rm bootstrap", cfg.EmbyURL)
			slog.Warn("Emby 不可达，跳过 Emby 相关步骤", "err", err)
		} else {
			rep.EmbyReady = true
			slog.Info("Emby 就绪", "server", info.ServerName, "version", info.Version)
		}
	}

	// ---------- 3. 启动向导 ----------
	if rep.EmbyReady {
		done, err := admin.WizardCompleted(ctx)
		if err != nil {
			rep.warn("无法判断 Emby 向导状态：%v", err)
		}
		switch {
		case done:
			rep.WizardDone = true
			slog.Info("Emby 启动向导已完成，保持现状")
		case !done:
			pass := cfg.EmbyPass
			generated := false
			if pass == "" {
				pass = randomPassword(18)
				generated = true
			}
			step(3, totalSteps, "完成 Emby 启动向导（管理员 %s）", cfg.EmbyUser)
			if err := admin.CompleteWizard(ctx, cfg.EmbyUser, pass, cfg.UILocale, cfg.Country); err != nil {
				rep.warn("完成 Emby 向导失败：%v", err)
			} else {
				rep.WizardDone = true
				if generated {
					slog.Warn("已为 Emby 生成管理员密码，请立刻保存（只显示这一次）",
						"user", cfg.EmbyUser, "password", pass)
				}
			}
		}
	}

	// ---------- 4. API Key ----------
	if rep.EmbyReady {
		step(4, totalSteps, "获取 Emby API Key")
		key, err := resolveEmbyKey(ctx, admin, cfg, rep)
		if err != nil {
			rep.warn("%v", err)
		} else {
			rep.EmbyKey = key
			slog.Info("API Key 就绪", "app", cfg.EmbyApp, "len", len(key))
		}
	}

	// ---------- 5. 写入设置 ----------
	step(5, totalSteps, "写入 strm115 设置")
	cur, err := api.Settings(ctx)
	if err != nil {
		return rep, fmt.Errorf("读取设置失败: %w", err)
	}
	patch := map[string]string{}

	playDomain := cfg.PlayDomain
	if playDomain == "" {
		if v := cur["play_domain"]; v != "" {
			playDomain = v
			slog.Info("沿用已有播放域名", "domain", v)
		} else {
			rep.warn("没有可用的播放域名：请在 .env 里设置 PLAY_DOMAIN=" +
				"http://<本机局域网IP>:6086（必须是播放器能访问的地址，不能是 127.0.0.1）")
		}
	}
	if playDomain != "" && playDomain != cur["play_domain"] {
		patch["play_domain"] = playDomain
	}

	if rep.EmbyKey != "" {
		patch["server_kind"] = "emby"
		patch["server_url"] = mediaserver.NormalizeBase(cfg.EmbyURL)
		patch["server_key"] = rep.EmbyKey
	}
	if len(patch) > 0 {
		if err := api.PutSettings(ctx, patch); err != nil {
			return rep, fmt.Errorf("写入设置失败: %w", err)
		}
		slog.Info("设置已更新", "字段", strings.Join(keysOf(patch), ", "))
	} else {
		slog.Info("设置无需变更")
	}

	if rep.EmbyKey != "" {
		if err := api.TestServer(ctx, "emby", mediaserver.NormalizeBase(cfg.EmbyURL), rep.EmbyKey); err != nil {
			rep.warn("Emby 连通性自检未通过：%v", err)
		} else {
			slog.Info("Emby 连通性自检通过")
		}
	}

	// ---------- 6. 115 账号 ----------
	step(6, totalSteps, "检查 115 账号")
	accts, err := api.Accounts(ctx)
	if err != nil {
		return rep, fmt.Errorf("读取账号失败: %w", err)
	}
	var acctID int64
	for _, a := range accts {
		if a.Kind == "115" {
			acctID = a.ID
			break
		}
	}
	switch {
	case acctID != 0:
		rep.AccountID = acctID
		slog.Info("已有 115 账号，跳过登录", "id", acctID)
	case cfg.Cookie == "":
		rep.warn("没有 115 账号，也没有 PAN115_COOKIE：打开后台「账号」页扫码添加，" +
			"然后重跑 docker compose run --rm bootstrap")
	default:
		a, err := api.CookieLogin(ctx, cfg.Cookie, cfg.AccountNm)
		if err != nil {
			rep.warn("用 PAN115_COOKIE 登录 115 失败：%v"+
				"（Cookie 会过期，从浏览器重新复制 UID/CID/SEID/KID）", err)
		} else {
			acctID = a.ID
			rep.AccountID = a.ID
			slog.Info("已添加 115 账号", "id", a.ID, "name", a.Name)
		}
	}

	// ---------- 7. 媒体库 + 同步 ----------
	var libID int64
	libs, err := api.Libraries(ctx)
	if err != nil {
		return rep, fmt.Errorf("读取媒体库失败: %w", err)
	}
	for _, l := range libs {
		if strings.EqualFold(l.Name, cfg.LibName) {
			libID = l.ID
			break
		}
	}

	step(7, totalSteps, "媒体库与同步")

	// PAN115_CID 允许直接填文件夹名：「电影」比「3555355939475293750」好记也好填，
	// 而且用户本来就只知道自己要哪个目录，不该被迫去查 cid。
	cid := cfg.LibCID
	if libID == 0 && acctID != 0 && cid != "" && !isNumeric(cid) {
		resolved, err := resolveCID(ctx, api, acctID, cid)
		if err != nil {
			rep.warn("%v", err)
			cid = ""
		} else {
			slog.Info("已按名称找到网盘目录", "name", cfg.LibCID, "cid", resolved)
			cid = resolved
		}
	}

	switch {
	case libID != 0:
		rep.LibraryID = libID
		slog.Info("媒体库已存在", "id", libID, "name", cfg.LibName)
	case acctID == 0:
		rep.warn("没有 115 账号，无法建库。扫码添加账号后重跑本命令")
	case cid == "":
		listTopDirs(ctx, api, acctID, rep)
	default:
		lib, err := api.CreateLibrary(ctx, map[string]any{
			"account_id":      acctID,
			"name":            cfg.LibName,
			"remote_cid":      cid,
			"collection_type": cfg.LibType,
			"keep_ext":        cfg.KeepExt,
		})
		if err != nil {
			rep.warn("创建媒体库失败：%v", err)
		} else {
			libID = lib.ID
			rep.LibraryID = lib.ID
			slog.Info("已创建媒体库", "id", lib.ID, "name", lib.Name, "local", lib.LocalPath)
		}
	}

	if libID != 0 {
		if playDomain == "" {
			rep.warn("没有播放域名，本次不触发同步（.strm 里的地址会是错的）")
		} else {
			job, err := api.Job(ctx, libID)
			if err != nil {
				rep.warn("读取同步状态失败：%v", err)
			}
			if job.Running {
				slog.Info("同步已在运行，等待它结束", "phase", job.Phase)
				rep.SyncStarted = true
			} else {
				j, err := api.StartSync(ctx, libID)
				if err != nil {
					rep.warn("启动同步失败：%v", err)
				} else {
					rep.SyncStarted = true
					slog.Info("已开始全量同步", "phase", j.Phase)
				}
			}
			if rep.SyncStarted {
				done := waitSync(ctx, api, libID, cfg.WaitSync, log)
				rep.SyncDone = done
				if !done {
					rep.warn("同步还在跑（超过 %s）：可以先让 Emby 扫一遍，"+
						"之后在后台点「同步到 Emby」再刷新", cfg.WaitSync)
				}
			}
		}
	}

	// ---------- 8. 接入 Emby ----------
	step(8, totalSteps, "在 Emby 上建库并触发扫描")
	switch {
	case rep.EmbyKey == "":
		rep.warn("Emby 未接入（没有 API Key），跳过建库")
	case libID == 0:
		rep.warn("没有媒体库，跳过 Emby 建库")
	default:
		res, err := api.AttachServer(ctx, libID)
		if err != nil {
			// 最典型的失败：Emby 容器里看不到这个路径（没挂同一个卷 / 缺路径映射）
			rep.warn("在 Emby 上建库失败：%v。"+
				"若提示路径不存在，说明 Emby 容器没有挂载同一个媒体目录，"+
				"或需要在「设置 → 路径映射」里填 容器内路径 → Emby 看到的路径", err)
		} else {
			rep.Attached = true
			slog.Info("Emby 库已就绪",
				"server", res["server"], "path", res["server_path"], "library_id", res["server_lib_id"])
		}
	}

	return rep, nil
}

// isNumeric 判断是否纯数字：115 的 cid 都是数字串，根目录是 "0"。
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// resolveCID 把「文件夹名」解析成 cid，只在根目录里找。
// 115 的 cid 是十几位数字，手抄容易错一位，写名字更符合人的习惯。
func resolveCID(ctx context.Context, api *apiClient, acctID int64, name string) (string, error) {
	entries, err := api.Browse(ctx, acctID, "")
	if err != nil {
		return "", fmt.Errorf("按名称查目录时读取网盘失败：%w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		if e.Name == name {
			return e.ID, nil
		}
		names = append(names, e.Name)
	}
	// 大小写不敏感再找一遍（英文目录名常见）
	for _, e := range entries {
		if e.IsDir && strings.EqualFold(e.Name, name) {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("网盘根目录下没有名为 %q 的文件夹（现有：%s）",
		name, strings.Join(names, "、"))
}

// listTopDirs 在用户还没选目录时，把网盘根目录下的文件夹和 cid 打出来。
//
// 这一步是刻意的「宁可多问一句」：替用户随便挑一个目录开始全量同步，
// 可能是一次几万个文件的下载请求，而选错目录只有他自己能判断。
func listTopDirs(ctx context.Context, api *apiClient, acctID int64, rep *Report) {
	entries, err := api.Browse(ctx, acctID, "")
	if err != nil {
		rep.warn("没有 PAN115_CID，列网盘目录也失败了：%v", err)
		return
	}
	dirs := make([]browseEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) == 0 {
		rep.warn("没有 PAN115_CID，网盘根目录下也没有子目录。请在后台建库")
		return
	}

	fmt.Println()
	fmt.Println("  网盘根目录下的文件夹（挑一个，把 cid 填进 .env 的 PAN115_CID）：")
	fmt.Println()
	const show = 20
	for i, d := range dirs {
		if i >= show {
			fmt.Printf("    …… 另有 %d 个未显示\n", len(dirs)-show)
			break
		}
		fmt.Printf("    %-32s cid=%s\n", truncate(d.Name, 30), d.ID)
	}
	fmt.Println()
	rep.warn("还没选目录：把上面某个 cid 填进 .env 的 PAN115_CID，" +
		"然后重跑 docker compose run --rm bootstrap")
}

// truncate 按字符截断，避免中文名把表格顶歪。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// resolveEmbyKey 按优先级拿 API Key：手动给的 > 已存在的 > 用管理员密码新建。
func resolveEmbyKey(ctx context.Context, admin *mediaserver.Admin, cfg Config, rep *Report) (string, error) {
	if cfg.EmbyKey != "" {
		return cfg.EmbyKey, nil
	}
	if cfg.EmbyPass == "" {
		return "", fmt.Errorf("Emby 已经初始化过，但没有 EMBY_ADMIN_PASSWORD，无法创建 API Key。" +
			"两种做法任选其一：在 .env 里填 EMBY_ADMIN_PASSWORD（你 Emby 管理员的密码），" +
			"或在 Emby 后台「设置 → 高级 → API 密钥」新建一个填到 EMBY_API_KEY，然后重跑")
	}

	// 刚完成向导时 Emby 可能还要一两秒才接受登录，这里重试几次
	var token string
	var err error
	for i := 0; i < 5; i++ {
		token, err = admin.Authenticate(ctx, cfg.EmbyUser, cfg.EmbyPass)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(i+1) * time.Second):
		}
	}
	if err != nil {
		return "", fmt.Errorf("用 %s 登录 Emby 失败（检查 EMBY_ADMIN_USER / EMBY_ADMIN_PASSWORD）：%v",
			cfg.EmbyUser, err)
	}

	key, err := admin.EnsureAPIKey(ctx, token, cfg.EmbyApp)
	if err != nil {
		return "", fmt.Errorf("创建 Emby API Key 失败：%w", err)
	}
	return key, nil
}

// waitSync 轮询同步进度。返回 true 表示在等待窗口内跑完了。
func waitSync(ctx context.Context, api *apiClient, libID int64, wait time.Duration, log *slog.Logger) bool {
	deadline := time.Now().Add(wait)
	lastReport := time.Time{}
	for {
		j, err := api.Job(ctx, libID)
		if err != nil {
			log.Warn("轮询同步进度失败", "err", err)
			return false
		}
		if !j.Running {
			if j.Err != "" {
				log.Error("同步失败", "err", j.Err, "phase", j.Phase)
				return false
			}
			log.Info("同步完成",
				"dirs", j.Dirs, "files", j.Files, "written", j.Written, "removed", j.Removed)
			return true
		}
		if time.Since(lastReport) > 10*time.Second {
			log.Info("同步中…", "phase", j.Phase, "dirs", j.Dirs, "files", j.Files, "written", j.Written)
			lastReport = time.Now()
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// randomPassword 生成管理员密码。字符集去掉了容易看错的 0/O/1/l/I，
// 因为这个密码可能是用户从日志里手抄下来的。
func randomPassword(n int) string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			b[i] = chars[i%len(chars)]
			continue
		}
		b[i] = chars[idx.Int64()]
	}
	return string(b)
}
