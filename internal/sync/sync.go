// Package sync 实现 v1 的核心链路：网盘目录 → 本地 .strm → 通知媒体服务器。
//
// 一条不变的规矩：**遍历只读，写盘只写我们自己的东西**。
// 同步不移动、不改名、不删除任何网盘文件；它只做「把目录树镜像成一组 URL 文件」。
// 网盘的写操作属于后面的「整理」模块，两者混在一起是这类系统最常见的翻车原因。
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/mediaserver"
	"strm115/internal/pan"
	"strm115/internal/scrape"
	"strm115/internal/store"
	"strm115/internal/strm"
)

// Options 是一次同步的运行参数。
type Options struct {
	Domain      string // 播放域名，如 http://192.168.1.10:6086
	KeepExt     bool   // 直链路径里是否带扩展名
	DeleteStale bool   // 网盘侧消失的文件，本地 .strm 是否跟着删
	MinSize     int64  // 小于这个体积的视频跳过（0 = 不过滤）
	PageSize    int    // 每页条目数
	// Dedupe 表示「同一部作品只留一条 .strm」。
	//
	// 网盘目录里同一部片常有多个副本（mp4 + mkv + 不同压制组），全写出来
	// Emby 会出现两三条同名条目，其中只有带 nfo 的那条有简介和海报。
	// 默认开启：保留体积最大的那个，其余的 .strm 与台账行一起清掉。
	Dedupe bool
	// Force 为真时，即使 .strm 内容与目标一致也重写一遍。
	//
	// 这是「全量」和「增量」唯一的实质区别：增量只补缺失/变化的，
	// 全量会把每个文件的 mtime 刷新，Emby 因此会整库重扫——
	// 换域名、改命名规则之后要的就是这个效果。
	Force bool
	// Trigger 是这轮同步的来源，只用于流水账展示（full / incremental / auto）。
	Trigger string
}

var ErrRunning = errors.New("这个库正在同步")

// Job 是一次同步的运行时状态。前端轮询它拿进度。
type Job struct {
	mu  sync.Mutex
	p   Progress
	ctx context.Context
	cnl context.CancelFunc
}

type Progress struct {
	LibraryID  int64     `json:"library_id"`
	Phase      string    `json:"phase"` // scan / write / clean / emby / done / error / canceled
	Dir        string    `json:"dir"`
	Dirs       int       `json:"dirs"`
	Files      int       `json:"files"`
	Written    int       `json:"written"`
	Removed    int       `json:"removed"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	Running    bool      `json:"running"`
	Message    string    `json:"message"`
	Err        string    `json:"err"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// 多来源进度：一个库可能同时扫 115 和夸克，界面要知道现在扫到哪一家。
	SourceName  string `json:"source_name"`
	SourceIndex int    `json:"source_index"`
	SourceTotal int    `json:"source_total"`
}

func (j *Job) update(fn func(*Progress)) {
	j.mu.Lock()
	fn(&j.p)
	j.mu.Unlock()
}

func (j *Job) Snapshot() Progress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.p
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.p.Running
}

func (j *Job) Cancel() { j.cnl() }

// Service 持有「同步」这一侧的依赖。
type Service struct {
	st   *store.Store
	open func(ctx context.Context, accountID int64) (*pan.Manager, error)
	emby func(ctx context.Context) (mediaserver.Server, error)
	log  *slog.Logger

	mu   sync.Mutex
	jobs map[int64]*Job
}

func New(st *store.Store,
	open func(context.Context, int64) (*pan.Manager, error),
	emby func(context.Context) (mediaserver.Server, error),
	log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, open: open, emby: emby, log: log, jobs: map[int64]*Job{}}
}

// Job 返回某个库当前的同步状态（没有则 nil）。
func (s *Service) Job(libraryID int64) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[libraryID]
}

