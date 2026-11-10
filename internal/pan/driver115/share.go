package driver115

// 115 分享转存。
//
// 两条请求，都在 115cdn 域名下（和分享页同一套）：
//
//	GET  /webapi/share/snap?share_code=xx&receive_code=xx&cid=0&limit=100&format=json
//	     → shareinfo（标题、封禁原因、过期时间）+ list（顶层文件，fid / 文件名）
//	POST /webapi/share/receive
//	     share_code / receive_code / file_id（多个 fid 逗号分隔）/ cid（目标目录）/ is_check=0
//	     → {"state":true} 或 {"state":false,"errno":4100024,"error":"已经转存过"}
//
// 为什么不直接用 SDK 的 GetShareSnap：那条链路里 URL 是常量，测试没法换成假服务器，
// 而这条链路值得测（提取码错、分享失效、已转存、风控都要翻成人话）。所以两个端点
// 都做成包级变量，跟 offline.go 一个套路。
//
// 先 snap 后 receive 的顺序也有意义：snap 能拿到「分享标题 + 文件数」，转存失败时
// 报错更具体；而且分享已封禁 / 已过期在 snap 阶段就能拦下来，不必去撞 receive。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"strm115/internal/pan"

	driver "github.com/SheltonZhu/115driver/pkg/driver"
)

var (
	shareSnapURL    = "https://115cdn.com/webapi/share/snap"
	shareReceiveURL = "https://115cdn.com/webapi/share/receive"
)

// 115 分享相关的业务码。
const (
	errnoShareRepeated = 4100024 // 之前已经转存过（不算失败）
	errnoShareInvalid  = 4100009 // 分享失效 / 提取码不对
	errnoShareNotFound = 4100026 // 分享不存在
	errnoNotLogin2     = 990001  // 未登录（app 侧的业务码）
)

type shareSnapResp struct {
	State  bool   `json:"state"`
	Errno  int    `json:"errno"`
	Error  string `json:"error"`
	ErrMsg string `json:"error_msg"`
	Data   struct {
		Shareinfo struct {
			SnapID       string           `json:"snap_id"`
			ShareTitle   string           `json:"share_title"`
			ForbidReason string           `json:"forbid_reason"`
			ExpireTime   int64            `json:"expire_time"`
			ShareState   driver.StringInt `json:"share_state"`
			ReceiveCode  string           `json:"receive_code"`
		} `json:"shareinfo"`
		Count int `json:"count"`
		List  []struct {
			// 分享快照里的顶层条目：新版接口只给 cid（分享内的条目 id），
			// 老版给 fid。两个都收，转存时哪个有就用哪个。
			FileID   flexString `json:"fid"`
			CID      flexString `json:"cid"`
			FileName string     `json:"n"`
			IsDir    int        `json:"fc"`
		} `json:"list"`
	} `json:"data"`
}

type shareReceiveResp struct {
	State  bool   `json:"state"`
	Errno  int    `json:"errno"`
	Error  string `json:"error"`
	ErrMsg string `json:"error_msg"`
}

// flexString 兼容「数字写成数字、字符串写成字符串」的字段。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(string(b))
	return nil
}

// SaveShare 把一条 115 分享转存到 toDirID。
func (c *Client) SaveShare(ctx context.Context, shareCode, password, toDirID string) (pan.ShareSaveResult, error) {
	shareCode = strings.TrimSpace(shareCode)
	password = strings.TrimSpace(password)
	if shareCode == "" {
		return pan.ShareSaveResult{}, pan.NewErr(pan.ErrRejected, "转存分享", "没有识别到 115 分享码", nil)
	}
	if toDirID == "" {
		toDirID = "0" // 根目录
	}

	snap, err := c.shareSnap(ctx, shareCode, password)
	if err != nil {
		return pan.ShareSaveResult{}, err
	}

	// 顶层条目的 id：新版快照只给 cid，老版给 fid。
	// 一个都拿不到时不要硬失败 —— receive 不带 file_id 就是「转存全部」，
	// 这正是网页端点「保存到网盘」的行为。
	ids := make([]string, 0, len(snap.Data.List))
	for _, f := range snap.Data.List {
		if id := firstNonEmpty(string(f.FileID), string(f.CID)); id != "" {
			ids = append(ids, id)
		}
	}
	files := len(ids)
	if files == 0 {
		files = snap.Data.Count
	}
	if files == 0 {
		return pan.ShareSaveResult{}, pan.NewErr(pan.ErrNotFound, "转存分享",
			"分享里没有可转存的文件（可能只剩空目录，或已被作者删除）", nil)
	}

	already, err := c.shareReceive(ctx, shareCode, password, toDirID, ids)
	if err != nil {
		return pan.ShareSaveResult{}, err
	}
	return pan.ShareSaveResult{
		Title:   strings.TrimSpace(snap.Data.Shareinfo.ShareTitle),
		Files:   files,
		Already: already,
	}, nil
}

