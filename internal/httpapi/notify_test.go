package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"strm115/internal/config"
	"strm115/internal/mediaserver"
	"strm115/internal/pan"
	"strm115/internal/panpool"
	"strm115/internal/store"
	syncsvc "strm115/internal/sync"
)

// newServerWithStore 和 newTestServer 一样，但把 Server 和 store 都交出来 ——
// 通知配置要验证「密钥到底存下来了没」，只能直接看库。
func newServerWithStore(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	pool := panpool.New(st, pan.Options{MinGap: time.Millisecond, MaxAttempts: 1})
	t.Cleanup(pool.CloseAll)

	syncSvc := syncsvc.New(st, pool.Get,
		func(context.Context) (mediaserver.Server, error) { return nil, errors.New("无媒体服务器") },
		slog.Default())

	s := New(Deps{
		Cfg:   config.Config{Addr: ":0", DataDir: dir, AdminUser: "admin"},
		Store: st,
		Pool:  pool,
		Sync:  syncSvc,
		Log:   slog.Default(),
	})
	t.Cleanup(s.offline.Close)
	// New 会起订阅轮询器和（配了 Token 时的）机器人。Close 把它们的 ctx
	// 一起 cancel，否则每个测试都漏一个常驻 goroutine。
	t.Cleanup(s.Close)
	return s, st
}

// TestMergeNotifySecrets 「留空 = 不修改」只在密钥字段上生效。
func TestMergeNotifySecrets(t *testing.T) {
	old := map[string]any{
		"dingtalk": map[string]any{"webhook": "https://old", "secret": "SEC-OLD"},
		"tg":       map[string]any{"token": "TG-OLD"},
	}
	in := map[string]any{
		"dingtalk": map[string]any{"webhook": "https://new", "secret": ""},
		"tg":       map[string]any{"token": "   "},
	}

	mergeNotifySecrets(in, old)

	if got := jsonString(in, []string{"dingtalk", "secret"}); got != "SEC-OLD" {
		t.Fatalf("空密钥应沿用旧值，实际 %q", got)
	}
	if got := jsonString(in, []string{"dingtalk", "webhook"}); got != "https://new" {
		t.Fatalf("非密钥字段不该被旧值覆盖，实际 %q", got)
	}
	if got := jsonString(in, []string{"tg", "token"}); got != "TG-OLD" {
		t.Fatalf("只有空白的密钥也该沿用旧值，实际 %q", got)
	}

	// 用户重新填了就用新的
	in2 := map[string]any{"dingtalk": map[string]any{"secret": "SEC-NEW"}}
	mergeNotifySecrets(in2, old)
	if got := jsonString(in2, []string{"dingtalk", "secret"}); got != "SEC-NEW" {
		t.Fatalf("填了新值就该用新值，实际 %q", got)
	}
}