// Start 后台跑一次全量同步。同一个库同时只允许一个任务。
func (s *Service) Start(lib store.Library, opt Options) (*Job, error) {
	s.mu.Lock()
	if j, ok := s.jobs[lib.ID]; ok && j.Running() {
		s.mu.Unlock()
		return nil, ErrRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ctx: ctx, cnl: cancel}
	j.p = Progress{
		LibraryID: lib.ID,
		Phase:     "scan",
		Running:   true,
		StartedAt: time.Now(),
		Message:   "准备开始",
	}
	s.jobs[lib.ID] = j
	s.mu.Unlock()

	// 流水账开跑即落库：这样即使进程中途被重启，页面上也留得下「有一轮曾经开始」。
	trigger := strings.TrimSpace(opt.Trigger)
	if trigger == "" {
		trigger = "manual"
	}
	runID, err := s.st.StartSyncRun(context.Background(), lib.ID, lib.Name, trigger)
	if err != nil {
		// 记账失败不该拦住同步本身
		s.log.Warn("写同步流水账失败", "lib", lib.Name, "err", err)
	}

	go func() {
		defer cancel()
		err := s.run(ctx, j, lib, opt)
		j.update(func(p *Progress) {
			p.Running = false
			p.FinishedAt = time.Now()
			p.Dir = ""
			switch {
			case err == nil:
				p.Phase = "done"
				p.Message = "同步完成"
			case errors.Is(err, context.Canceled):
				p.Phase = "canceled"
				p.Message = "已取消"
			default:
				p.Phase = "error"
				p.Err = err.Error()
				p.Message = "同步失败"
			}
		})
		if runID > 0 {
			p := j.Snapshot()
			if e := s.st.FinishSyncRun(context.Background(), runID, p.Phase,
				p.Files, p.Written, p.Removed, p.Failed, p.Skipped, p.Message); e != nil {
				s.log.Warn("更新同步流水账失败", "id", runID, "err", e)
			}
			_ = s.st.PruneSyncRuns(context.Background(), 200)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("同步失败", "lib", lib.Name, "err", err)
		}
	}()
	return j, nil
}

// source 是「这个媒体库的一条网盘来源」。
//
// 一个库现在可以同时挂 115 的目录和夸克的目录：本地目录只有一个、Emby 库
// 只有一个，区别只在 .strm 里那行 302 地址（115 是 /d/{pickcode}，
// 夸克是 /d/q/{fid}）。
type source struct {
	ID         int64
	AccountID  int64
	Kind       string // 115 / quarktv
	RemoteCID  string
	RemotePath string
}

// Label 给进度条和日志用：优先显网盘目录名，没有就退到 id。
func (s source) Label() string {
	if s.RemotePath != "" {
		return s.RemotePath
	}
	if s.Kind == "quarktv" {
		return "夸克：" + s.RemoteCID
	}
	return s.RemoteCID
}

