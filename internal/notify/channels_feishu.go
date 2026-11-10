package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// feishuChannel 飞书 / Lark 群自定义机器人（POST JSON，可选签名）。
type feishuChannel struct{}

func (feishuChannel) Name() string { return "feishu" }

func (feishuChannel) Enabled(cfg *Config) bool {
	return cfg.Feishu.Enabled.On() && strings.TrimSpace(cfg.Feishu.Webhook) != ""
}

// FeishuSign 飞书自定义机器人签名：HMAC-SHA256(key=timestamp+"\n"+secret, data="") 再 base64。
func FeishuSign(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (feishuChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	f := cfg.Feishu
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": m.Text()},
	}
	if secret := strings.TrimSpace(f.Secret); secret != "" {
		ts := fmt.Sprintf("%d", time.Now().Unix())
		payload["timestamp"] = ts
		payload["sign"] = FeishuSign(ts, secret)
	}
	body, status, err := postJSON(ctx, do, strings.TrimSpace(f.Webhook), payload, nil)
	if err != nil {
		return fmt.Errorf("feishu: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("feishu HTTP %d: %s", status, truncate(string(body), 120))
	}
	// 新版返回 {"code":0,...}，旧版返回 {"StatusCode":0}
	var r struct {
		Code       int    `json:"code"`
		StatusCode int    `json:"StatusCode"`
		Msg        string `json:"msg"`
	}
	if json.Unmarshal(body, &r) == nil {
		if r.Code != 0 {
			return fmt.Errorf("feishu 错误 %d: %s", r.Code, r.Msg)
		}
		if r.StatusCode != 0 && r.StatusCode != 200 {
			return fmt.Errorf("feishu 错误 %d", r.StatusCode)
		}
	}
	return nil
}
