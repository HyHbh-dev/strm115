// strm115 —— 把 115 网盘映射成本地 STRM，播放时 302 直连 CDN。
//
// v1 范围：
//   - 登录 115（扫码 / Cookie）            → internal/pan/driver115
//   - 选一个网盘目录                        → /api/accounts/{id}/browse
//   - 全量遍历并写 .strm                    → internal/sync
//   - :6086/d/{pickcode} 302                → internal/play
//   - Emby 加库                             → internal/mediaserver
//
// 网盘的增删改查全部收敛在 internal/pan（接口 + 节流 + 熔断），
// 将来接夸克只需要加一个 driver 实现，业务代码一行不改。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"strm115/internal/bootstrap"
	"strm115/internal/config"
	"strm115/internal/httpapi"
	"strm115/internal/embygw"
	"strm115/internal/mediaserver"
	"strm115/internal/pan"
	"strm115/internal/panpool"
	"strm115/internal/play"
	"strm115/internal/store"
	syncsvc "strm115/internal/sync"

	_ "strm115/internal/pan/driver115" // 注册 115 驱动
)

// 构建时用 -ldflags "-X main.Version=v0.1.0" 注入
var Version = "v0.7.3-dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// 子命令：同一个二进制既当服务端，也当一次性引导程序。
	// 分成两个镜像会把「两个东西版本不一致」变成一个新的故障来源。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "bootstrap":
			os.Exit(runBootstrap())
		case "sync":
			os.Exit(runSync(os.Args[2:]))
		case "backup":
			os.Exit(runBackup(os.Args[2:]))
		case "version", "-v", "--version":
			fmt.Println(Version)
			return
		}
	}

	cfg := config.Load()
	cfg.MediaRoot = config.ResolveMediaRoot(cfg.MediaRoot)
	httpapi.Version = Version

	st, err := store.Open(filepath.Join(cfg.DataDir, "strm115.db"))
	if err != nil {
		slog.Error("打开数据库失败", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 节流参数：默认 1.2 秒。这个值不是性能参数，是「防封号」参数，
	// 调低之前请先读 internal/pan/manager.go 顶部的说明。
	pool := panpool.New(st, pan.Options{
		MinGap:       1200 * time.Millisecond,
		MaxAttempts:  3,
		RiskCooldown: 10 * time.Minute,
	})
	defer pool.CloseAll()
	// 「今天调了 115 多少次」要按分钟落库：只在内存里数的话，换镜像就归零，
	// 而 115 的风控额度是按天算的，数字偏小比没有更糟。
	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	pool.Start(poolCtx)

	embyFactory := func(ctx context.Context) (mediaserver.Server, error) {
		get := func(k string) string { v, _ := st.GetSetting(ctx, k); return v }
		return mediaserver.New(mediaserver.Config{
			Kind:   get("server_kind"),
			URL:    get("server_url"),
			APIKey: get("server_key"),
		})
	}

	syncSvc := syncsvc.New(st, pool.Get, embyFactory, slog.Default())

	// 播放代理要先于 httpapi 建：Emby 302 网关把「条目 → 直链」这件事
	// 直接委托给它，两条入口共用同一份直链缓存与限流状态。
	proxy := play.New(
		pool.Get,
		func(ctx context.Context, pickCode string) (int64, bool) { return st.AccountByPickcode(ctx, pickCode) },
		func(ctx context.Context, kind string) (int64, bool) { return st.AccountByKind(ctx, kind) },
		firstAccount(st),
		slog.Default(),
	)

	// Emby 302 网关和播放代理共用一个端口（cfg.ProxyAddr）：
	// 先绑定播放代理，配置齐全时再换成网关；网关内部会把 /d/ 还给播放代理。
	gw := embygw.NewManager(slog.Default())
	defer gw.Close()
	gw.Bind(cfg.ProxyAddr, proxy.Handler())
	if err := embygw.ApplyFromStore(context.Background(), st, gw, proxy.Resolve, slog.Default()); err != nil {
		slog.Info("Emby 302 网关未启用，端口只做播放跳转", "reason", err)
	}

	api := httpapi.New(httpapi.Deps{
		Cfg:     cfg,
		Store:   st,
		Pool:    pool,
		Sync:    syncSvc,
		Log:     slog.Default(),
		Gateway: gw,
	})

	adminSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	proxySrv := &http.Server{
		Addr:              cfg.ProxyAddr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 播放是 302，不需要长连接超时；但别设 WriteTimeout，将来加中转时会切断流
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("管理后台已启动", "addr", cfg.Addr, "media_root", cfg.MediaRoot, "version", Version)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		slog.Info("播放入口已启动（播放跳转 + Emby 网关同端口）", "addr", cfg.ProxyAddr)
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		slog.Error("服务启动失败", "err", err)
	case s := <-sig:
		slog.Info("收到退出信号，正在收尾", "signal", s.String())
	}

	// 收尾顺序：先停 HTTP（不再接新请求）→ 等后台同步停手 → 关连接池。
	// 给同步留时间是因为它可能正在写 .strm；硬杀会留下 .tmp 文件。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(ctx)
	_ = proxySrv.Shutdown(ctx)
	// 机器人是长轮询，不主动停的话会挂着到超时；它不写文件，可以早停
	api.Close()
	pool.CloseAll()
	slog.Info("已退出")
}

func firstAccount(st *store.Store) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return st.FirstAccountID(ctx)
}

