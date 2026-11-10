package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedReq struct {
	method string
	url    string
	body   []byte
	header http.Header
}

type captureDoer struct {
	mu   sync.Mutex
	reqs []recordedReq
	resp func(*http.Request) (int, string)
}

const okBody = `{"errcode":0,"errmsg":"ok","code":0,"StatusCode":0,"ok":true,"status":"ok","retcode":0}`

func (d *captureDoer) Do(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
	}
	d.mu.Lock()
	d.reqs = append(d.reqs, recordedReq{req.Method, req.URL.String(), b, req.Header.Clone()})
	d.mu.Unlock()
	status, body := 200, okBody
	if d.resp != nil {
		status, body = d.resp(req)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}, nil
}

func (d *captureDoer) all() []recordedReq {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedReq(nil), d.reqs...)
}

func tokenAwareResp(req *http.Request) (int, string) {
	switch {
	case strings.Contains(req.URL.Path, "getAppAccessToken"):
		return 200, `{"access_token":"QQTK","expires_in":"7200"}`
	case strings.Contains(req.URL.Path, "gettoken"):
		return 200, `{"errcode":0,"access_token":"WECOM_TK","expires_in":7200}`
	default:
		return 200, okBody
	}
}

func quietClient(cfg Config, d Doer) *Client { return NewWithDoer(cfg, d, nil) }

