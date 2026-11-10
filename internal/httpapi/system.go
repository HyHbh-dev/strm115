package httpapi

// 首页的「系统资源 + 网盘容量」。
//
// 全部只读 /proc、cgroup 和各家网盘的轻量接口，不依赖第三方监控组件。
// 容器里 /proc/stat 反映的是宿主机 CPU，内存优先读 cgroup 限额（才是容器
// 真正能用的量），没有 cgroup 再退回 /proc/meminfo。
//
// 网盘容量要发外部请求，所以整份结果缓存 30 秒；首页刷新、多开标签都不会
// 把 115 或夸克打一遍。单个账号失败只影响那一行，不拖垮整张卡片。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"strm115/internal/quark"
)

const systemStatusTTL = 30 * time.Second

type accountQuota struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
	OK    bool   `json:"ok"`
	Note  string `json:"note,omitempty"`
}

type systemStatus struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemUsed    uint64  `json:"mem_used"`
	MemTotal   uint64  `json:"mem_total"`
	DiskTotal  uint64  `json:"disk_total"`
	DiskFree   uint64  `json:"disk_free"`
	// ProcessMem 是 Go 进程自己占的内存，和整机内存分开显示。
	ProcessMem   uint64         `json:"process_mem"`
	UptimeSecs   int64          `json:"uptime_secs"`
	IndexedBytes uint64         `json:"indexed_bytes"`
	Accounts     []accountQuota `json:"accounts"`
	// Available 是这些指标有没有读出来；Windows 本地跑没有 /proc 时前端显示「—」。
	Available bool      `json:"available"`
	At        time.Time `json:"at"`
}

func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	s.sysMu.Lock()
	if s.sysAt.After(time.Time{}) && time.Since(s.sysAt) < systemStatusTTL {
		out := s.sysCache
		s.sysMu.Unlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.sysMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out := s.collectSystem(ctx)

	s.sysMu.Lock()
	s.sysCache, s.sysAt = out, time.Now()
	s.sysMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) collectSystem(ctx context.Context) systemStatus {
	out := systemStatus{At: time.Now()}
	out.CPUPercent = cpuPercent()
	out.MemUsed, out.MemTotal = readMemory()
	out.DiskTotal, out.DiskFree = diskUsage(s.cfg.MediaRoot)
	out.Available = out.MemTotal > 0 || out.DiskTotal > 0 || out.CPUPercent > 0
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out.ProcessMem = ms.Sys
	out.UptimeSecs = int64(time.Since(s.start).Seconds())
	if b, err := s.st.TotalFileBytes(ctx); err == nil && b > 0 {
		out.IndexedBytes = uint64(b)
	}
	out.Accounts = s.accountQuotas(ctx)
	return out
}

func (s *Server) accountQuotas(ctx context.Context) []accountQuota {
	accts, err := s.st.ListAccounts(ctx)
	if err != nil {
		return []accountQuota{}
	}
	base := strings.TrimSpace(s.setting(ctx, "quark_web_base"))
	out := make([]accountQuota, 0, len(accts))
	for _, a := range accts {
		q := accountQuota{ID: a.ID, Kind: a.Kind, Name: a.Name}
		full, err := s.st.GetAccount(ctx, a.ID)
		if err != nil {
			q.Note = "读不到账号凭证"
			out = append(out, q)
			continue
		}
		switch {
		case strings.EqualFold(a.Kind, "115"):
			var cr map[string]string
			if json.Unmarshal([]byte(full.Credential), &cr) == nil {
				parts := make([]string, 0, len(cr))
				for k, v := range cr {
					if v != "" {
						parts = append(parts, k+"="+v)
					}
				}
				cookie := strings.Join(parts, "; ")
				used, total, err := quota115(ctx, cookie)
				q.Used, q.Total, q.OK = used, total, err == nil && total > 0
				if err != nil {
					q.Note = err.Error()
				}
			}
		case strings.EqualFold(a.Kind, quarkKind):
			var cr struct {
				WebCookie string `json:"web_cookie"`
			}
			_ = json.Unmarshal([]byte(full.Credential), &cr)
			if strings.TrimSpace(cr.WebCookie) == "" {
				q.Note = "还没配网页 Cookie：账号 → 夸克 → 转存设置，扫码即可（TV 登录不能代替）"
			} else {
				c := quark.NewWithBase(cr.WebCookie, base)
				used, total, err := c.Quota(ctx)
				q.Used, q.Total, q.OK = used, total, err == nil && total > 0
				if err != nil {
					q.Note = err.Error()
				} else if total == 0 {
					q.Note = "夸克这次没返回容量字段"
				}
			}
		default:
			q.Note = "这个网盘暂不提供容量"
		}
		out = append(out, q)
	}
	return out
}

