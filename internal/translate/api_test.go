package translate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := NewClient(srv.URL+"/v1", DefaultModel, 5*time.Second)
	c.RetryDelay = 0
	t.Cleanup(srv.Close)
	return c, srv
}

func respond(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{"message": map[string]interface{}{"content": content}},
		},
	})
}

func TestCompleteRequestMatchesCallAPI(t *testing.T) {
	var gotPath, gotMethod, gotUA, gotCT string
	var payload chatRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotUA = r.Header.Get("User-Agent")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		respond(w, "<think>let me translate...</think>\n你好，世界！")
	})

	text, err := c.Complete(context.Background(), "请将以下文本翻译为中文...")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" || gotMethod != http.MethodPost {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotUA != DefaultUserAgent || gotCT != "application/json" {
		t.Errorf("headers = UA:%q CT:%q", gotUA, gotCT)
	}
	if payload.Model != DefaultModel || payload.Temperature != 0.0 || payload.MaxTokens != 4096 {
		t.Errorf("payload = %+v", payload)
	}
	if payload.Stream || payload.ChatTemplateKwargs.EnableThinking {
		t.Error("stream must be false and enable_thinking must be false")
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
		t.Errorf("messages = %+v", payload.Messages)
	}
	var prompt string
	if err := json.Unmarshal(payload.Messages[0].Content, &prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "翻译为中文") {
		t.Errorf("prompt lost: %q", prompt)
	}
	if text != "你好，世界！" {
		t.Errorf("strip_think failed: %q", text)
	}
}

func TestCompleteContentParts(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"第一"},{"type":"text","text":"行"}]}}]}`))
	})
	text, err := c.Complete(context.Background(), "hi")
	if err != nil || text != "第一行" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestCompleteErrors(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	})
	_, err := c.Complete(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected HTTP 400 error, got %v", err)
	}

	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	if _, err := c2.Complete(context.Background(), "x"); err == nil {
		t.Error("expected error for empty choices")
	}
}

func TestRetryTransientErrors(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		respond(w, "成功")
	})
	text, err := c.CompleteWithRetry(context.Background(), "x", 3)
	if err != nil || text != "成功" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid"}}`))
	})
	_, err := c.CompleteWithRetry(context.Background(), "x", 3)
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("4xx should not be retried, calls = %d", calls)
	}
}

func TestBuildPromptPlainText(t *testing.T) {
	got, err := BuildPrompt("Hello, world!", "zh", "auto", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "请将以下文本翻译为中文，直接输出翻译结果，不要进行任何解释。\n\nHello, world!"
	if got != want {
		t.Errorf("prompt mismatch:\ngot  %q\nwant %q", got, want)
	}

	got2, err := BuildPrompt("Bonjour", "zh", "fr", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	want2 := "请将以下法语文本翻译为中文，直接输出翻译结果，不要进行任何解释。\n\nBonjour"
	if got2 != want2 {
		t.Errorf("source-language prompt mismatch:\ngot  %q\nwant %q", got2, want2)
	}
	if _, err := BuildPrompt("x", "xx", "auto", "", "", ""); err == nil {
		t.Error("unknown target language should fail")
	}
}

func TestBuildPromptConstraints(t *testing.T) {
	got, err := BuildPrompt(`{"title": "用户协议"}`, "en", "auto",
		"1. 严格保留合法 JSON 语法格式，仅翻译 value 文本\n2. 保持简洁",
		"碳纤维:carbon fiber, 抗裂缝:crack resistance", "文本")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"请将以下文本翻译成英语，并且严格遵循所有约束要求。",
		"【源文】",
		"【约束要求】",
		"1. 【硬性要求】专名/术语对照: 碳纤维→carbon fiber、抗裂缝→crack resistance",
		"2. 【注意】严格保留合法 JSON 语法格式，仅翻译 value 文本",
		"3. 【注意】保持简洁",
		"请以相同的 JSON 格式输出翻译结果",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildPromptNonJSONSuffix(t *testing.T) {
	got, err := BuildPrompt("Hello", "zh", "auto", "【注意】口语化", "", "字幕文本")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "请将以下字幕文本翻译成中文") {
		t.Errorf("genre not used: %s", got)
	}
	if !strings.Contains(got, "只输出译文，不要有任何额外说明。") {
		t.Errorf("unexpected suffix: %s", got)
	}
}

func TestParseGlossaryTerms(t *testing.T) {
	got := ParseGlossaryTerms("碳纤维:carbon fiber，抗裂缝：crack resistance, A->B, C→D, lone")
	want := []string{"碳纤维→carbon fiber", "抗裂缝→crack resistance", "A→B", "C→D", "lone"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
	if got := ParseGlossaryTerms(`{"z":"Z","a":"A"}`); strings.Join(got, "|") != "a→A|z→Z" {
		t.Errorf("inline JSON not sorted: %q", got)
	}
	path := filepath.Join(t.TempDir(), "terms.json")
	if err := os.WriteFile(path, []byte(`{"GPU":"显卡"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ParseGlossaryTerms(path); strings.Join(got, "|") != "GPU→显卡" {
		t.Errorf("glossary file: %q", got)
	}
	if ParseGlossaryTerms("") != nil {
		t.Error("empty glossary should return nil")
	}
}

func TestStripThink(t *testing.T) {
	// Mirror Python: only text after </think> is kept; a leading "<think>"
	// prefix is removed from the remaining text.
	if got := StripThink("<think>r</think>\n 译文 \n"); got != "译文" {
		t.Errorf("got %q", got)
	}
	if got := StripThink("  <think>译文  "); got != "译文" {
		t.Errorf("leading <think> not removed: %q", got)
	}
	if got := StripThink("译文"); got != "译文" {
		t.Errorf("got %q", got)
	}
}

func TestLanguageList(t *testing.T) {
	list := LanguageList()
	if len(list) != len(LangNames) {
		t.Errorf("len = %d, want %d", len(list), len(LangNames))
	}
	joined := strings.Join(list, "\n")
	if !strings.Contains(joined, "zh") || !strings.Contains(joined, "中文") {
		t.Error("zh missing from language list")
	}
}