// shareSnap 读一次分享快照：标题、状态、顶层文件。
func (c *Client) shareSnap(ctx context.Context, code, pwd string) (*shareSnapResp, error) {
	req := c.c.NewRequest().
		SetContext(ctx).
		SetQueryParams(map[string]string{
			"share_code":   code,
			"receive_code": pwd,
			"cid":          "0",
			"limit":        "100",
			"offset":       "0",
			"asc":          "0",
			"format":       "json",
		}).
		SetHeader("referer", driver.BuildShareReferer(code, pwd)).
		ForceContentType("application/json;charset=UTF-8")

	resp, err := req.Get(shareSnapURL)
	if err != nil {
		return nil, classify("转存分享", err)
	}
	var out shareSnapResp
	if err := json.Unmarshal(resp.Body(), &out); err != nil {
		return nil, pan.NewErr(pan.ErrTransient, "转存分享",
			"115 返回了非 JSON 响应: "+clipText(resp.String(), 80), err)
	}
	if !out.State {
		return nil, shareReject("转存分享", out.Errno, firstNonEmpty(out.Error, out.ErrMsg))
	}
	if reason := strings.TrimSpace(out.Data.Shareinfo.ForbidReason); reason != "" {
		return nil, pan.NewErr(pan.ErrRejected, "转存分享", "分享已被封禁："+reason, nil)
	}
	if exp := out.Data.Shareinfo.ExpireTime; exp > 0 && exp < time.Now().Unix() {
		return nil, pan.NewErr(pan.ErrRejected, "转存分享", "分享已过期，让频道补一条新的", nil)
	}
	return &out, nil
}

// shareReceive 真正把文件收到自己的网盘。返回 already=true 表示之前就转存过。
func (c *Client) shareReceive(ctx context.Context, code, pwd, dirID string, ids []string) (bool, error) {
	form := map[string]string{
		"share_code":   code,
		"receive_code": pwd,
		"cid":          dirID,
		"is_check":     "0",
	}
	if len(ids) > 0 {
		form["file_id"] = strings.Join(ids, ",")
	}
	req := c.c.NewRequest().
		SetContext(ctx).
		SetFormData(form).
		SetHeader("referer", driver.BuildShareReferer(code, pwd)).
		ForceContentType("application/x-www-form-urlencoded;charset=UTF-8")

	resp, err := req.Post(shareReceiveURL)
	if err != nil {
		return false, classify("转存分享", err)
	}
	var out shareReceiveResp
	if err := json.Unmarshal(resp.Body(), &out); err != nil {
		return false, pan.NewErr(pan.ErrTransient, "转存分享",
			"115 返回了非 JSON 响应: "+clipText(resp.String(), 80), err)
	}
	if out.State {
		return false, nil
	}
	if out.Errno == errnoShareRepeated {
		return true, nil
	}
	return false, shareReject("转存分享", out.Errno, firstNonEmpty(out.Error, out.ErrMsg))
}

// shareReject 把 115 的业务拒绝翻成带处置建议的错误。
func shareReject(op string, errno int, msg string) error {
	if strings.TrimSpace(msg) == "" {
		msg = "115 拒绝了这次转存"
	}
	switch errno {
	case errnoNotLogin, errnoNotLogin2:
		return pan.NewErr(pan.ErrAuth, op, "115 登录已失效，请重新扫码或更新 Cookie", nil)
	case errnoShareInvalid:
		return pan.NewErr(pan.ErrRejected, op, "分享失效或提取码不对："+msg, nil)
	case errnoShareNotFound:
		return pan.NewErr(pan.ErrNotFound, op, "分享不存在："+msg, nil)
	case errnoRiskVerify:
		return pan.NewErr(pan.ErrRisk, op, "115 要求安全验证，请先在网页端完成一次："+msg, nil)
	default:
		if errno != 0 {
			msg = fmt.Sprintf("%s（errno %d）", msg, errno)
		}
		return pan.NewErr(pan.ErrRejected, op, msg, nil)
	}
}
