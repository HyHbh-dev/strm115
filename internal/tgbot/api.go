package tgbot

// 只包了 Bot API 里这个 bot 真要用的六个方法。
//
// 不用现成的 telegram-bot-api 库：这里要的是「长轮询 + 发消息 + 点按钮回执」，
// 加起来不到两百行；引一个库反而多一份依赖、多一处和项目风格不一致的错误处理。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"strm115/internal/netproxy"
)

const (
	// pollTimeout 是 getUpdates 的服务端等待秒数：Telegram 会一直挂着连接
	// 直到有新消息或超时，这是「实时」的来源，也省掉了自己定时轮询。
	pollTimeout = 25
	// httpTimeout 要比 pollTimeout 大：客户端先超时的话，长轮询永远拿不到结果。
	httpTimeout = pollTimeout*time.Second + 15*time.Second
)

type api struct {
	token string
	base  string
	hc    *http.Client
}

func newAPI(token, proxy string) *api {
	tr := &http.Transport{
		Proxy:                 netproxy.URL,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: httpTimeout,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &api{
		token: token,
		base:  "https://api.telegram.org/bot" + token,
		hc:    &http.Client{Transport: tr, Timeout: httpTimeout},
	}
}

// apiError 是 Telegram 返回的业务错误：HTTP 200 也可能 ok=false。
type apiError struct {
	Status      int
	Code        int
	Description string
}

func (e *apiError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("telegram %d: %s", e.Code, e.Description)
	}
	return fmt.Sprintf("telegram HTTP %d", e.Status)
}

// call 发一个 JSON 请求并把 result 解到 out（out 可为 nil）。
func (a *api) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}

	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// 网关/代理返回的 HTML 错误页：带上开头一段，否则排查时只剩「解析失败」
		return &apiError{Status: res.StatusCode, Description: "返回的不是 JSON: " + cut(string(raw), 120)}
	}
	if !env.OK {
		return &apiError{Status: res.StatusCode, Code: env.ErrorCode, Description: env.Description}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return err
		}
	}
	return nil
}

func (a *api) getMe(ctx context.Context) (User, error) {
	var u User
	err := a.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}

func (a *api) getUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	// allowed_updates 必须显式声明：不写的话 Telegram 会把所有类型都推过来，
	// 其中大部分（成员变动、编辑记录…）这个 bot 根本不处理。
	// callback_query 要点名要：搜索结果靠内联键盘选片，漏了它按钮点了没反应。
	var ups []Update
	err := a.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// send 发一条消息，返回 message_id（0 表示发失败或没返回）。
func (a *api) send(ctx context.Context, chatID int64, html string, kb InlineKeyboard) (int64, error) {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if len(kb) > 0 {
		payload["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	var m Message
	if err := a.call(ctx, "sendMessage", payload, &m); err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// edit 改写一条已发出的消息（列表 → 结果时用，避免聊天记录越滚越长）。
func (a *api) edit(ctx context.Context, chatID, msgID int64, html string, kb InlineKeyboard) error {
	payload := map[string]any{
		"chat_id":                  chatID,
		"message_id":               msgID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	// 空键盘也要显式给：不给的话 Telegram 会保留旧按钮，
	// 用户会看到一个「已经处理完但按钮还在」的消息，点第二次就重复入库了。
	if kb == nil {
		kb = InlineKeyboard{}
	}
	payload["reply_markup"] = map[string]any{"inline_keyboard": kb}
	return a.call(ctx, "editMessageText", payload, nil)
}

// answer 必须调：不调的话用户手机上的按钮会一直转圈。
func (a *api) answer(ctx context.Context, id, text string) error {
	p := map[string]any{"callback_query_id": id}
	if text != "" {
		p["text"] = text
	}
	return a.call(ctx, "answerCallbackQuery", p, nil)
}

func (a *api) setMyCommands(ctx context.Context, cmds []BotCommand) error {
	return a.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// cut 按 rune 截断，避免把中文截成半个字。
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
