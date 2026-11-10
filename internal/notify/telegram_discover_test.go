package notify

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 单渠道测试的价值在于「只打扰该打扰的人」。八个渠道全配好之后，
// 点企业微信的「测试」不能顺带把一条消息扇到 TG 群和钉钉群。
func TestTestChannelOnlySendsOne(t *testing.T) {
	resetGlobalCaches()
	d := &captureDoer{resp: tokenAwareResp}
	cfg := Config{
		TG:      TGConfig{Enabled: true, Token: "tgtoken", ChatID: "123"},
		Webhook: WebhookConfig{Enabled: true, URL: "https://hook.example/a", Method: "POST"},
	}
	c := quietClient(cfg, d)

	results := c.TestChannel(context.Background(), "telegram")
	if len(results) != 1 || results[0].Channel != "telegram" || !results[0].OK {
		t.Fatalf("应只发 telegram: %+v", results)
	}
	reqs := d.all()
	if len(reqs) != 1 {
		t.Fatalf("应只有 1 个请求，实际 %d", len(reqs))
	}
	if !strings.Contains(reqs[0].url, "api.telegram.org") {
		t.Fatalf("发错了地址: %s", reqs[0].url)
	}
}

// 渠道名拼错 / 还没启用时不能假装成功：返回空结果，界面据此提示先启用。
func TestTestChannelUnknownIsEmpty(t *testing.T) {
	resetGlobalCaches()
	d := &captureDoer{}
	c := quietClient(Config{TG: TGConfig{Enabled: true, Token: "t", ChatID: "1"}}, d)

	if got := c.TestChannel(context.Background(), "feishu"); len(got) != 0 {
		t.Fatalf("未启用的渠道不应有结果: %+v", got)
	}
	if len(d.all()) != 0 {
		t.Fatal("未启用的渠道不应发出请求")
	}
	// 空名字等价于「全部」，保持 Send 的语义，避免误传字段变成一条都不发。
	if got := c.SendTo(context.Background(), nil, testMessage()); len(got) != 1 {
		t.Fatalf("空名单应扇出到全部渠道: %+v", got)
	}
}

// tgStub 按路径返回 Telegram 的固定响应，顺便记下发过哪些请求。
type tgStub struct {
	paths []string
	me    string
	ups   string
	code  int
}

func (s *tgStub) Do(req *http.Request) (*http.Response, error) {
	s.paths = append(s.paths, req.URL.Path)
	body := s.me
	if strings.HasSuffix(req.URL.Path, "/getUpdates") {
		body = s.ups
	}
	code := s.code
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}, nil
}

func TestDiscoverTelegramListsChats(t *testing.T) {
	stub := &tgStub{
		me: `{"ok":true,"result":{"id":7088690767,"is_bot":true,"first_name":"hyaria2","username":"hyaria2_bot"}}`,
		ups: `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":1,"text":"/start","chat":{"id":555,"type":"private","first_name":"黄","last_name":"颖"}}},
			{"update_id":2,"message":{"message_id":2,"text":"/start","chat":{"id":555,"type":"private","first_name":"黄","last_name":"颖"}}},
			{"update_id":3,"message":{"message_id":3,"text":"同步好了","chat":{"id":-1001234567890,"type":"supergroup","title":"NAS 通知群"}}}
		]}`,
	}
	c := quietClient(Config{}, stub)

	got, err := c.DiscoverTelegram(context.Background(), "7088690767:AAE")
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if got.Bot.Username != "hyaria2_bot" || got.Bot.ID != 7088690767 {
		t.Fatalf("Bot 信息不对: %+v", got.Bot)
	}
	// 同一个会话只留一条，且最近的排前面（getUpdates 是从旧到新）。
	if len(got.Chats) != 2 {
		t.Fatalf("应去重成 2 个会话，实际 %d: %+v", len(got.Chats), got.Chats)
	}
	if got.Chats[0].ID != "-1001234567890" || got.Chats[0].Name != "NAS 通知群" {
		t.Errorf("最近的群应排第一: %+v", got.Chats[0])
	}
	if got.Chats[1].ID != "555" || got.Chats[1].Name != "黄 颖" {
		t.Errorf("私聊应回落到 first_name + last_name: %+v", got.Chats[1])
	}
	if got.Webhook {
		t.Error("正常情况不应标记 webhook")
	}
	if !strings.Contains(got.Hint, "选") {
		t.Errorf("多个会话时提示应让用户选: %s", got.Hint)
	}
}

// 设了 webhook 的 Bot 调 getUpdates 会 409。这是最常见的一种「Token 明明是对的，
// 就是拿不到会话」，必须给出能搜到的解释，而不是 HTTP 409 原文。
func TestDiscoverTelegramWebhookConflict(t *testing.T) {
	stub := &tgStub{
		me:   `{"ok":true,"result":{"id":1,"first_name":"b","username":"b_bot"}}`,
		ups:  `{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active"}`,
		code: http.StatusConflict,
	}
	c := quietClient(Config{}, stub)

	got, err := c.DiscoverTelegram(context.Background(), "tok")
	if err != nil {
		t.Fatalf("409 不该当成错误抛出: %v", err)
	}
	if !got.Webhook || !strings.Contains(got.Hint, "Webhook") {
		t.Fatalf("应提示 webhook 冲突: %+v", got)
	}
}

func TestDiscoverTelegramBadToken(t *testing.T) {
	stub := &tgStub{code: http.StatusUnauthorized, me: `{"ok":false,"error_code":401,"description":"Unauthorized"}`}
	c := quietClient(Config{}, stub)

	_, err := c.DiscoverTelegram(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "Token") {
		t.Fatalf("Token 无效要有明确提示，实际: %v", err)
	}

	if _, err := c.DiscoverTelegram(context.Background(), "  "); err == nil {
		t.Fatal("空 Token 应直接报错，不该发请求")
	}
}

// 一个消息都没收到时，提示要告诉用户「先给 Bot 发句话」——
// 这正是绝大多数人卡住的那一步。
func TestDiscoverTelegramNoMessages(t *testing.T) {
	stub := &tgStub{
		me:  `{"ok":true,"result":{"id":1,"first_name":"b","username":"b_bot"}}`,
		ups: `{"ok":true,"result":[]}`,
	}
	got, err := quietClient(Config{}, stub).DiscoverTelegram(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chats) != 0 {
		t.Fatalf("不该有会话: %+v", got.Chats)
	}
	if !strings.Contains(got.Hint, "@b_bot") {
		t.Errorf("提示应带 Bot 用户名，方便直接搜: %s", got.Hint)
	}
}
