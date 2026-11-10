package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// webhookChannel 通用 Webhook：默认 POST JSON，支持地址 / 请求体模板，
// 可对接 Bark、PushPlus、ntfy 以及钉钉等自定义机器人。
type webhookChannel struct{}

func (webhookChannel) Name() string { return "webhook" }

func (webhookChannel) Enabled(cfg *Config) bool {
	return cfg.Webhook.Enabled.On() && strings.TrimSpace(cfg.Webhook.URL) != ""
}

func (webhookChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	if !m.Force && !kindWanted(cfg.Webhook.Types, m.Kind) {
		return errSkip
	}
	req, err := BuildWebhookRequest(cfg.Webhook, m)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	req = req.WithContext(ctx)
	resp, err := do.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	return nil
}

// WebhookFields 返回模板可用变量，也是默认 JSON 载荷的字段。
func WebhookFields(m Message) map[string]string {
	m = m.normalized()
	return map[string]string{
		"type":      string(m.Kind),
		"type_name": KindNames[m.Kind],
		"title":     strings.TrimSpace(m.Title),
		"content":   strings.TrimSpace(m.Content),
		"text":      m.Text(),
		"image":     m.Image,
		"link":      m.Link,
		"time":      m.At.Format(time.RFC3339),
	}
}

// BuildWebhookRequest 按配置构造请求（纯函数，可直接单测）。
// POST 且 Body 为空 → 默认 JSON；Body 是模板 → 填 {{var}} 后校验是否 JSON；
// GET 且地址含 {{var}} → 按路径转义填充，否则把字段放进查询参数。
func BuildWebhookRequest(cfg WebhookConfig, m Message) (*http.Request, error) {
	fields := WebhookFields(m)
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, fmt.Errorf("webhook 地址为空")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Method), http.MethodGet) {
		var target string
		if strings.Contains(raw, "{{") {
			target = fillTemplate(raw, fields, url.PathEscape)
		} else {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("webhook 地址不对: %w", err)
			}
			q := u.Query()
			for _, k := range []string{"type", "type_name", "title", "content", "image", "link", "time"} {
				if fields[k] != "" {
					q.Set(k, fields[k])
				}
			}
			u.RawQuery = q.Encode()
			target = u.String()
		}
		return http.NewRequest(http.MethodGet, target, nil)
	}

	target := raw
	if strings.Contains(raw, "{{") {
		target = fillTemplate(raw, fields, url.QueryEscape)
	}
	var body []byte
	ctype := "application/json"
	if tpl := strings.TrimSpace(cfg.Body); tpl != "" {
		body = []byte(fillTemplate(tpl, fields, jsonInner))
		if !json.Valid(body) {
			ctype = "text/plain; charset=utf-8" // 用户写的不是 JSON，照原样发
		}
	} else {
		payload := map[string]string{"source": "strm115"}
		for k, v := range fields {
			payload[k] = v
		}
		body, _ = json.Marshal(payload)
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("User-Agent", "strm115/1.0")
	return req, nil
}
