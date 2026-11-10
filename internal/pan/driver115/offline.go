package driver115

// 离线下载（115 网页端通道）。
//
// 踩过的坑，别改回去：
//
//	115 老的 lixianssp/?ac=add_task_urls 是 RSA 加密通道（把 url[0]、url[1]…
//	塞进一段加密 JSON 里）。这条通道已经失效，现在恒定返回纯文本 "decode fail!"，
//	上游 SDK 的 AddOfflineTaskURIs 因此只会吐 invalid character 'd' 这种误导性
//	JSON 错误。网页端早已换成 sign/time 签名方案：
//
//		GET  https://115.com/?ct=offline&ac=space
//		     → {"state":true,"sign":"...","time":1794047939,"size":"21.83TB"}
//		POST https://115.com/web/lixian/?ct=lixian&ac=add_task_url
//		     url / savepath / wp_path_id / uid / sign / time
//		     → {"state":true,"info_hash":"def03a36…","url":"magnet:…"}
//
// 新接口一次只收一条链接（回包是单个 info_hash，不是数组），所以批量提交要在
// 这里逐条发。sign 有时效，网页端每次现取，这里缓存 5 分钟省掉一半请求。
//
// 115 的原生离线吃 magnet / ed2k / 种子直链。提交成功后它回的是 info_hash，
// 任务列表也按 hash 索引，所以上层拿 hash 当任务标识最稳。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"strm115/internal/pan"
)

const (
	// sign 的有效期没有公开，网页端每次都现取，保守按 5 分钟缓存。
	offlineSignTTL = 5 * time.Minute
)

// 端点抽成变量只为了测试能换成 httptest 服务器：这条链路（取签名 → 逐条提交）
// 的真正坑在参数拼装和回包解析上，用假服务器跑一遍比往用户网盘里塞测试任务靠谱。
var (
	offlineSpaceURL = "https://115.com/?ct=offline&ac=space"
	offlineAddURL   = "https://115.com/web/lixian/?ct=lixian&ac=add_task_url"
)

// offlineAuth 缓存一次 sign/time，避免每条链接都去问一次。
type offlineAuth struct {
	sign string
	ts   int64
	at   time.Time
}

// offlineSpaceResp 是 ac=space 的回包，只取用得到的字段。
type offlineSpaceResp struct {
	State bool   `json:"state"`
	Sign  string `json:"sign"`
	Time  int64  `json:"time"`
	Size  string `json:"size"`
}

// offlineAddResp 是 add_task_url 的回包。失败时 115 会把原因放 error_msg。
type offlineAddResp struct {
	State    bool   `json:"state"`
	Errno    int    `json:"errno"`
	Errcode  int    `json:"errcode"`
	ErrorMsg string `json:"error_msg"`
	Error    string `json:"error"`
	InfoHash string `json:"info_hash"`
	URL      string `json:"url"`
}

