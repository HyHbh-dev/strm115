package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sourceBlock extracts the 【源文】 block from a generated prompt so a mock
// server can behave like a (well-behaved) translation model.
func sourceBlock(prompt string) []string {
	const marker = "【源文】\n"
	if i := strings.Index(prompt, marker); i >= 0 {
		rest := prompt[i+len(marker):]
		if j := strings.Index(rest, "\n\n【约束要求】"); j >= 0 {
			rest = rest[:j]
		}
		return strings.Split(strings.TrimSuffix(rest, "\n"), "\n")
	}
	if j := strings.LastIndex(prompt, "\n\n"); j >= 0 { // unconstrained prompt
		return strings.Split(strings.TrimSuffix(prompt[j+2:], "\n"), "\n")
	}
	return []string{prompt}
}

// mockTranslate builds a local API that applies rule() to the source lines of
// every request and returns the result as the assistant message.
func mockTranslate(t *testing.T, rule func(src []string) string) *Client {
	t.Helper()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		var prompt string
		_ = json.Unmarshal(req.Messages[0].Content, &prompt)
		respond(w, rule(sourceBlock(prompt)))
	})
	return c
}

// echoRule behaves like a model that keeps the "n|" numbering and copies all
// placeholders, prefixing each translation with 【译】.
func echoRule(src []string) string {
	var b strings.Builder
	for _, line := range src {
		if m := reNumberedPipe.FindStringSubmatch(line); m != nil {
			fmt.Fprintf(&b, "%s|【译】%s\n", m[1], m[2])
		} else {
			fmt.Fprintf(&b, "【译】%s\n", line)
		}
	}
	return b.String()
}

func newTranslator(c *Client) *Translator {
	return &Translator{
		Client:     c,
		Target:     "zh",
		Source:     "auto",
		BatchSize:  3,
		BatchChars: 10000,
		Workers:    3,
		Retries:    0,
	}
}

func TestTranslateBatchKeepsOrder(t *testing.T) {
	c := mockTranslate(t, echoRule)
	tr := newTranslator(c)
	input := make([]string, 10)
	for i := range input {
		input[i] = fmt.Sprintf("line %d", i)
	}
	var progress int32
	tr.OnProgress = func(done, total int) { atomic.StoreInt32(&progress, int32(total)) }

	got, rep, err := tr.Translate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range input {
		if got[i] != "【译】"+want {
			t.Errorf("cue %d = %q, want %q", i, got[i], "【译】"+want)
		}
	}
	if rep.Batches != 4 || rep.Total != 10 || rep.Translated() != 10 || rep.Fallback != 0 {
		t.Errorf("report = %+v", rep)
	}
	if atomic.LoadInt32(&progress) != 4 {
		t.Errorf("progress total = %d", progress)
	}
}

func TestTranslatePreservesInlineTags(t *testing.T) {
	c := mockTranslate(t, echoRule)
	tr := newTranslator(c)
	input := []string{"<i>Hello</i> world", `{\an8}Top line`, "plain text"}
	got, rep, err := tr.Translate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"【译】<i>Hello</i> world", `【译】{\an8}Top line`, "【译】plain text"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cue %d = %q, want %q", i, got[i], want[i])
		}
	}
	if rep.Fallback != 0 {
		t.Errorf("unexpected fallback: %+v", rep)
	}
}

