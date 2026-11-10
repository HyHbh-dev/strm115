package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// dingTalkChannel 钉钉群自定义机器人。
// 三种安全设置都照顾到：Secret 非空 → 加签；Keyword 非空 → 自动把关键词
// 前置进正文，避免「自定义关键词」把通知拒收；都不填则用 IP 白名单。
type dingTalkChannel struct{}

func (dingTalkChannel) Name() string { return "dingtalk" }

func (dingTalkChannel) Enabled(cfg *Config) bool {
	return cfg.DingTalk.Enabled.On() && strings.TrimSpace(cfg.DingTalk.Webhook) != ""
}

// DingTalkSignURL 给钉钉机器人地址追加加签参数（导出便于自行对接）。
// 规则：sign = base64(HMAC-SHA256(key=secret, data=timestamp+"\n"+secret))，
// timestamp 为毫秒，sign 需要 URL 编码后拼到 query。
func DingTalkSignURL(raw, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return raw
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "\n" + secret))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + "timestamp=" + ts + "&sign=" + sign
}

func (ch dingTalkChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	d := cfg.DingTalk
	text := m.Text()
	if kw := strings.TrimSpace(d.Keyword); kw != "" && !strings.Contains(text, kw) {
		text = kw + "\n" + text
	}
	payload := map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	}
	if at := dingTalkAt(d); at != nil {
		payload["at"] = at
	}
	return ch.post(ctx, d, do, payload)
}

// SendRich 钉钉 markdown 消息（带海报），失败退回纯文本。
func (ch dingTalkChannel) SendRich(ctx context.Context, cfg *Config, do Doer, m Message) error {
	d := cfg.DingTalk
	var b strings.Builder
	b.WriteString("### " + strings.TrimSpace(m.Title) + "\n\n")
	if m.Content != "" {
		b.WriteString(m.Content + "\n\n")
	}
	if m.Image != "" {
		b.WriteString(fmt.Sprintf("![cover](%s)\n\n", m.Image))
	}
	if m.Link != "" {
		b.WriteString(fmt.Sprintf("[查看详情](%s)", m.Link))
	}
	text := b.String()
	if kw := strings.TrimSpace(d.Keyword); kw != "" && !strings.Contains(text, kw) {
		text = kw + "\n\n" + text
	}
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"title": truncate(strings.TrimSpace(m.Title), 60), "text": text},
	}
	if at := dingTalkAt(d); at != nil {
		payload["at"] = at
	}
	if err := ch.post(ctx, d, do, payload); err != nil {
		return ch.Send(ctx, cfg, do, Message{Kind: m.Kind, Title: m.Title, Content: m.Content, Force: m.Force})
	}
	return nil
}

func (ch dingTalkChannel) post(ctx context.Context, d DingTalkConfig, do Doer, payload any) error {
	to := DingTalkSignURL(strings.TrimSpace(d.Webhook), d.Secret)
	body, status, err := postJSON(ctx, do, to, payload, nil)
	if err != nil {
		return fmt.Errorf("dingtalk: %w", err)
	}
	if e := apiError(body, status); e != nil {
		return fmt.Errorf("dingtalk: %w", e)
	}
	return nil
}

func dingTalkAt(d DingTalkConfig) map[string]any {
	if len(d.AtMobiles) == 0 && !d.AtAll {
		return nil
	}
	return map[string]any{"atMobiles": d.AtMobiles, "isAtAll": d.AtAll}
}