// TestNotifySaveAndMask 保存后读回来必须是打码的，且密钥真的落库了。
func TestNotifySaveAndMask(t *testing.T) {
	s, st := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	cfg := map[string]any{
		"dingtalk": map[string]any{"enabled": true, "webhook": "https://oapi.example/robot", "secret": "SEC-RIGHT"},
		"tg":       map[string]any{"enabled": false, "token": "TG-TOKEN", "chat_id": "42"},
	}
	rec, _ := doJSON(t, s.Handler(), http.MethodPut, "/api/notify", map[string]any{
		"config": cfg, "events": []string{"sync_ok", "offline_ok"}, "auto_sync": false,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}

	// 库里应该有明文（服务端要用），读接口不该给
	raw, _ := st.GetSetting(context.Background(), settingNotifyConfig)
	var saved map[string]any
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatalf("入库的 JSON 不合法: %v", err)
	}
	if jsonString(saved, []string{"dingtalk", "secret"}) != "SEC-RIGHT" {
		t.Fatalf("密钥没落库: %s", raw)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodGet, "/api/notify", nil, cookies)
	var got struct {
		Config     map[string]any   `json:"config"`
		SecretSet  map[string]bool  `json:"secret_set"`
		Channels   []string         `json:"channels"`
		Events     []string         `json:"events"`
		AutoSync   bool             `json:"auto_sync"`
		EventLabel []map[string]any `json:"event_labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if v := jsonString(got.Config, []string{"dingtalk", "secret"}); v != "" {
		t.Fatalf("不该回传明文密钥: %q", v)
	}
	if !got.SecretSet["dingtalk.secret"] {
		t.Fatalf("应该标记 secret 已配置: %v", got.SecretSet)
	}
	if !got.SecretSet["tg.token"] {
		t.Fatalf("tg token 也应标记已配置: %v", got.SecretSet)
	}
	if len(got.Channels) != 1 || got.Channels[0] != "dingtalk" {
		t.Fatalf("只应启用 dingtalk，实际 %v", got.Channels)
	}
	if len(got.Events) != 2 {
		t.Fatalf("事件订阅不对: %v", got.Events)
	}
	if got.AutoSync {
		t.Fatal("auto_sync 应该是 false")
	}

	// 再存一次，密钥留空 → 库里仍是旧值
	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/notify", map[string]any{
		"config": map[string]any{
			"dingtalk": map[string]any{"enabled": true, "webhook": "https://oapi.example/robot2", "secret": ""},
		},
		"events": []string{"sync_ok"},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ = st.GetSetting(context.Background(), settingNotifyConfig)
	saved = nil
	_ = json.Unmarshal([]byte(raw), &saved)
	if jsonString(saved, []string{"dingtalk", "secret"}) != "SEC-RIGHT" {
		t.Fatalf("空密钥把已存的冲掉了: %s", raw)
	}
	if jsonString(saved, []string{"dingtalk", "webhook"}) != "https://oapi.example/robot2" {
		t.Fatalf("webhook 应该更新: %s", raw)
	}
}

// TestNotifyEventFilter 事件订阅决定要不要发；没配过就是全开。
func TestNotifyEventFilter(t *testing.T) {
	s, _ := newServerWithStore(t)
	ctx := context.Background()

	if !s.eventOn("sync_ok") || !s.eventOn("offline_fail") {
		t.Fatal("没配过事件时应该全开")
	}

	if err := s.st.SetSetting(ctx, settingNotifyEvents, "sync_ok, offline_ok"); err != nil {
		t.Fatal(err)
	}
	if !s.eventOn("sync_ok") || !s.eventOn("offline_ok") {
		t.Fatal("订阅了的事件应该为真")
	}
	if s.eventOn("sync_fail") {
		t.Fatal("没订阅的事件应该为假")
	}

	// 全都不勾 = 一条都不发（这和「没配过」不同，后者是全开）
	if err := s.st.SetSetting(ctx, settingNotifyEvents, " , "); err != nil {
		t.Fatal(err)
	}
	if s.eventOn("sync_ok") {
		t.Fatal("空列表应视为全关")
	}
}

// TestNotifyTestNoChannel 没配渠道时测试接口要给出「没有启用的渠道」，而不是假装成功。
func TestNotifyTestNoChannel(t *testing.T) {
	s, _ := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodPost, "/api/notify/test", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("接口本身应成功 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ok"] != false {
		t.Fatalf("没有渠道时 ok 应为 false: %v", out)
	}
}

// TestOfflineAddValidation 没有链接 / 没有账号时要给明确错误。
func TestOfflineAddValidation(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/offline/add", map[string]any{"uris": []string{}}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空链接应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/offline/add",
		map[string]any{"uris": []string{"magnet:?xt=urn:btih:aaa"}}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没账号应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestOfflineTasksNoAccount 没账号时列任务要返回空列表 + 说明，不能 500。
func TestOfflineTasksNoAccount(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/offline/tasks", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] == nil || out["error"] == "" {
		t.Fatalf("应该说明原因: %v", out)
	}
}

// TestFindRequiresText 空输入不该去查 IMDb。
func TestFindRequiresText(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/find", map[string]any{"text": "   "}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空文本应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNotifyEventLabelsJSON 事件清单的字段名必须是前端认的 id/label。
//
// 这条测试是为一个真实 bug 加的：结构体少了 json tag，序列化出来是 "ID"/"Label"，
// 前端渲染成四个没有文字的勾选框；用户点保存时 events 传成空数组，
// 于是「保存通知设置」这个动作把全部通知事件静默关掉了。
func TestNotifyEventLabelsJSON(t *testing.T) {
	s, _ := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodGet, "/api/notify", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取通知配置失败 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		EventLabels []map[string]string `json:"event_labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.EventLabels) != 10 {
		t.Fatalf("应有 10 个事件，实际 %d", len(out.EventLabels))
	}
	// 顺序就是界面上勾选框的顺序，按一条片的生命周期排；少一个或顺序乱了
	// 说明有人在中间插了事件却没想清楚它属于哪一段。
	wantIDs := []string{
		"offline_add", "offline_ok", "offline_fail", "organize_ok", "sync_ok",
		"sync_fail", "scrape_ok", "ingest", "subtitle_ok", "delete",
	}
	for i, e := range out.EventLabels {
		if e["id"] == "" || e["label"] == "" {
			t.Fatalf("事件项缺少 id/label（json tag 掉了？）: %#v", e)
		}
		// 界面上每行除标题还有一句说明，缺了会只剩一个空段落。
		if e["desc"] == "" {
			t.Fatalf("事件项缺少 desc: %#v", e)
		}
		if i < len(wantIDs) && e["id"] != wantIDs[i] {
			t.Fatalf("第 %d 个事件应是 %s，实际 %s", i, wantIDs[i], e["id"])
		}
	}
}