// runBootstrap 执行一次性引导（docker compose 里的 bootstrap 服务）。
//
// 退出码的约定：只有「必须人介入才能继续」的配置问题才返回 1；
// 缺 Cookie、缺网盘目录这类「还没准备好」的情况返回 0 并打警告 ——
// 否则 docker compose up 看起来像部署失败了，实际上服务本身是好的。
func runBootstrap() int {
	cfg := bootstrap.LoadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("开始自动配置", "strm", cfg.StrmURL, "emby", cfg.EmbyURL)
	rep, err := bootstrap.Run(ctx, cfg, slog.Default())

	fmt.Println()
	if rep != nil {
		printBootstrapReport(rep, cfg)
	}
	if err != nil {
		slog.Error("自动配置中断", "err", err)
		fmt.Println()
		fmt.Println("修好上面这条错误后重跑：docker compose run --rm bootstrap")
		return 1
	}
	return 0
}

func printBootstrapReport(rep *bootstrap.Report, cfg bootstrap.Config) {
	mark := func(ok bool) string {
		if ok {
			return "[完成]"
		}
		return "[跳过]"
	}
	rows := []struct {
		ok    bool
		label string
		value string
	}{
		{rep.EmbyReady, "Emby 可达", cfg.EmbyURL},
		{rep.WizardDone, "Emby 启动向导", ""},
		{rep.EmbyKey != "", "Emby API Key", maskKey(rep.EmbyKey)},
		{rep.AccountID != 0, "115 账号", fmt.Sprintf("id=%d", rep.AccountID)},
		{rep.LibraryID != 0, "媒体库", fmt.Sprintf("id=%d name=%s", rep.LibraryID, cfg.LibName)},
		{rep.SyncStarted, "全量同步", fmt.Sprintf("等待窗口内完成=%v", rep.SyncDone)},
		{rep.Attached, "Emby 建库+扫描", ""},
	}

	fmt.Println("================ 自动配置结果 ================")
	for _, r := range rows {
		fmt.Printf("  %s %-18s %s\n", mark(r.ok), r.label, r.value)
	}

	if len(rep.Warnings) > 0 {
		fmt.Println()
		fmt.Println("---------------- 还需要你处理 ----------------")
		for i, w := range rep.Warnings {
			fmt.Printf("  %d) %s\n", i+1, w)
		}
	}
	fmt.Println("=============================================")
}

func maskKey(k string) string {
	if len(k) <= 8 {
		if k == "" {
			return "(无)"
		}
		return "***"
	}
	return k[:4] + "…" + k[len(k)-4:]
}