func TestMultilineCueUsesEncodedNewline(t *testing.T) {
	var sawEncoded bool
	c := mockTranslate(t, func(src []string) string {
		for _, l := range src {
			if strings.Contains(l, `First\nSecond`) {
				sawEncoded = true
			}
		}
		return echoRule(src)
	})
	tr := newTranslator(c)
	tr.BatchSize = 1 // force the single-cue path as well
	got, _, err := tr.Translate(context.Background(), []string{"First\nSecond", "Third"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawEncoded {
		t.Error("batch line did not encode embedded newlines")
	}
	if got[0] != "【译】First\nSecond" {
		t.Errorf("newline not restored: %q", got[0])
	}
	if got[1] != "【译】Third" {
		t.Errorf("cue 1 = %q", got[1])
	}
}

func TestMisalignedResponseSplitsAndRetries(t *testing.T) {
	c := mockTranslate(t, func(src []string) string {
		if len(src) > 1 {
			return "I cannot follow the numbering." // no numbered lines
		}
		return echoRule(src)
	})
	tr := newTranslator(c)
	input := []string{"a", "b", "c", "d", "e"}
	got, rep, err := tr.Translate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range input {
		if got[i] != "【译】"+want {
			t.Errorf("cue %d = %q, want %q", i, got[i], "【译】"+want)
		}
	}
	if rep.Fallback != 0 {
		t.Errorf("fallback after split: %+v", rep)
	}
}

var reAnyPlaceholder = regexp.MustCompile(`⟦\s*\d+\s*⟧`)

func TestLostPlaceholderKeepsOriginalText(t *testing.T) {
	c := mockTranslate(t, func(src []string) string {
		var b strings.Builder
		for _, line := range src {
			line = reAnyPlaceholder.ReplaceAllString(line, "")
			if m := reNumberedPipe.FindStringSubmatch(line); m != nil {
				fmt.Fprintf(&b, "%s|【译】%s\n", m[1], m[2])
			} else {
				fmt.Fprintf(&b, "【译】%s\n", line)
			}
		}
		return b.String()
	})
	tr := newTranslator(c)
	input := []string{"<i>Hello</i>", "<b>world</b>"}
	got, rep, err := tr.Translate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for i := range input {
		if got[i] != input[i] {
			t.Errorf("cue %d = %q, want original %q", i, got[i], input[i])
		}
	}
	if rep.Fallback != 2 || len(rep.Messages) < 2 {
		t.Errorf("report = %+v", rep)
	}
}

func TestTranslateAllBatchesFailReturnsError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	tr := newTranslator(c)
	tr.BatchSize = 1
	_, _, err := tr.Translate(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected an error when every batch fails")
	}
}

func TestParseNumberedResponse(t *testing.T) {
	cases := []struct {
		name string
		resp string
		n    int
		want []string
		ok   bool
	}{
		{"strict", "1|一\n2|二\n3|三", 3, []string{"一", "二", "三"}, true},
		{"preamble", "以下是译文：\n1|一\n2|二\n（完）", 2, []string{"一", "二"}, true},
		{"reordered", "2|二\n1|一", 2, []string{"一", "二"}, true},
		{"brackets", "[1] 一\n[2] 二", 2, []string{"一", "二"}, true},
		{"dots", "1. 一\n2. 二", 2, []string{"一", "二"}, true},
		{"no numbering", "一\n二", 2, []string{"一", "二"}, true},
		{"missing line", "1|一", 2, nil, false},
		{"duplicate", "1|一\n1|二", 2, nil, false},
		{"wrong count", "1|一\n2|二\n3|三", 2, nil, false},
		{"empty", "", 2, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseNumberedResponse(c.resp, c.n)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, c.ok, got)
			}
			if ok && strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestProtectRestoreTags(t *testing.T) {
	in := `<i>Hi</i> and {\an8}there, done\h`
	masked, tags := ProtectTags(in)
	if strings.Contains(masked, "<i>") || strings.Contains(masked, `\an8`) {
		t.Errorf("tags not masked: %q", masked)
	}
	if len(tags) != 4 {
		t.Fatalf("tags = %q", tags)
	}
	out, ok := RestoreTags(masked, tags)
	if !ok || out != in {
		t.Errorf("restore = %q, ok=%v, want %q", out, ok, in)
	}
	if _, ok := RestoreTags(strings.Replace(masked, "⟦1⟧", "", 1), tags); ok {
		t.Error("missing placeholder must be rejected")
	}
	if _, ok := RestoreTags(masked+"⟦0⟧", tags); ok {
		t.Error("duplicated placeholder must be rejected")
	}
	if _, ok := RestoreTags(masked+"⟦99⟧", tags); ok {
		t.Error("unknown placeholder must be rejected")
	}
	alt, ok := RestoreTags(strings.ReplaceAll(masked, "⟦1⟧", "[[1]]"), tags)
	if !ok || !strings.Contains(alt, tags[1]) {
		t.Errorf("alternative brackets not normalized: %q ok=%v", alt, ok)
	}
}

func TestCleanSingle(t *testing.T) {
	cases := map[string]string{
		"```\n译文\n```":       "译文",
		"译文：你好":              "你好",
		"1|你好":               "你好",
		"\"你好\"":             "你好",
		"  你好  ":             "你好",
		"Translation: hello": "hello",
	}
	for in, want := range cases {
		if got := cleanSingle(in); got != want {
			t.Errorf("cleanSingle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreviewPrompt(t *testing.T) {
	tr := newTranslator(NewClient("http://127.0.0.1:1/v1", DefaultModel, time.Second))
	tr.BatchSize = 2
	prompt, err := tr.PreviewPrompt([]string{"Hello", "<i>World</i>", "Again"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "1|Hello") || !strings.Contains(prompt, "2|⟦0⟧World⟦1⟧") {
		t.Errorf("numbered source lines missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "【约束要求】") || !strings.Contains(prompt, "请将以下字幕文本翻译成中文") {
		t.Errorf("instTrans structure missing:\n%s", prompt)
	}
}

func TestMakeGroups(t *testing.T) {
	tr := newTranslator(nil)
	tr.BatchSize = 3
	tr.BatchChars = 40
	lines := prepareLines([]string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg"})
	groups := tr.makeGroups(lines)
	var flat []int
	for _, g := range groups {
		if len(g) > 3 {
			t.Errorf("group exceeds batch size: %v", g)
		}
		flat = append(flat, g...)
	}
	if len(flat) != len(lines) {
		t.Fatalf("groups cover %d cues, want %d", len(flat), len(lines))
	}
	for i, idx := range flat {
		if idx != i {
			t.Fatalf("group order broken at %d: %d", i, idx)
		}
	}
	// One very long cue must still get its own group rather than being dropped.
	tr.BatchChars = 10
	long := prepareLines([]string{strings.Repeat("x", 200)})
	if g := tr.makeGroups(long); len(g) != 1 || len(g[0]) != 1 {
		t.Errorf("long cue grouping = %v", g)
	}
}