// TestNotifyTestSingleChannel 传 channel 时只测那一个，别去惊动其他群；
// 渠道没启用时要如实说「还没启用」，而不是「已发送」。
func TestNotifyTestSingleChannel(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	// 先存一份只有 dingtalk 的配置（webhook 指向本机，发不出去但能证明走了它）
	rec, _ := doJSON(t, h, http.MethodPut, "/api/notify", map[string]any{
		"config": map[string]any{
			"dingtalk": map[string]any{"enabled": true, "webhook": "http://127.0.0.1:1/robot", "keyword": "通知"},
		},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}

	// 测一个没启用的渠道：结果为空，提示要说清楚
	rec, _ = doJSON(t, h, http.MethodPost, "/api/notify/test", map[string]any{"channel": "telegram"}, cookies)
	var out struct {
		OK      bool             `json:"ok"`
		Message string           `json:"message"`
		Channel string           `json:"channel"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || len(out.Results) != 0 {
		t.Fatalf("未启用的渠道不该有结果: %+v", out)
	}
	if !strings.Contains(out.Message, "还没启用") || out.Channel != "telegram" {
		t.Fatalf("提示不对: %+v", out)
	}
}

// TestTGDiscoverRequiresToken 没 Token 时不该真的去打 Telegram，
// 也不该返回 200 —— 让界面把话说清楚。
func TestTGDiscoverRequiresToken(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/notify/tg/discover", map[string]any{}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没 Token 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Token") {
		t.Fatalf("错误信息应提到 Token: %s", rec.Body.String())
	}
}

// TestNotifyPutWithoutEventsKeepsThem 请求体里没有 events 字段时，
// 事件开关必须保持原样 —— 「没传」不等于「全关」。
func TestNotifyPutWithoutEventsKeepsThem(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	if err := st.SetSetting(ctx, settingNotifyEvents, "sync_ok"); err != nil {
		t.Fatal(err)
	}

	rec2, _ := doJSON(t, s.Handler(), http.MethodPut, "/api/notify", map[string]any{"config": map[string]any{}}, cookies)
	if rec2.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec2.Code, rec2.Body.String())
	}
	raw, ok := st.GetSetting(ctx, settingNotifyEvents)
	if !ok {
		t.Fatal("notify_events 这一行不该被删掉")
	}
	if raw != "sync_ok" {
		t.Fatalf("没传 events 时不该改动事件开关，现在变成 %q", raw)
	}

	// 明确传空数组才是「全关」
	doJSON(t, s.Handler(), http.MethodPut, "/api/notify", map[string]any{"config": map[string]any{}, "events": []string{}}, cookies)
	raw2, _ := st.GetSetting(ctx, settingNotifyEvents)
	if raw2 != "" {
		t.Fatalf("显式传空数组应关掉全部事件，现在 %q", raw2)
	}
}
