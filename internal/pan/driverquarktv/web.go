package driverquarktv

// 夸克网页接口：TV 版只给「列目录 + 取直链」，移动 / 改名 / 删除 / 建目录
// 都在网页那套接口里，要带网页 Cookie（`__pus` / `__puus` 那一串）。
//
// 两条通道的分工保持不变：
//   - 播放、同步列目录走 TV 接口（不挑会员、直链能 302）；
//   - 自动整理这类「改网盘内容」的动作用网页接口（要 Cookie，会走风控）。
//
// 为什么不做成「全走网页接口」：网页接口的下载直链基本都要求会员，
// 而且对 UA、Cookie、频率都敏感；TV 接口正好相反。两套各干各擅长的事。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"strm115/internal/pan"
)

// webBase 是网页/PC 接口根。声明成变量是为了单测能指到假服务器。
var webBase = "https://drive-pc.quark.cn/1/clouddrive"

// webUA 必须是桌面浏览器：网页接口按 UA 判客户端，用 TV 的安卓 UA 会被
// 当成异常设备，轻则要求验证码，重则整号风控。
const webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// webMaxTaskPolls 是一轮任务最多轮询几次。
//
// 夸克移动/删除是异步任务，正常 1～3 秒完成。20 次 × 1.2 秒 ≈ 24 秒，
// 超过还没结束就当作失败 —— 让整理台账留下记录，比无限等下去好。
const webMaxTaskPolls = 20

type webEnv struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Status  int             `json:"status"`
	Data    json.RawMessage `json:"data"`
}

// ensureWebCookie 在动网盘内容之前确认一下有没有网页 Cookie。
//
// 只扫码登录过 TV 端的账号没有 Cookie，此时能同步、能播放，但一移动就
// 报「未登录」。提前抛一个明确的错误，用户才知道该去重新添加账号。
func (c *Client) ensureWebCookie(op string) error {
	if strings.TrimSpace(c.cred.WebCookie) == "" {
		return pan.NewErr(pan.ErrAuth, op,
			"这个夸克账号没有网页 Cookie，无法移动/改名/删除；请到「账号与媒体库」用 Cookie 方式重新添加", nil)
	}
	return nil
}

// webCall 发一次网页接口请求。body 为 nil 表示 GET。
func (c *Client) webCall(ctx context.Context, op, method, pathname string, body any, out any) error {
	if err := c.ensureWebCookie(op); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("pr", "ucpro")
	q.Set("fr", "pc")

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return pan.NewErr(pan.ErrTransient, op, "构造请求失败", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, webBase+pathname+"?"+q.Encode(), rdr)
	if err != nil {
		return pan.NewErr(pan.ErrTransient, op, "构造请求失败", err)
	}
	req.Header.Set("Cookie", c.cred.WebCookie)
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://pan.quark.cn/")
	req.Header.Set("Origin", "https://pan.quark.cn")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	raw, err := c.send(req)
	if err != nil {
		return err
	}
	var e webEnv
	if err := json.Unmarshal(raw, &e); err != nil {
		return pan.NewErr(pan.ErrTransient, op, "夸克返回的不是 JSON（可能被网关拦了）", err)
	}
	// 网页接口的错误码说明：0 成功；31001/31002 是登录态失效；
	// 其余（如 41004 无权限、23018 空间不足）按「被拒不重试」处理。
	if e.Code != 0 {
		switch e.Code {
		case 31001, 31002:
			return pan.NewErr(pan.ErrAuth, op,
				"夸克网页 Cookie 已失效，请到「账号与媒体库」重新添加： "+e.Message, nil)
		}
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = fmt.Sprintf("夸克返回 code=%d", e.Code)
		}
		switch {
		case strings.Contains(msg, "频繁"), strings.Contains(msg, "限流"), strings.Contains(msg, "稍后"):
			return pan.NewErr(pan.ErrRisk, op, msg, nil)
		case strings.Contains(msg, "不存在"), strings.Contains(msg, "已删除"):
			return pan.NewErr(pan.ErrNotFound, op, msg, nil)
		default:
			return pan.NewErr(pan.ErrRejected, op, msg, nil)
		}
	}
	if out != nil && len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, out); err != nil {
			return pan.NewErr(pan.ErrTransient, op, "解析夸克返回失败", err)
		}
	}
	return nil
}