// librarySources 取这个库要扫的全部来源。
//
// 来源表为空时退回库上的主账号列：v0.2.6 及更早的数据没有来源行，
// 启动时的回填会补上，但手工插库/刚从旧库导入的场景仍可能没有。
func (s *Service) librarySources(ctx context.Context, lib store.Library) ([]source, error) {
	rows, err := s.st.ListSources(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	out := make([]source, 0, len(rows))
	for _, r := range rows {
		kind := r.AccountKind
		if kind == "" {
			kind, _ = s.st.AccountKind(ctx, r.AccountID)
		}
		out = append(out, source{
			ID: r.ID, AccountID: r.AccountID, Kind: kind,
			RemoteCID: r.RemoteCID, RemotePath: r.RemotePath,
		})
	}
	if len(out) == 0 && lib.AccountID != 0 {
		kind, _ := s.st.AccountKind(ctx, lib.AccountID)
		out = append(out, source{
			AccountID: lib.AccountID, Kind: kind,
			RemoteCID: lib.RemoteCID, RemotePath: lib.RemotePath,
		})
	}
	return out, nil
}

// claim 记录「某个 .strm 路径当前指向哪一家网盘的文件」。
//
// 有了它才能在跨来源时决定留谁：同一部片 115 和夸克各有一份、文件名又一样，
// 落到磁盘上就是同一个 .strm，而一个 .strm 只能写一个地址。
type claim struct {
	pickCode string
	fileID   string
	account  int64
	size     int64
}

func (s *Service) run(ctx context.Context, j *Job, lib store.Library, opt Options) error {
	if opt.PageSize <= 0 || opt.PageSize > 500 {
		opt.PageSize = 200
	}
	if strings.TrimSpace(opt.Domain) == "" {
		return errors.New("未配置播放域名（例如 http://192.168.1.10:6086）")
	}

	srcs, err := s.librarySources(ctx, lib)
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		return errors.New("这个媒体库还没挂网盘目录，请先在「账号与媒体库」里加一个")
	}

	// 老台账：file_id → 旧 rel_path，用于改名后清理残留
	existing, err := s.st.FileRelPaths(ctx, lib.ID)
	if err != nil {
		return err
	}

	// 同一部片两个网盘都有时只留一份：判据是「写到同一个 .strm 路径」。
	// 体积大的留（画质高）；体积一样就留先扫到的那个（来源顺序 = 添加顺序，
	// 115 通常排在前面，因为它的直链更稳）。
	claims := map[string]claim{}
	seenBySource := map[int64]map[string]struct{}{}
	var dropped []string

	j.update(func(p *Progress) { p.SourceTotal = len(srcs) })

	for i, src := range srcs {
		if err := ctx.Err(); err != nil {
			return err
		}
		j.update(func(p *Progress) {
			p.Phase = "scan"
			p.SourceName = src.Label()
			p.SourceIndex = i + 1
			p.Dir = ""
		})
		seen := make(map[string]struct{}, 1024)
		seenBySource[src.ID] = seen
		if err := s.scanSource(ctx, j, lib, src, opt, existing, claims, seen, &dropped); err != nil {
			return err
		}
		s.log.Info("来源扫描完成", "lib", lib.Name, "source", src.Label(), "files", len(seen))
	}

	// 去重后的多余副本：台账行也要清掉，否则下次同步它们又会被当成「已存在」。
	if len(dropped) > 0 {
		if err := s.st.DeleteFilesByFileIDs(ctx, lib.ID, dropped); err != nil {
			return err
		}
	}

	// —— 清理：台账里有、这次没扫到的 ——
	//
	// 必须按来源分开清。拿 115 那半边的「这次见到的 file_id」去清全库，
	// 会把夸克半边连同 .strm 一起删掉，下一轮又写回来——Emby 于是每轮都
	// 看到一批条目消失又出现。
	if opt.DeleteStale {
		j.update(func(p *Progress) { p.Phase = "clean"; p.Message = "清理失效条目" })
		for _, src := range srcs {
			seen := seenBySource[src.ID]
			if seen == nil {
				continue
			}
			var staleIDs []int64
			err := s.st.IterStaleByAccount(ctx, lib.ID, src.AccountID, seen, func(f store.File) error {
				p := filepath.Join(lib.LocalPath, filepath.FromSlash(f.RelPath))
				if err := os.Remove(p); err == nil {
					j.update(func(pp *Progress) { pp.Removed++ })
				}
				staleIDs = append(staleIDs, f.ID)
				if len(staleIDs) >= 500 {
					if err := s.st.DeleteFileIDs(ctx, staleIDs); err != nil {
						return err
					}
					staleIDs = staleIDs[:0]
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := s.st.DeleteFileIDs(ctx, staleIDs); err != nil {
				return err
			}
			_ = s.st.MarkSourceSynced(ctx, src.ID, time.Now(), len(seen))
		}
	}

	n, _ := s.st.CountFiles(ctx, lib.ID)
	if err := s.st.MarkLibrarySynced(ctx, lib.ID, time.Now(), n); err != nil {
		return err
	}

	// —— 通知媒体服务器 ——
	j.update(func(p *Progress) { p.Phase = "emby"; p.Message = "通知媒体服务器刷新" })
	if s.emby != nil {
		srv, err := s.emby(ctx)
		if err != nil {
			// 没配媒体服务器不是错误：v1 允许「只生成 STRM，手动加库」
			j.update(func(p *Progress) { p.Message = "未配置媒体服务器，跳过刷新" })
		} else if lib.ServerLibID != "" {
			msg := ""
			if err := srv.Refresh(ctx, lib.ServerLibID); err != nil {
				msg = "同步完成，但刷新媒体库失败：" + err.Error()
			}
			// 刷完再核对一次「.strm 到底写在哪、库到底扫哪」。
			// 这两件事以前只能靠肉眼对：库里加了路径、本地镜像目录改了名，
			// 同步照样报「完成」，Emby 里却一条都不出现 —— 用户看到的就是
			// 「为什么没同步进来」。宁可在这里多说一句。
			if warn := s.mirrorWarning(ctx, srv, lib); warn != "" {
				s.log.Warn("本地镜像目录不在媒体库的扫描路径下", "lib", lib.Name, "detail", warn)
				if msg == "" {
					msg = warn
				} else {
					msg += "；" + warn
				}
			}
			if msg != "" {
				j.update(func(p *Progress) { p.Message = msg })
			}
		}
	}
	return nil
}

// scanSource 扫一个来源：遍历 → 写 .strm → 记台账。
//
// 和原来「一个库一个账号」时的区别只有三处：
//  1. Manager 按来源的账号取，而不是按库；
//  2. .strm 的播放标识带上网盘类型（夸克加 q/ 前缀，115 保持原样）；
//  3. 台账行的 account_id 记这个文件真正的归属，播放时代理才知道找谁要直链。
func (s *Service) scanSource(ctx context.Context, j *Job, lib store.Library, src source,
	opt Options, existing map[string]string, claims map[string]claim,
	seen map[string]struct{}, dropped *[]string) error {

	mgr, err := s.open(ctx, src.AccountID)
	if err != nil {
		return err
	}
	// 不要 Close：Manager 是池化对象，节流/熔断状态要跨请求复用

	var batch []store.File
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.st.UpsertFiles(ctx, src.AccountID, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	type frame struct{ cid, rel string }
	stack := []frame{{src.RemoteCID, ""}}

	// 被判定为「同一部作品的多余副本」的网盘文件 ID，扫描结束后统一删台账行。
	var localDropped []string

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		j.update(func(p *Progress) { p.Phase = "scan"; p.Dir = f.rel; p.Dirs++ })

		// 先把整个目录读完再决定谁写 .strm：去重必须看全目录，
		// 只看一页（默认 200 条）会把跨页的副本漏掉，结果就是「去重了但没完全去」。
		var videos []pan.Entry

		for offset := 0; ; {
			entries, total, err := mgr.ListPage(ctx, f.cid, offset, opt.PageSize)
			if err != nil {
				if errors.Is(err, pan.ErrCooling) || errors.Is(err, pan.ErrRisk) {
					// 风控不是「这个库的问题」，直接中止整轮，让用户等冷却后重跑
					return err
				}
				// 单个目录失败不该让整轮白跑：记下来，继续下一个目录
				s.log.Warn("列目录失败，跳过", "cid", f.cid, "rel", f.rel, "err", err)
				j.update(func(p *Progress) { p.Failed++ })
				break
			}

			for _, e := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				if e.IsDir {
					stack = append(stack, frame{e.ID, path.Join(f.rel, strm.Sanitize(e.Name))})
					continue
				}
				if !strm.IsVideo(e.Name) {
					continue
				}
				// 发布组塞的广告视频不生成 .strm：它们进了媒体库就是一张
				// 刮不出元数据的灰卡片，用户还得自己去删。
				if strm.IsAd(e.Name) {
					s.log.Info("跳过广告视频", "name", e.Name, "dir", f.rel)
					j.update(func(p *Progress) { p.Skipped++ })
					continue
				}
				if opt.MinSize > 0 && e.Size > 0 && e.Size < opt.MinSize {
					continue
				}
				if e.PickCode == "" {
					s.log.Warn("视频没有 pickcode/fid，跳过", "name", e.Name, "source", src.Label())
					j.update(func(p *Progress) { p.Failed++ })
					continue
				}
				videos = append(videos, e)
			}

			offset += len(entries)
			if len(entries) == 0 || len(entries) < opt.PageSize || (total >= 0 && offset >= total) {
				break
			}
		}

		keep := videos
		if opt.Dedupe && len(videos) > 1 {
			var dup []pan.Entry
			keep, dup = planDir(f.rel, videos)
			for _, e := range dup {
				// 上一轮已经写出去的 .strm 要收回，否则 Emby 那边还留着旧条目
				if oldRel, ok := existing[e.ID]; ok && oldRel != "" {
					if err := os.Remove(filepath.Join(lib.LocalPath, filepath.FromSlash(oldRel))); err == nil {
						j.update(func(p *Progress) { p.Removed++ })
					}
				}
				localDropped = append(localDropped, e.ID)
				j.update(func(p *Progress) { p.Skipped++ })
			}
			if len(dup) > 0 {
				s.log.Info("同目录重复视频，只保留一条 .strm",
					"dir", f.rel, "共", len(videos), "保留", len(keep), "删除", len(dup))
			}
		}

		for _, e := range keep {
			if err := ctx.Err(); err != nil {
				return err
			}
			seen[e.ID] = struct{}{}

			target := strm.Target(lib.LocalPath, f.rel, e.Name)
			// 同名不同文件：加 pickcode 后缀保证唯一，并把原因写进日志。
			if prev, ok := claims[target]; ok && prev.pickCode != e.PickCode {
				switch {
				case prev.account == src.AccountID:
					// 同一个来源里两个文件挤到同一个路径（比如 mkv/mp4 同名），
					// 只能靠后缀共存——这是老行为，别改。
					base := strings.TrimSuffix(target, ".strm")
					target = base + " [" + shortID(e.PickCode) + "].strm"
					s.log.Warn("同名文件冲突，已加后缀", "target", target)
				case e.Size > prev.size:
					// 另一个网盘有同名但更大的版本：撤掉小的那条，改指向这条。
					_ = os.Remove(target)
					*dropped = append(*dropped, prev.fileID)
					j.update(func(p *Progress) { p.Removed++ })
					s.log.Info("两个网盘都有这个文件，改留大的",
						"target", target, "旧", prev.size, "新", e.Size)
				default:
					// 已有那份更大或一样大：这一条不写，台账里也不留
					j.update(func(p *Progress) { p.Skipped++ })
					continue
				}
			}
			claims[target] = claim{pickCode: e.PickCode, fileID: e.ID, account: src.AccountID, size: e.Size}

			content := strm.Content(opt.Domain, strm.PlayIDForKind(src.Kind, e.PickCode), opt.KeepExt, e.Name)
			// 台账里的 rel_path 统一用 "/"：Windows 下 filepath.Rel 会返回反斜杠，
			// 不统一的话「改名判定」每次都会误判成改名。
			relNative, _ := filepath.Rel(lib.LocalPath, target)
			relPath := filepath.ToSlash(relNative)

			changed := true
			// Force 时跳过「内容没变就不写」的判断：全量的价值就在于让每个
			// 文件的 mtime 都刷新一次，好让 Emby 重扫整库。
			if !opt.Force {
				if old, err := strm.Read(target); err == nil && old == content {
					changed = false
				}
			}
			if changed {
				if err := strm.Write(target, content); err != nil {
					s.log.Warn("写 .strm 失败", "target", target, "err", err)
					j.update(func(p *Progress) { p.Failed++ })
					continue
				}
				j.update(func(p *Progress) { p.Written++ })
			}

			// 网盘里改名/移动过：旧 .strm 必须删掉，否则 Emby 会看到两个条目
			if oldRel, ok := existing[e.ID]; ok && oldRel != "" && oldRel != relPath {
				oldTarget := filepath.Join(lib.LocalPath, filepath.FromSlash(oldRel))
				if oldTarget != target {
					if err := os.Remove(oldTarget); err == nil {
						j.update(func(p *Progress) { p.Removed++ })
					}
				}
			}

			batch = append(batch, store.File{
				LibraryID: lib.ID,
				AccountID: src.AccountID,
				FileID:    e.ID,
				PickCode:  e.PickCode,
				RelPath:   relPath,
				Name:      e.Name,
				Size:      e.Size,
			})
			j.update(func(p *Progress) { p.Files++ })
		}

		if err := flush(); err != nil {
			return err
		}
	}

	if err := flush(); err != nil {
		return err
	}
	// 本来源内部的重复副本：台账行要清，但不要混进跨来源的 dropped
	// （那份是「另一个网盘的文件」，清理顺序不同）。
	if len(localDropped) > 0 {
		if err := s.st.DeleteFilesByFileIDs(ctx, lib.ID, localDropped); err != nil {
			return err
		}
	}
	return nil
}

// mirrorWarning 检查 lib.LocalPath 是否落在 Emby 那个库的扫描路径之内。
//
// 只可能「本地比库更细」（库扫 /media，本地镜像 /media/movies），
// 反过来就一定看不到：库扫 /media/movies/电影，而 .strm 写到了
// /media/movies/空枪 (2026)。这正是整理完「Emby 没动静」的根源。
//
// 拿不到库列表时返回空：这是诊断，不该让一轮成功的同步变成失败。
func (s *Service) mirrorWarning(ctx context.Context, srv mediaserver.Server, lib store.Library) string {
	libs, err := srv.Libraries(ctx)
	if err != nil {
		return ""
	}
	var target *mediaserver.Library
	for i := range libs {
		if libs[i].ID == lib.ServerLibID {
			target = &libs[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("同步完成，但媒体服务器上找不到库 #%s：Emby 里不会出现这些条目", lib.ServerLibID)
	}
	if len(target.Paths) == 0 {
		return ""
	}
	if underAnyRoot(target.Paths, lib.LocalPath) {
		return ""
	}
	return fmt.Sprintf("同步完成，但「%s」库扫的是 %s，而 .strm 写在 %s —— Emby 看不到这些文件",
		target.Name, strings.Join(target.Paths, "、"), lib.LocalPath)
}

// underAnyRoot 判断本地镜像目录是否被某个扫描根覆盖。
//
// 允许「本地比根更细」（根 /media、镜像 /media/movies）：Emby 递归扫，
// 子目录照样能看见。反过来一定看不到，这才是要拦的情况。
func underAnyRoot(roots []string, local string) bool {
	local = filepath.Clean(local)
	for _, p := range roots {
		root := filepath.Clean(p)
		if local == root {
			return true
		}
		if rel, err := filepath.Rel(root, local); err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// planDir 把一个目录里的视频分成「要写 .strm 的」和「多余副本」。
//
// 归并的单位是「同一部作品」，不是「同一个目录」——这个区别很要紧：
//   - 有季集号的按 S/E 归并，Season 目录里 20 集各留一条；
//   - 没有季集号的按「片名+年份」归并，合集目录里的不同影片不会被误合并。
//
// 片名优先从文件名解析，解析不出才退回目录名：目录名在「2024合集」这种场景下
// 会把一堆不相干的影片洗成同一个名字，用它当归并键就会误删。
func planDir(rel string, files []pan.Entry) (keep, drop []pan.Entry) {
	groups := make(map[string][]pan.Entry, len(files))
	order := make([]string, 0, len(files))
	for _, e := range files {
		k := mediaKey(rel, e.Name)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	for _, k := range order {
		g := groups[k]
		if len(g) == 1 {
			keep = append(keep, g[0])
			continue
		}
		best := 0
		for i := 1; i < len(g); i++ {
			if betterCopy(g[i], g[best]) {
				best = i
			}
		}
		for i, e := range g {
			if i == best {
				keep = append(keep, e)
				continue
			}
			drop = append(drop, e)
		}
	}
	return keep, drop
}

// mediaKey 给「一部作品」算一个归并键。
func mediaKey(rel, name string) string {
	p := scrape.Parse(name)
	if p.Title == "" {
		// 文件名本身是通用词（movie.mkv）或纯数字，只能退回目录名
		p = scrape.Parse(path.Join(rel, name))
	}
	if p.HasEpisode && p.Episode > 0 {
		return "e|" + strconv.Itoa(p.Season) + "|" + strconv.Itoa(p.Episode)
	}
	if t := normalizeName(p.Title); t != "" {
		return "t|" + t + "|" + p.Year
	}
	// 什么都认不出来：只把「完全同名」的当作副本，宁可不去重也不要误删
	return "n|" + strings.ToLower(name)
}

// betterCopy 判断 a 是否比 b 更值得保留：先看体积，再看容器格式，最后按名字定序。
//
// 体积最大的通常是画质最高的那个版本；体积相同（极少见）时优先 mkv，
// 因为 115 的 mp4 常被转码过，码率反而更低。
func betterCopy(a, b pan.Entry) bool {
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	ea, eb := extRank(a.Name), extRank(b.Name)
	if ea != eb {
		return ea < eb
	}
	return a.Name < b.Name // 保证同样的输入得到同样的结果
}

func extRank(name string) int {
	switch strm.Ext(name) {
	case ".mkv":
		return 0
	case ".mp4":
		return 1
	case ".ts", ".m2ts":
		return 2
	case ".avi", ".wmv", ".mov", ".flv":
		return 3
	}
	return 4
}

// normalizeName 用于比较：小写，只留字母、数字和汉字。
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func shortID(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:6]
}
