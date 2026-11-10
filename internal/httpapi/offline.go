package httpapi

import (
	"context"
	"fmt"
	"time"

	"strm115/internal/notify"
	"strm115/internal/offline"
	"strm115/internal/pan"
	"strm115/internal/store"
)

// onOfflineDone 是离线任务到终态时的回调：发通知，必要时顺手触发一次同步。
//
// 「顺手同步」是这套组合最有用的一环：链接下完就落在媒体库对应的网盘目录里，
// 不自动同步的话用户还得回媒体库页手动点一次，而那正是他最容易忘的一步。
func (s *Server) onOfflineDone(ev offline.Event) {
	ctx := context.Background()
	name := ev.Task.Name
	if name == "" {
		name = ev.Label
	}
	if name == "" {
		name = ev.Task.Hash
	}

	switch ev.Task.Status {
	case pan.OfflineDone:
		body := name
		if ev.Task.Size > 0 {
			body += " · " + humanSize(ev.Task.Size)
		}
		// 三种落地位置，三种下半场：
		//  1. 下到媒体库目录里 → 直接同步（同步完会自动刮削）
		//  2. 下到资源保存目录（TG / 订阅的默认落脚点）→ 先整理进媒体库，
		//     整理完自己会触发同步和刮削
		//  3. 下到别的地方 → 只能提醒用户
		if lib, ok := s.libraryForDir(ctx, ev.AccountID, ev.DirID); ok {
			if s.settingBool(ctx, settingOfflineAutoSync, true) {
				if job, err := s.beginSync(lib); err != nil {
					s.log.Warn("离线完成后自动同步失败", "library", lib.Name, "err", err)
					body += "\n同步没起来：" + err.Error()
				} else {
					s.watchSyncAndNotify(job, lib)
					body += "\n已同步「" + lib.Name + "」"
				}
			}
		} else if s.organizerIngestOn(ctx) {
			body += "\n接着自动整理进媒体库 → 同步 → 刮削"
			s.kickOrganizeIngest(ev.AccountID, 5*time.Second, "离线完成")
		} else if s.settingBool(ctx, settingOfflineAutoSync, true) {
			body += "\n这个目录没有对应媒体库，没生成 STRM"
		}
		s.pushEvent("offline_ok", notify.KindIngest, "✓ 离线下载完成", body)

	case pan.OfflineFailed:
		s.pushEvent("offline_fail", notify.KindSystem, "✕ 离线下载失败",
			name+"\n链接失效或没有可用节点")
	}
}

// libraryForDir 找出「这个账号的这个网盘目录」对应的媒体库。
func (s *Server) libraryForDir(ctx context.Context, accountID int64, dirID string) (store.Library, bool) {
	// 根目录（"0"）不匹配任何库：下到根目录的任务无从判断该同步哪个库。
	if dirID == "" || dirID == "0" {
		return store.Library{}, false
	}
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		return store.Library{}, false
	}
	for _, l := range libs {
		if l.AccountID == accountID && l.RemoteCID == dirID {
			return l, true
		}
		// 一个库挂了两家网盘：夸克那半边不该因为「库的主账号是 115」而匹配不上，
		// 否则夸克离线任务下完不会触发同步，用户只能自己点。
		for _, src := range l.Sources {
			if src.AccountID == accountID && src.RemoteCID == dirID {
				return l, true
			}
		}
	}
	return store.Library{}, false
}

// humanSize 给通知正文用的体积格式化。
func humanSize(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
