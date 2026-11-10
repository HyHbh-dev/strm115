package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeBody 把接口返回的 JSON 解成 map（doJSON 的第二个返回值是 cookie）。
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("返回的不是 JSON: %s", rec.Body.String())
	}
	return out
}

// 没配 Token 时状态接口要说清楚「没 Token、没绑定」，界面据此显示引导。
func TestTGBotStatusEmpty(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/notify/tg/bot", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["token_set"] != false || out["bound"] != false {
		t.Fatalf("空配置时 token_set/bound 都该是 false: %v", out)
	}
	if _, ok := out["commands"]; !ok {
		t.Fatal("应该返回支持的命令列表，前端要展示用法")
	}
}

// 保存了 TG Token 之后，token_set 要跟着变 —— 界面靠它决定提示哪一句。
func TestTGBotStatusAfterToken(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPut, "/api/notify", map[string]any{
		"config": map[string]any{"tg": map[string]any{"enabled": true, "token": "123:abc", "chat_id": ""}},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存通知配置失败 %d: %s", rec.Code, rec.Body.String())
	}

	rec2, _ := doJSON(t, h, http.MethodGet, "/api/notify/tg/bot", nil, cookies)
	out := decodeBody(t, rec2)
	if out["token_set"] != true {
		t.Fatalf("token_set 应为 true: %v", out)
	}
	if out["bound"] != false {
		t.Fatalf("还没人说话，bound 应为 false: %v", out)
	}
}

// 白名单写入：非数字要 400，正常值要看得到。
func TestTGBotConfigValidation(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPut, "/api/notify/tg/bot",
		map[string]any{"chat_ids": "abc"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非数字 Chat ID 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodPut, "/api/notify/tg/bot",
		map[string]any{"chat_ids": "  "}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空列表应 400（要重新绑定请用解绑），实际 %d", rec.Code)
	}

	// 群聊 id 是负数，必须认
	rec, _ = doJSON(t, h, http.MethodPut, "/api/notify/tg/bot",
		map[string]any{"chat_ids": "123456789, -1009876543210"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 ID 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.botAllow(context.Background()); len(got) != 2 {
		t.Fatalf("白名单应有 2 项，实际 %v", got)
	}
	if strings.Contains(s.setting(context.Background(), settingTGBotChats), " ") {
		t.Fatal("存盘时应去掉空格：\"123, -100\" 会让解析多一个空项")
	}

	// 解绑
	rec, _ = doJSON(t, h, http.MethodPut, "/api/notify/tg/bot", map[string]any{"reset": true}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("解绑应 200，实际 %d", rec.Code)
	}
	if got := s.botAllow(context.Background()); len(got) != 0 {
		t.Fatalf("解绑后白名单应为空，实际 %v", got)
	}
}

// 首次说话自动绑定：白名单、推送 Chat ID 都要一起补上。
func TestTGBotBindFirstChat(t *testing.T) {
	s, _ := newServerWithStore(t)
	ctx := context.Background()

	if err := s.botBind(ctx, 778899); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	if got := s.botAllow(ctx); len(got) != 1 || got[0] != 778899 {
		t.Fatalf("白名单应该是 778899，实际 %v", got)
	}
	// 推送用的 Chat ID 顺带补上：同一个会话，不该让用户填第二遍
	if got := s.notify.Config().TG.ChatID; got != "778899" {
		t.Fatalf("TG 推送 Chat ID 应该顺手补上，实际 %q", got)
	}
}

// 消息配置里已经填过 Chat ID 时，机器人直接认它，不必再配一次白名单。
func TestTGBotAllowFallsBackToNotifyChatID(t *testing.T) {
	s, _ := newServerWithStore(t)
	ctx := context.Background()

	cfg := s.notify.Config()
	cfg.TG.ChatID = "424242"
	s.notify.Reload(cfg)

	got := s.botAllow(ctx)
	if len(got) != 1 || got[0] != 424242 {
		t.Fatalf("应该从推送配置里继承 Chat ID，实际 %v", got)
	}
}

// 机器人提交离线时没有 115 账号，要报「先添加账号」而不是网关错误。
func TestBotOfflineNeedsAccount(t *testing.T) {
	s, _ := newServerWithStore(t)
	d := s.botDeps()
	_, err := d.Offline(context.Background(),
		"magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "沙丘2")
	if err == nil {
		t.Fatal("没有账号时应该报错")
	}
	if !strings.Contains(err.Error(), "115 账号") {
		t.Fatalf("错误信息要能指导用户，实际：%v", err)
	}
}