// webWaitTask 等一个异步任务结束。
//
// 和 TV 侧那套一样只看 status：夸克完成时 task_status 可能是 null，
// 要求两者都为 2 会让每次成功都干等到超时。
func (c *Client) webWaitTask(ctx context.Context, op, taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return nil
	}
	for i := 0; i < webMaxTaskPolls; i++ {
		var out struct {
			Status     int    `json:"status"`
			TaskStatus int    `json:"task_status"`
			Message    string `json:"message"`
		}
		q := url.Values{}
		q.Set("pr", "ucpro")
		q.Set("fr", "pc")
		q.Set("task_id", taskID)
		q.Set("retry_index", strconv.Itoa(i))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, webBase+"/task?"+q.Encode(), nil)
		if err != nil {
			return pan.NewErr(pan.ErrTransient, op, "构造请求失败", err)
		}
		req.Header.Set("Cookie", c.cred.WebCookie)
		req.Header.Set("User-Agent", webUA)
		req.Header.Set("Referer", "https://pan.quark.cn/")
		raw, err := c.send(req)
		if err != nil {
			return err
		}
		var e webEnv
		if err := json.Unmarshal(raw, &e); err != nil {
			return pan.NewErr(pan.ErrTransient, op, "解析任务状态失败", err)
		}
		if e.Code != 0 {
			return pan.NewErr(pan.ErrRejected, op, e.Message, nil)
		}
		if len(e.Data) > 0 {
			if err := json.Unmarshal(e.Data, &out); err != nil {
				return pan.NewErr(pan.ErrTransient, op, "解析任务状态失败", err)
			}
		}
		if out.Status == 2 {
			if msg := strings.TrimSpace(out.Message); msg != "" && !strings.EqualFold(msg, "ok") {
				return pan.NewErr(pan.ErrRejected, op, msg, nil)
			}
			return nil
		}
		if out.Status < 0 {
			return pan.NewErr(pan.ErrRejected, op, "夸克任务失败（status="+strconv.Itoa(out.Status)+"）", nil)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
	return pan.NewErr(pan.ErrTransient, op, "夸克任务一直没有完成（已等约 24 秒）", nil)
}

// Mkdir 建目录。网页接口是同步返回 fid 的。
func (c *Client) Mkdir(ctx context.Context, parentID, name string) (string, error) {
	if strings.TrimSpace(parentID) == "" {
		parentID = "0"
	}
	var out struct {
		Fid string `json:"fid"`
	}
	body := map[string]any{
		"pdir_fid":      parentID,
		"file_name":     name,
		"dir_path":      "",
		"dir_init_lock": false,
	}
	if err := c.webCall(ctx, "建目录", http.MethodPost, "/file", body, &out); err != nil {
		return "", err
	}
	return out.Fid, nil
}

// Move 把文件/目录移到 toDirID。
func (c *Client) Move(ctx context.Context, toDirID string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	body := map[string]any{
		"filelist":     ids,
		"to_pdir_fid":  toDirID,
		"action_type":  1,
		"exclude_fids": []string{},
	}
	if err := c.webCall(ctx, "移动", http.MethodPost, "/file/move", body, &out); err != nil {
		return err
	}
	return c.webWaitTask(ctx, "移动", out.TaskID)
}

// Rename 改名。
func (c *Client) Rename(ctx context.Context, id, name string) error {
	var out struct {
		TaskID string `json:"task_id"`
	}
	body := map[string]any{"fid": id, "file_name": name}
	if err := c.webCall(ctx, "改名", http.MethodPost, "/file/rename", body, &out); err != nil {
		return err
	}
	return c.webWaitTask(ctx, "改名", out.TaskID)
}

// Remove 删除（进回收站）。整理按来源清理失效条目时会用到。
func (c *Client) Remove(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	body := map[string]any{
		"filelist":     ids,
		"action_type":  2,
		"exclude_fids": []string{},
	}
	if err := c.webCall(ctx, "删除", http.MethodPost, "/file/delete", body, &out); err != nil {
		return err
	}
	return c.webWaitTask(ctx, "删除", out.TaskID)
}