// AddOfflineURIs 提交一批链接，返回成功提交的任务 hash。
//
// 逐条提交：中途被拒时把已经成功的 hash 一并返回，让上层知道哪些真的进了队列，
// 而不是重试整批、把成功的那些变成重复任务。
func (c *Client) AddOfflineURIs(ctx context.Context, uris []string, saveDirID string) ([]string, error) {
	clean := dedupURIs(uris)
	if len(clean) == 0 {
		return nil, pan.NewErr(pan.ErrRejected, "离线下载", "没有可提交的链接", nil)
	}
	if saveDirID == "" {
		saveDirID = "0" // 根目录
	}

	uid, err := c.offlineUID()
	if err != nil {
		return nil, err
	}

	hashes := make([]string, 0, len(clean))
	for i, uri := range clean {
		sign, ts, err := c.offlineSign()
		if err != nil {
			if i > 0 {
				return hashes, err
			}
			return nil, err
		}
		h, err := c.addOneOffline(uri, saveDirID, uid, sign, ts)
		if err != nil {
			if i > 0 {
				// 已经提交成功的部分照样交给上层，别让它们变成"未知状态"
				return hashes, err
			}
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}

// dedupURIs 去空白、去重并保序。同一条磁力链提交两次在 115 侧会变成重复任务，
// 清理在本地做掉比让 115 报错体面得多。
func dedupURIs(uris []string) []string {
	out := make([]string, 0, len(uris))
	seen := make(map[string]struct{}, len(uris))
	for _, u := range uris {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// addOneOffline 提交单条链接。
func (c *Client) addOneOffline(uri, dirID string, uid int64, sign string, ts int64) (string, error) {
	resp, err := c.c.NewRequest().
		SetFormData(map[string]string{
			"url":        uri,
			"savepath":   "",
			"wp_path_id": dirID,
			"uid":        strconv.FormatInt(uid, 10),
			"sign":       sign,
			"time":       strconv.FormatInt(ts, 10),
		}).
		ForceContentType("application/x-www-form-urlencoded;charset=UTF-8").
		Post(offlineAddURL)
	if err != nil {
		return "", classify("离线下载", err)
	}

	var out offlineAddResp
	if err := json.Unmarshal(resp.Body(), &out); err != nil {
		// 115 在请求异常时会回纯文本（历史上就是那句 decode fail!）。
		// 这种响应没有结构可言，只能把原文带出去给人看。
		return "", pan.NewErr(pan.ErrTransient, "离线下载",
			"115 返回了非 JSON 响应: "+clipText(resp.String(), 80), err)
	}
	if out.State {
		if out.InfoHash == "" {
			return "", pan.NewErr(pan.ErrRejected, "离线下载", "115 接受了请求但没给 info_hash", nil)
		}
		return out.InfoHash, nil
	}

	// state=false：登录失效时要把签名的缓存一起作废，否则下一条还会拿着废 sign 去撞
	if errno := findErrno(out.Errno, out.Errcode); errno == errnoNotLogin {
		c.resetOfflineAuth()
		return "", pan.NewErr(pan.ErrAuth, "离线下载", "115 登录已失效，请重新更新 Cookie", nil)
	}
	return "", offlineReject(out)
}

// offlineReject 把 115 的业务拒绝翻成有信息量的错误。
func offlineReject(out offlineAddResp) error {
	msg := firstNonEmpty(out.ErrorMsg, out.Error)
	if msg == "" {
		msg = "115 拒绝了该任务"
	}
	switch errno := findErrno(out.Errno, out.Errcode); errno {
	case errnoRiskVerify:
		return pan.NewErr(pan.ErrRisk, "离线下载", "115 要求安全验证，请先在网页端完成一次验证: "+msg, nil)
	case errnoBadURL:
		return pan.NewErr(pan.ErrRejected, "离线下载", "链接无效或不受支持: "+msg, nil)
	default:
		if errno != 0 {
			msg = fmt.Sprintf("%s（errno %d）", msg, errno)
		}
		return pan.NewErr(pan.ErrRejected, "离线下载", msg, nil)
	}
}

// 115 的几个关键业务码。
const (
	errnoNotLogin   = 99    // 未登录 / 登录态失效
	errnoRiskVerify = 911   // 需要安全验证（风控）
	errnoBadURL     = 10008 // 链接无效
)

func findErrno(codes ...int) int {
	for _, c := range codes {
		if c != 0 {
			return c
		}
	}
	return 0
}

// offlineSign 取（并缓存）本次提交要用的 sign/time。
func (c *Client) offlineSign() (string, int64, error) {
	c.mu.Lock()
	a := c.auth
	c.mu.Unlock()
	if a.sign != "" && time.Since(a.at) < offlineSignTTL {
		return a.sign, a.ts, nil
	}

	resp, err := c.c.NewRequest().Get(offlineSpaceURL)
	if err != nil {
		return "", 0, classify("获取离线签名", err)
	}
	var out offlineSpaceResp
	if err := json.Unmarshal(resp.Body(), &out); err != nil {
		return "", 0, pan.NewErr(pan.ErrTransient, "获取离线签名",
			"115 返回了非 JSON 响应: "+clipText(resp.String(), 80), err)
	}
	if !out.State || out.Sign == "" {
		return "", 0, pan.NewErr(pan.ErrAuth, "获取离线签名", "115 登录已失效，请重新更新 Cookie", nil)
	}

	c.mu.Lock()
	c.auth = offlineAuth{sign: out.Sign, ts: out.Time, at: time.Now()}
	c.mu.Unlock()
	return out.Sign, out.Time, nil
}

func (c *Client) resetOfflineAuth() {
	c.mu.Lock()
	c.auth = offlineAuth{}
	c.mu.Unlock()
}

// offlineUID 拿提交时要带的 uid（115 要求与登录态一致）。
func (c *Client) offlineUID() (int64, error) {
	c.mu.Lock()
	uid := c.uid
	c.mu.Unlock()
	if uid > 0 {
		return uid, nil
	}
	u, err := c.c.GetUser()
	if err != nil {
		return 0, classify("读取账号信息", err)
	}
	if u == nil || u.UserID <= 0 {
		return 0, pan.NewErr(pan.ErrAuth, "读取账号信息", "115 没有返回用户 ID", nil)
	}
	c.mu.Lock()
	c.uid = u.UserID
	c.mu.Unlock()
	return u.UserID, nil
}

// ListOfflineTasks 列一页离线任务。
//
// 列表接口还是老路径（lixian.115.com/lixian/?ct=lixian&ac=task_lists），它没跟着
// 提交接口一起改，SDK 的 ListOfflineTask 实测可用。
func (c *Client) ListOfflineTasks(ctx context.Context, page int64) ([]pan.OfflineTask, int64, error) {
	if page <= 0 {
		page = 1
	}
	r, err := c.c.ListOfflineTask(page)
	if err != nil {
		return nil, 0, classify("列离线任务", err)
	}
	out := make([]pan.OfflineTask, 0, len(r.Tasks))
	for _, t := range r.Tasks {
		if t == nil {
			continue
		}
		out = append(out, pan.OfflineTask{
			Hash:   t.InfoHash,
			Name:   t.Name,
			Size:   t.Size,
			Status: offlineStatus(t.Status),
			// 实测 115 的 percentDone 已经是百分数（95.69 表示 95.69%），
			// 再乘 100 会出现 9569% 这种数。
			Percent: clampPercent(t.Percent),
			FileID:  firstNonEmpty(t.FileId, t.DelFileId),
			DirID:   t.DirId,
			AddTime: unixTime(t.AddTime),
		})
	}
	return out, r.Total, nil
}

// clampPercent 把进度夹到 0~100。上游偶尔会给出越界值，显示 100% 也比 4000% 体面。
func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// offlineStatus 把 115 的数字状态码翻成中立状态。
func offlineStatus(code int) pan.OfflineStatus {
	switch code {
	case 2:
		return pan.OfflineDone
	case 1:
		return pan.OfflineRunning
	case -1:
		return pan.OfflineFailed
	default:
		return pan.OfflineWaiting
	}
}

func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// clipText 截断超长响应体，日志和错误信息里不至于刷屏。
func clipText(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
