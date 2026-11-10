package main

// `strm115 sync --library <id>`：不经过网页、直接在命令行跑一次全量同步。
//
// 存在的理由：容器里 /media 的属主、定时任务、以及「网页没打开也要跑」的场景
// （cron / 群晖任务计划）都需要一个不带 HTTP 鉴权的入口。它和网页上点
// 「全量同步」走的是同一个 sync.Service，参数也从同一张 settings 表读，
// 所以两边行为一致，不会出现「命令行能跑、网页跑不了」这种分叉。
//
//	docker compose exec --user 1000:1000 strm115 /app/strm115 sync --library 1

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"strm115/internal/config"
	"strm115/internal/mediaserver"
	"strm115/internal/pan"
	"strm115/internal/panpool"
	"strm115/internal/store"
	syncsvc "strm115/internal/sync"
)

func runSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	libID := fs.Int64("library", 0, "媒体库 id（网页「媒体库」页 URL 里能看到）")
	domain := fs.String("domain", "", "播放域名，覆盖设置里的 play_domain")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法：strm115 sync --library <id> [--domain http://IP:6086]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *libID <= 0 {
		fs.Usage()
		return 2
	}

	cfg := config.Load()
	st, err := store.Open(filepath.Join(cfg.DataDir, "strm115.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败：", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lib, err := st.GetLibrary(ctx, *libID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "没有 id=%d 的媒体库：%v\n", *libID, err)
		return 1
	}

	get := func(k, def string) string {
		if v, ok := st.GetSetting(ctx, k); ok && v != "" {
			return v
		}
		return def
	}
	playDomain := *domain
	if playDomain == "" {
		playDomain = get("play_domain", "")
	}
	if playDomain == "" {
		fmt.Fprintln(os.Stderr, "还没设播放域名：先在网页「设置」里填，或加 --domain http://<IP>:6086")
		return 1
	}

	pool := panpool.New(st, pan.Options{
		MinGap:       1200 * time.Millisecond,
		MaxAttempts:  3,
		RiskCooldown: 10 * time.Minute,
	})
	defer pool.CloseAll()

	embyFactory := func(ctx context.Context) (mediaserver.Server, error) {
		return mediaserver.New(mediaserver.Config{
			Kind:   get("server_kind", ""),
			URL:    get("server_url", ""),
			APIKey: get("server_key", ""),
		})
	}
	svc := syncsvc.New(st, pool.Get, embyFactory, slog.Default())

	minSize, _ := strconv.ParseInt(get("sync_min_size", "0"), 10, 64)
	job, err := svc.Start(lib, syncsvc.Options{
		Domain:      playDomain,
		KeepExt:     lib.KeepExt,
		DeleteStale: get("sync_delete_stale", "true") != "false",
		MinSize:     minSize,
		Dedupe:      get("sync_dedupe", "true") != "false",
	})
	if errors.Is(err, syncsvc.ErrRunning) {
		fmt.Fprintln(os.Stderr, "这个库正在同步（可能是网页里刚点的），等它跑完再来")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动同步失败：", err)
		return 1
	}
	fmt.Printf("开始同步「%s」→ %s（播放域名 %s）\n", lib.Name, lib.LocalPath, playDomain)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		p := job.Snapshot()
		fmt.Printf("\r目录 %d · 视频 %d · 新写 %d · 清理 %d · 失败 %d   ", p.Dirs, p.Files, p.Written, p.Removed, p.Failed)
		if !p.Running {
			fmt.Println()
			if p.Err != "" {
				fmt.Fprintln(os.Stderr, "同步失败：", p.Err)
				return 1
			}
			if p.Phase == "canceled" {
				fmt.Fprintln(os.Stderr, "同步被取消")
				return 1
			}
			fmt.Printf("完成：%s\n", p.Message)
			return 0
		}
		select {
		case <-ctx.Done():
			fmt.Println("\n收到退出信号，取消同步…")
			job.Cancel()
			return 1
		case <-tick.C:
		}
	}
}