func TestSwitchAcceptsLegacyValues(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`true`, true}, {`false`, false}, {`"true"`, true}, {`"TRUE"`, true},
		{`"false"`, false}, {`1`, true}, {`0`, false}, {`null`, false},
	}
	for _, tc := range cases {
		var cfg struct {
			DingTalk DingTalkConfig `json:"dingtalk"`
		}
		if err := json.Unmarshal([]byte(`{"dingtalk":{"enabled":`+tc.in+`}}`), &cfg); err != nil {
			t.Fatalf("解析 %s 失败: %v", tc.in, err)
		}
		if got := cfg.DingTalk.Enabled.On(); got != tc.want {
			t.Errorf("enabled=%s 得到 %v，期望 %v", tc.in, got, tc.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("空配置不该报错: %v", err)
	}
	bad := Config{DingTalk: DingTalkConfig{Enabled: true}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "dingtalk") {
		t.Fatalf("启用但缺 webhook 应报错，得到 %v", err)
	}
	good := Config{DingTalk: DingTalkConfig{Enabled: true, Webhook: "https://oapi.dingtalk.com/robot/send?access_token=x"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("完整配置不该报错: %v", err)
	}
}

func TestKindWanted(t *testing.T) {
	if !kindWanted(nil, KindIngest) {
		t.Fatal("空勾选应全推")
	}
	if kindWanted(nil, KindBot) {
		t.Fatal("机器人回复永不推送")
	}
	if kindWanted([]string{string(KindIngest)}, KindTask) {
		t.Fatal("未勾选的类型应被过滤")
	}
	if !kindWanted([]string{string(KindIngest)}, KindIngest) {
		t.Fatal("已勾选的类型应放行")
	}
}

func TestSplitWecomTextKeepsUTF8(t *testing.T) {
	msg := strings.Repeat("中文消息ABC", 500) // 约 4500 字节
	chunks := splitWecomText(msg, 1800)
	if len(chunks) < 2 {
		t.Fatal("长文本应被切分")
	}
	var b strings.Builder
	for _, c := range chunks {
		if len(c) > 1800 {
			t.Fatalf("分段超长: %d", len(c))
		}
		b.WriteString(c)
	}
	if b.String() != msg {
		t.Fatal("切分后拼回的内容与原文本不一致（换行被吞或截断）")
	}
	for _, c := range chunks {
		if !isUTF8(c) {
			t.Fatal("分段切出了非法 UTF-8")
		}
	}
}

func isUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func TestDingTalkSignAndPayload(t *testing.T) {
	d := &captureDoer{}
	cfg := Config{DingTalk: DingTalkConfig{
		Enabled: true,
		Webhook: "https://oapi.dingtalk.com/robot/send?access_token=abc",
		Secret:  "SECtest",
		Keyword: "监控",
	}}
	results := quietClient(cfg, d).Send(context.Background(), Message{
		Kind: KindTask, Title: "清理完成", Content: "删除 3 个空目录",
	})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("钉钉发送失败: %+v", results)
	}
	req := d.all()[0]
	if !strings.Contains(req.url, "timestamp=") || !strings.Contains(req.url, "sign=") {
		t.Fatalf("加签参数缺失: %s", req.url)
	}
	u, _ := url.Parse(req.url)
	if u.Query().Get("access_token") != "abc" || u.Query().Get("sign") == "" {
		t.Fatalf("加签 URL 拼错: %s", req.url)
	}
	var payload map[string]any
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["msgtype"] != "text" {
		t.Fatalf("msgtype 错误: %v", payload["msgtype"])
	}
	text := payload["text"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(text, "监控") || !strings.Contains(text, "删除 3 个空目录") {
		t.Fatalf("关键词未前置或正文丢失: %q", text)
	}
	// 无 Secret 时不加签
	if got := DingTalkSignURL("https://x/hook", ""); got != "https://x/hook" {
		t.Fatalf("空 Secret 不应改 URL: %s", got)
	}
	// 已有 query 时用 & 连接
	if got := DingTalkSignURL("https://x/hook?a=1", "SEC"); !strings.Contains(got, "?a=1&timestamp=") {
		t.Fatalf("已有 query 拼接错误: %s", got)
	}
}

func TestDingTalkRichMarkdown(t *testing.T) {
	d := &captureDoer{}
	cfg := Config{DingTalk: DingTalkConfig{Enabled: true, Webhook: "https://x/hook"}}
	c := quietClient(cfg, d)
	res := c.SendRich(context.Background(), Message{
		Kind: KindIngest, Title: "新片入库", Content: "沙丘 2",
		Image: "https://img/p.jpg", Link: "https://detail",
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("富媒体发送失败: %+v", res)
	}
	var p map[string]any
	_ = json.Unmarshal(d.all()[0].body, &p)
	if p["msgtype"] != "markdown" {
		t.Fatalf("应发 markdown，得到 %v", p["msgtype"])
	}
	md := p["markdown"].(map[string]any)["text"].(string)
	if !strings.Contains(md, "![cover](https://img/p.jpg)") || !strings.Contains(md, "https://detail") {
		t.Fatalf("markdown 缺海报或链接: %s", md)
	}
}

func TestBuildWebhookRequest(t *testing.T) {
	m := Message{Kind: KindIngest, Title: "入库", Content: "第一行\n第二行", Image: "https://img/p.jpg", Link: "https://d"}

	// 默认 POST JSON
	req, err := BuildWebhookRequest(WebhookConfig{URL: "https://hook.example/a", Method: "POST"}, m)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != "https://hook.example/a" {
		t.Fatalf("默认请求不对: %s %s", req.Method, req.URL)
	}
	var p map[string]string
	_ = json.Unmarshal(readBody(req), &p)
	if p["source"] != "strm115" || p["type"] != "ingest" || p["type_name"] != "入库" || p["text"] != "入库\n第一行\n第二行" {
		t.Fatalf("默认载荷字段错误: %+v", p)
	}

	// POST 模板 + JSON 转义（正文含换行与引号不应破坏 JSON）
	req, _ = BuildWebhookRequest(WebhookConfig{
		URL: "https://hook.example/a", Method: "POST",
		Body: `{"title":"{{title}}","text":"{{text}}","time":"{{time}}"}`,
	}, Message{Kind: KindSystem, Title: `带"引号"`, Content: "换行\n内容"})
	if !json.Valid(readBody(req)) {
		t.Fatalf("模板填充后不是合法 JSON: %s", readBody(req))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type 错误: %s", req.Header.Get("Content-Type"))
	}

	// GET 模板：按路径转义
	req, _ = BuildWebhookRequest(WebhookConfig{
		URL: "https://api.day.app/KEY/{{title}}/{{content}}", Method: "GET",
	}, Message{Kind: KindSystem, Title: "入库 通知", Content: "a/b"})
	if !strings.Contains(req.URL.String(), "%20") || !strings.Contains(req.URL.EscapedPath(), "a%2Fb") {
		t.Fatalf("GET 模板路径未转义: %s", req.URL.String())
	}

	// GET 无模板：字段进查询参数
	req, _ = BuildWebhookRequest(WebhookConfig{URL: "https://hook.example/a", Method: "GET"}, m)
	if req.URL.Query().Get("title") != "入库" || req.URL.Query().Get("image") != "https://img/p.jpg" {
		t.Fatalf("GET 查询参数缺失: %s", req.URL.String())
	}
}

func readBody(req *http.Request) []byte {
	if req.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(strings.NewReader(string(b)))
	return b
}

// resetGlobalCaches 保证 -count=N 复跑时断言稳定：
// token 缓存与限速器是包级状态，跨迭代不会自动清空。
func resetGlobalCaches() {
	wecomTokenCache.Lock()
	wecomTokenCache.key, wecomTokenCache.token, wecomTokenCache.expire = "", "", time.Time{}
	wecomTokenCache.Unlock()
	qqTokenCache.Lock()
	qqTokenCache.key, qqTokenCache.token, qqTokenCache.expire = "", "", time.Time{}
	qqTokenCache.Unlock()
	iyuuLimiter.mu.Lock()
	iyuuLimiter.last = time.Time{}
	iyuuLimiter.mu.Unlock()
}

func TestAllChannelsFanOut(t *testing.T) {
	resetGlobalCaches()
	d := &captureDoer{resp: tokenAwareResp}
	cfg := Config{
		Wecom:      WecomConfig{Enabled: true, CorpID: "corp", Secret: "sec", AgentID: "100"},
		DingTalk:   DingTalkConfig{Enabled: true, Webhook: "https://oapi.dingtalk.com/robot/send?access_token=x"},
		TG:         TGConfig{Enabled: true, Token: "tgtoken", ChatID: "123"},
		Feishu:     FeishuConfig{Enabled: true, Webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/x", Secret: "fs"},
		QQOneBot:   QQOneBotConfig{Enabled: true, URL: "http://napcat:3000", Target: "888", TargetType: "group"},
		QQOfficial: QQOfficialConfig{Enabled: true, AppID: "app", Secret: "s", GroupID: "grp"},
		Iyuu:       IyuuConfig{Enabled: true, Token: "IYUU123"},
		Webhook:    WebhookConfig{Enabled: true, URL: "https://hook.example/a", Method: "POST"},
	}
	c := quietClient(cfg, d)
	if got := len(c.Channels()); got != 8 {
		t.Fatalf("应启用 8 个渠道，实际 %d: %v", got, c.Channels())
	}
	results := c.Send(context.Background(), Message{Kind: KindIngest, Title: "入库", Content: "测试"})
	if len(results) != 8 {
		t.Fatalf("结果数量不对: %d", len(results))
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("渠道 %s 失败: %s", r.Channel, r.Error)
		}
	}
	joined := ""
	for _, r := range d.all() {
		joined += r.url + "\n"
	}
	for _, want := range []string{"gettoken", "message/send", "oapi.dingtalk.com", "api.telegram.org", "open.feishu.cn", "/send_msg", "getAppAccessToken", "/groups/grp/messages", "iyuu.cn/IYUU123.send", "hook.example/a"} {
		if !strings.Contains(joined, want) {
			t.Errorf("未看到 %s 的请求；实际请求:\n%s", want, joined)
		}
	}
}

func TestTypeFilterSkipAndForce(t *testing.T) {
	d := &captureDoer{}
	cfg := Config{Webhook: WebhookConfig{
		Enabled: true, URL: "https://hook.example/a", Method: "POST", Types: []string{string(KindIngest)},
	}}
	c := quietClient(cfg, d)
	r := c.Send(context.Background(), Message{Kind: KindTask, Title: "定时任务"})
	if len(r) != 1 || !r[0].Skipped || r[0].OK {
		t.Fatalf("未勾选类型应标记 skipped: %+v", r)
	}
	if len(d.all()) != 0 {
		t.Fatal("被过滤的消息不应发出 HTTP 请求")
	}
	r = c.Test(context.Background()) // Force=true 绕过过滤
	if len(r) != 1 || !r[0].OK {
		t.Fatalf("测试消息应强制发送: %+v", r)
	}
	if len(d.all()) != 1 {
		t.Fatal("测试消息未发出")
	}
}

type fakeChannel struct {
	name string
	mu   sync.Mutex
	msgs []Message
}

func (f *fakeChannel) Name() string         { return f.name }
func (f *fakeChannel) Enabled(*Config) bool { return true }
func (f *fakeChannel) Send(_ context.Context, _ *Config, _ Doer, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return nil
}

func TestRegisterCustomChannelAndRichFallback(t *testing.T) {
	registryMu.Lock()
	saved := append([]Channel(nil), customChannels...)
	customChannels = nil
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		customChannels = saved
		registryMu.Unlock()
	}()

	f := &fakeChannel{name: "custom"}
	Register(f)
	if got := RegisteredChannels(); got[len(got)-1] != "custom" {
		t.Fatalf("自定义渠道未注册: %v", got)
	}
	c := quietClient(Config{}, &captureDoer{})
	res := c.SendRich(context.Background(), Message{Kind: KindSystem, Title: "标题", Content: "正文", Image: "https://img/p.jpg"})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("自定义渠道未收到消息: %+v", res)
	}
	if got := f.msgs[0]; got.Title != "标题" || got.Content != "正文" {
		t.Fatalf("未实现 RichChannel 时应回退纯文本，得到 %+v", got)
	}

	// 同名注册覆盖内置渠道
	d := &captureDoer{}
	override := &fakeChannel{name: "dingtalk"}
	Register(override)
	c2 := quietClient(Config{DingTalk: DingTalkConfig{Enabled: true, Webhook: "https://x/hook"}}, d)
	res = c2.Send(context.Background(), Message{Title: "覆盖测试"})
	// fakeChannel 的 Enabled 恒为 true，因此 custom 也在此次扇出里。
	if len(res) != 2 {
		t.Fatalf("应扇出 2 个渠道，得到 %+v", res)
	}
	for _, r := range res {
		if !r.OK {
			t.Fatalf("渠道发送失败: %+v", r)
		}
	}
	if len(d.all()) != 0 {
		t.Fatalf("内置 dingtalk 已被覆盖，不该发出 HTTP 请求，实际 %d 条", len(d.all()))
	}
	if len(override.msgs) != 1 {
		t.Fatal("覆盖实现未被调用")
	}
}