// quota115 读 115 的 space_info。实测响应：
//
//	data.space_info.all_total.size / all_use.size（字节，可能是浮点数）
//
// webapi115Base 抽成变量是为了单测能指到假网关，别在测试里真打 115。
var webapi115Base = "https://webapi.115.com"

func quota115(ctx context.Context, cookie string) (uint64, uint64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		webapi115Base+"/files/index_info", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://115.com/")
	hc := &http.Client{Timeout: 5 * time.Second}
	res, err := hc.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("连不上 115: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, 0, err
	}
	var out struct {
		Data struct {
			SpaceInfo struct {
				AllTotal struct {
					Size float64 `json:"size"`
				} `json:"all_total"`
				AllUse struct {
					Size float64 `json:"size"`
				} `json:"all_use"`
			} `json:"space_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, 0, fmt.Errorf("115 返回的不是 JSON")
	}
	if out.Data.SpaceInfo.AllTotal.Size <= 0 {
		return 0, 0, fmt.Errorf("115 没给容量（Cookie 可能已失效）")
	}
	return uint64(out.Data.SpaceInfo.AllUse.Size), uint64(out.Data.SpaceInfo.AllTotal.Size), nil
}

// ==================== /proc 与 cgroup ====================

type cpuSample struct{ total, idle uint64 }

func snapshotCPU() (cpuSample, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("/proc/stat 格式不对")
	}
	var s cpuSample
	for i, f := range fields[1:] {
		n, _ := strconv.ParseUint(f, 10, 64)
		s.total += n
		// 第 4、5 列是 idle 与 iowait，都算「没在干活」。
		if i == 3 || i == 4 {
			s.idle += n
		}
	}
	return s, nil
}

// cpuPercent 两次采样算差值。200ms 足够平滑，又不会让接口明显变慢；
// 结果缓存 30 秒，所以这个等待大约每半分钟才有一次。
func cpuPercent() float64 {
	a, err := snapshotCPU()
	if err != nil {
		return 0
	}
	time.Sleep(200 * time.Millisecond)
	b, err := snapshotCPU()
	if err != nil {
		return 0
	}
	total, idle := b.total-a.total, b.idle-a.idle
	if total == 0 {
		return 0
	}
	p := (1 - float64(idle)/float64(total)) * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

func readUintFile(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readMemory 优先用 cgroup v2 的当前用量 / 限额（容器里才是真实上限），
// 宿主机或 Windows 没有这些文件，退回 /proc/meminfo。
func readMemory() (used, total uint64) {
	if cur, err := readUintFile("/sys/fs/cgroup/memory.current"); err == nil {
		if max, err := readUintFile("/sys/fs/cgroup/memory.max"); err == nil && max > 0 && max < 1<<60 {
			return cur, max
		}
		// 没限额（max 是 "max"）时继续用 /proc/meminfo 拿总量。
	}
	if cur, err := readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
		if max, err := readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil && max > 0 && max < 1<<60 {
			return cur, max
		}
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var kb, availKB uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			kb = n
		case "MemAvailable:":
			availKB = n
		}
	}
	if kb == 0 {
		return 0, 0
	}
	total = kb * 1024
	if availKB <= kb {
		used = total - availKB*1024
	}
	return used, total
}
