package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ==================== QQ OneBot（NapCat / Lagrange / LLOneBot） ====================

type oneBotChannel struct{}

func (oneBotChannel) Name() string { return "qq_onebot" }

func (oneBotChannel) Enabled(cfg *Config) bool {
	q := cfg.QQOneBot
	return q.Enabled.On() && strings.TrimSpace(q.URL) != "" && strings.TrimSpace(q.Target) != ""
}

func (oneBotChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	q := cfg.QQOneBot
	base := strings.TrimRight(strings.TrimSpace(q.URL), "/")
	num, err := strconv.ParseInt(digitsOnly(q.Target), 10, 64)
	if err != nil {
		return fmt.Errorf("qq_onebot: 目标群号/QQ 号无效: %q", q.Target)
	}
	msgType, key := "group", "group_id"
	if strings.EqualFold(q.TargetType, "private") {
		msgType, key = "private", "user_id"
	}
	payload := map[string]any{
		"message_type": msgType,
		key:            num,
		"message":      m.Text(),
	}
	headers := map[string]string{}
	if tk := strings.TrimSpace(q.Token); tk != "" {
		headers["Authorization"] = "Bearer " + tk
	}
	body, status, err := postJSON(ctx, do, base+"/send_msg", payload, headers)
	if err != nil {
		return fmt.Errorf("qq_onebot: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("qq_onebot HTTP %d: %s", status, truncate(string(body), 120))
	}
	var r struct {
		Status  string `json:"status"`
		Retcode int    `json:"retcode"`
	}
	if json.Unmarshal(body, &r) == nil && r.Retcode != 0 && r.Retcode != 1 {
		return fmt.Errorf("qq_onebot retcode %d (%s)", r.Retcode, r.Status)
	}
	return nil
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ==================== QQ 官方机器人（q.qq.com Bot API） ====================

type qqOfficialChannel struct{}

func (qqOfficialChannel) Name() string { return "qq_official" }

func (qqOfficialChannel) Enabled(cfg *Config) bool {
	q := cfg.QQOfficial
	return q.Enabled.On() && q.AppID != "" && q.Secret != "" && strings.TrimSpace(q.GroupID) != ""
}

var qqTokenCache = struct {
	sync.Mutex
	key    string
	token  string
	expire time.Time
}{}

func qqOfficialAccessToken(ctx context.Context, cfg *Config, do Doer) (string, error) {
	q := cfg.QQOfficial
	key := q.AppID + "|" + q.Secret
	qqTokenCache.Lock()
	if qqTokenCache.key == key && qqTokenCache.token != "" && time.Now().Before(qqTokenCache.expire) {
		t := qqTokenCache.token
		qqTokenCache.Unlock()
		return t, nil
	}
	qqTokenCache.Unlock()

	body, status, err := postJSON(ctx, do, "https://bots.qq.com/app/getAppAccessToken",
		map[string]string{"appId": q.AppID, "clientSecret": q.Secret}, nil)
	if err != nil {
		return "", fmt.Errorf("qq_official token: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("qq_official token HTTP %d: %s", status, truncate(string(body), 120))
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if json.Unmarshal(body, &r) != nil || r.AccessToken == "" {
		return "", fmt.Errorf("qq_official token 返回异常: %s", truncate(string(body), 120))
	}
	sec, _ := strconv.ParseInt(r.ExpiresIn, 10, 64)
	if sec <= 0 {
		sec = 7200
	}
	qqTokenCache.Lock()
	qqTokenCache.key, qqTokenCache.token = key, r.AccessToken
	qqTokenCache.expire = time.Now().Add(time.Duration(sec-300) * time.Second)
	qqTokenCache.Unlock()
	return r.AccessToken, nil
}

func (qqOfficialChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	q := cfg.QQOfficial
	token, err := qqOfficialAccessToken(ctx, cfg, do)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"content":  m.Text(),
		"msg_type": 0,
		"msg_id":   strings.TrimSpace(q.LastMsgID),
	}
	to := "https://api.sgroup.qq.com/groups/" + urlQueryEscape(strings.TrimSpace(q.GroupID)) + "/messages"
	body, status, err := postJSON(ctx, do, to, payload,
		map[string]string{"Authorization": "QQBot " + token})
	if err != nil {
		return fmt.Errorf("qq_official: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("qq_official HTTP %d: %s", status, truncate(string(body), 150))
	}
	return nil
}
