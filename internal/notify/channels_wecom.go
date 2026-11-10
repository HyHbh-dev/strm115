package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// wecomChannel 企业微信自建应用：gettoken → message/send。
// 不是群机器人 webhook，因此需要 CorpID / Secret / AgentID。
type wecomChannel struct{}

func (wecomChannel) Name() string { return "wecom" }

func (wecomChannel) Enabled(cfg *Config) bool {
	return cfg.Wecom.Enabled.On() && cfg.Wecom.CorpID != "" && cfg.Wecom.Secret != ""
}

var wecomTokenCache = struct {
	sync.Mutex
	key    string
	token  string
	expire time.Time
}{}

// wecomAccessToken 带缓存的 access_token（企微有效期 7200 秒，提前 10 分钟刷新）。
// 缓存按 API 地址 + CorpID + Secret 区分，多租户不会串号。
func wecomAccessToken(ctx context.Context, cfg *Config, do Doer) (string, error) {
	w := cfg.Wecom
	key := w.APIBase() + "|" + w.CorpID + "|" + w.Secret
	wecomTokenCache.Lock()
	if wecomTokenCache.key == key && wecomTokenCache.token != "" && time.Now().Before(wecomTokenCache.expire) {
		t := wecomTokenCache.token
		wecomTokenCache.Unlock()
		return t, nil
	}
	wecomTokenCache.Unlock()

	to := fmt.Sprintf("%s/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
		w.APIBase(), urlQueryEscape(w.CorpID), urlQueryEscape(w.Secret))
	body, status, err := doRequest(ctx, do, http.MethodGet, to, "", nil, nil)
	if err != nil {
		return "", fmt.Errorf("wecom gettoken: %s", sanitizeErr(err))
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("wecom gettoken HTTP %d", status)
	}
	var r struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if json.Unmarshal(body, &r) != nil || r.AccessToken == "" {
		return "", fmt.Errorf("wecom gettoken 返回异常: %s", truncate(string(body), 120))
	}
	if r.ErrCode != 0 {
		return "", fmt.Errorf("wecom gettoken %d: %s", r.ErrCode, r.ErrMsg)
	}
	expire := time.Now().Add(time.Duration(r.ExpiresIn)*time.Second - 10*time.Minute)
	wecomTokenCache.Lock()
	wecomTokenCache.key, wecomTokenCache.token, wecomTokenCache.expire = key, r.AccessToken, expire
	wecomTokenCache.Unlock()
	return r.AccessToken, nil
}

func (ch wecomChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	token, err := wecomAccessToken(ctx, cfg, do)
	if err != nil {
		return err
	}
	sendURL := fmt.Sprintf("%s/cgi-bin/message/send?access_token=%s",
		cfg.Wecom.APIBase(), urlQueryEscape(token))
	chunks := splitWecomText(m.Text(), 1800)
	for i, chunk := range chunks {
		payload := map[string]any{
			"touser":  "@all",
			"msgtype": "text",
			"agentid": cfg.Wecom.AgentID,
			"text":    map[string]string{"content": chunk},
		}
		body, status, err := postJSON(ctx, do, sendURL, payload, nil)
		if err != nil {
			// 传输层错误重试一次，避免网络抖动丢消息。
			if !sleepCtx(ctx, 2*time.Second) {
				return ctx.Err()
			}
			body, status, err = postJSON(ctx, do, sendURL, payload, nil)
			if err != nil {
				return fmt.Errorf("wecom send: %w", err)
			}
		}
		if e := apiError(body, status); e != nil {
			return fmt.Errorf("wecom send: %w", e)
		}
		if i < len(chunks)-1 && !sleepCtx(ctx, 400*time.Millisecond) {
			return ctx.Err()
		}
	}
	return nil
}

// SendRich 企微 news 图文卡片；被拒（封面不可达等）自动退回纯文本。
func (ch wecomChannel) SendRich(ctx context.Context, cfg *Config, do Doer, m Message) error {
	link := m.Link
	if link == "" {
		link = "https://115.com"
	}
	err := ch.sendNews(ctx, cfg, do, []Article{{
		Title:       truncate(m.Title, 90),
		Description: truncate(m.Content, 300),
		PicURL:      m.Image,
		URL:         link,
	}})
	if err == nil {
		return nil
	}
	return ch.Send(ctx, cfg, do, Message{Kind: m.Kind, Title: m.Title, Content: m.Content, Force: m.Force})
}

// SendNews 一条 news 最多 8 篇卡片。
func (ch wecomChannel) SendNews(ctx context.Context, cfg *Config, do Doer, articles []Article) error {
	if len(articles) > 8 {
		articles = articles[:8]
	}
	return ch.sendNews(ctx, cfg, do, articles)
}

func (ch wecomChannel) sendNews(ctx context.Context, cfg *Config, do Doer, articles []Article) error {
	token, err := wecomAccessToken(ctx, cfg, do)
	if err != nil {
		return err
	}
	list := make([]map[string]string, 0, len(articles))
	for _, a := range articles {
		link := a.URL
		if link == "" {
			link = "https://115.com"
		}
		list = append(list, map[string]string{
			"title":       truncate(a.Title, 90),
			"description": truncate(a.Description, 200),
			"picurl":      a.PicURL,
			"url":         link,
		})
	}
	payload := map[string]any{
		"touser":  "@all",
		"msgtype": "news",
		"agentid": cfg.Wecom.AgentID,
		"news":    map[string]any{"articles": list},
	}
	sendURL := fmt.Sprintf("%s/cgi-bin/message/send?access_token=%s",
		cfg.Wecom.APIBase(), urlQueryEscape(token))
	body, status, err := postJSON(ctx, do, sendURL, payload, nil)
	if err != nil {
		return err
	}
	return apiError(body, status)
}

// splitWecomText 企微 text 上限 2048 字节，按行边界切成 ≤ maxBytes 段；
// 单行超长时在 rune 边界硬切，避免切出半个汉字。
func splitWecomText(msg string, maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = 1800
	}
	if len(msg) <= maxBytes {
		return []string{msg}
	}
	var out, cur []string
	curLen := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
			cur, curLen = nil, 0
		}
	}
	for _, ln := range strings.Split(msg, "\n") {
		for len(ln) > maxBytes {
			cut := maxBytes
			for cut > 0 && !utf8.RuneStart(ln[cut]) {
				cut--
			}
			flush()
			out = append(out, ln[:cut])
			ln = ln[cut:]
		}
		if curLen+len(ln)+1 > maxBytes && len(cur) > 0 {
			flush()
		}
		cur = append(cur, ln)
		curLen += len(ln) + 1
	}
	flush()
	if len(out) == 0 {
		return []string{msg}
	}
	return out
}
