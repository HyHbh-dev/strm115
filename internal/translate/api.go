// Package translator implements the Index-Translate free public API client
// and the batching/alignment logic needed to translate whole subtitle files.
//
// The wire format and prompt construction mirror bilibili/Index-Translate's
// inference/llm/call_api.py: an OpenAI-compatible /chat/completions endpoint
// with greedy decoding, thinking disabled, and a Chinese instTrans prompt.
package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"strm115/internal/netproxy"
)

const (
	// DefaultAPIBase is the free public endpoint used by call_api.py.
	DefaultAPIBase = "https://index-translate.bilibili.com/v1"
	// DefaultModel is the flagship model used by call_api.py.
	DefaultModel = "Index-Translate-35B-A3B"
	// DefaultUserAgent must match the reference script.
	DefaultUserAgent = "Index-Translate-Client/1.0"
)

// AvailableModels mirrors the model list exposed by call_api.py's proxy.
var AvailableModels = []string{
	"Index-Translate-35B-A3B",
	"Index-Translate-9B",
	"Index-Translate-2B",
}

// LangNames is the language code -> Chinese name table from call_api.py,
// kept in sync so both entry points build byte-identical prompts.
var LangNames = map[string]string{
	"en": "英语", "zh": "中文", "de": "德语", "fr": "法语", "es": "西班牙语",
	"ja": "日语", "ko": "韩语", "pt": "葡萄牙语", "ru": "俄语", "ar": "阿拉伯语",
	"it": "意大利语", "nl": "荷兰语", "pl": "波兰语", "ro": "罗马尼亚语",
	"sv": "瑞典语", "tr": "土耳其语", "hi": "印地语", "vi": "越南语",
	"th": "泰语", "id": "印尼语", "ms": "马来语", "fil": "菲律宾语",
	"ukr_cyrl": "乌克兰语", "fas_arab": "波斯语", "ces_latn": "捷克语",
	"ell_grek": "希腊语", "dan_latn": "丹麦语", "hun_latn": "匈牙利语",
	"fin_latn": "芬兰语", "nob_latn": "书面挪威语", "slk_latn": "斯洛伐克语",
	"bul_cyrl": "保加利亚语",
}

// LanguageList returns supported language codes with their Chinese names,
// sorted by code for stable CLI output.
func LanguageList() []string {
	codes := make([]string, 0, len(LangNames))
	for c := range LangNames {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, fmt.Sprintf("  %-10s %s", c, LangNames[c]))
	}
	return out
}

// ValidateLang checks a target/source language code.
func ValidateLang(code string) (string, error) {
	name, ok := LangNames[strings.ToLower(code)]
	if !ok {
		return "", fmt.Errorf("unknown language code %q (use --list-langs)", code)
	}
	return name, nil
}

// ---------- prompt construction (equivalent to call_api.py::build_prompt) ----------

// ParseGlossaryTerms accepts a JSON file path, an inline JSON object, or
// comma-separated "k:v" / "k→v" pairs, and returns standardized "A→B" terms.
func ParseGlossaryTerms(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	// Case 1: path to an existing JSON file.
	if data, err := os.ReadFile(input); err == nil {
		if terms, ok := glossaryFromJSON(data); ok {
			return terms
		}
	}
	// Case 2: inline JSON object.
	if strings.HasPrefix(input, "{") && strings.HasSuffix(input, "}") {
		if terms, ok := glossaryFromJSON([]byte(input)); ok {
			return terms
		}
	}
	// Case 3: delimited pairs.
	var pairs []string
	for _, item := range strings.Split(strings.ReplaceAll(input, "，", ","), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		matched := false
		for _, sep := range []string{"->", "→", ":", "："} {
			if idx := strings.Index(item, sep); idx >= 0 {
				k := strings.TrimSpace(item[:idx])
				v := strings.TrimSpace(item[idx+len(sep):])
				pairs = append(pairs, k+"→"+v)
				matched = true
				break
			}
		}
		if !matched {
			pairs = append(pairs, item)
		}
	}
	return pairs
}

// glossaryFromJSON parses {"term": "target"} into "term→target" pairs.
// Map keys are sorted so the generated prompt is deterministic.
func glossaryFromJSON(data []byte) ([]string, bool) {
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	terms := make([]string, 0, len(keys))
	for _, k := range keys {
		v := fmt.Sprint(obj[k])
		terms = append(terms, strings.TrimSpace(k)+"→"+strings.TrimSpace(v))
	}
	return terms, true
}

// BuildPrompt builds the canonical Index-Translate instTrans prompt.
//
// It is a direct Go port of call_api.py::build_prompt. genre is "文本" for
// plain text (identical output to the Python script) and "字幕文本" for
// subtitle batches. Constraints carry their own tags when supplied; bare
// instruction lines are prefixed with 【注意】.
func BuildPrompt(text, targetLang, sourceLang, instruction, glossary, genre string) (string, error) {
	tgtName, err := ValidateLang(targetLang)
	if err != nil {
		return "", err
	}
	hasSource := sourceLang != "" && !strings.EqualFold(sourceLang, "auto")
	srcName := ""
	if hasSource {
		srcName = LangNames[strings.ToLower(sourceLang)]
		if srcName == "" {
			srcName = sourceLang
		}
	}
	if genre == "" {
		genre = "文本"
	}

	var constraints []string
	if glossary != "" {
		if terms := ParseGlossaryTerms(glossary); len(terms) > 0 {
			constraints = append(constraints, "【硬性要求】专名/术语对照: "+strings.Join(terms, "、"))
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(instruction), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "0123456789. ")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "【硬性要求】") || strings.HasPrefix(line, "【注意】") || strings.HasPrefix(line, "【软性要求】") {
			constraints = append(constraints, line)
		} else {
			constraints = append(constraints, "【注意】"+line)
		}
	}

	stripped := strings.TrimSpace(text)
	if len(constraints) > 0 {
		headerSrc := genre
		if srcName != "" {
			headerSrc = srcName + genre
		}
		header := fmt.Sprintf("请将以下%s翻译成%s，并且严格遵循所有约束要求。", headerSrc, tgtName)
		reqLines := make([]string, len(constraints))
		for i, c := range constraints {
			reqLines[i] = fmt.Sprintf("%d. %s", i+1, c)
		}

		suffix := "只输出译文，不要有任何额外说明。"
		if isJSONObject(stripped) {
			for _, c := range constraints {
				if strings.Contains(c, "JSON") || strings.Contains(c, "json") {
					suffix = "请以相同的 JSON 格式输出翻译结果，key 保持不变，value 为对应译文。只输出 JSON，不要有任何额外说明。"
					break
				}
			}
		}
		return fmt.Sprintf("%s\n\n【源文】\n%s\n\n【约束要求】\n%s\n\n%s",
			header, stripped, strings.Join(reqLines, "\n"), suffix), nil
	}

	if srcName != "" {
		return fmt.Sprintf("请将以下%s文本翻译为%s，直接输出翻译结果，不要进行任何解释。\n\n%s", srcName, tgtName, stripped), nil
	}
	return fmt.Sprintf("请将以下文本翻译为%s，直接输出翻译结果，不要进行任何解释。\n\n%s", tgtName, stripped), nil
}

func isJSONObject(s string) bool {
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var obj map[string]interface{}
	return json.Unmarshal([]byte(s), &obj) == nil
}

// StripThink removes chain-of-thought blocks, mirroring call_api.py::strip_think.
func StripThink(text string) string {
	if i := strings.Index(text, "</think>"); i >= 0 {
		text = text[i+len("</think>"):]
	}
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "<think>")
	return strings.TrimSpace(text)
}

// ---------- API client ----------

// APIError is a non-2xx response or an error object returned by the API.
type APIError struct {
	StatusCode int
	Body       string
	Message    string
	Err        error
}

func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("API error (HTTP %d): %s", e.StatusCode, strings.TrimSpace(e.Body))
	}
	if e.Message != "" {
		return "API error: " + e.Message
	}
	return "API error: " + e.Err.Error()
}

func (e *APIError) Unwrap() error { return e.Err }

// Retryable reports whether retrying the same request could plausibly succeed.
func (e *APIError) Retryable() bool {
	if e.Err != nil { // transport-level failure
		return true
	}
	if e.StatusCode == 408 || e.StatusCode == 409 || e.StatusCode == 429 {
		return true
	}
	return e.StatusCode >= 500
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model              string        `json:"model"`
	Messages           []chatMessage `json:"messages"`
	Temperature        float64       `json:"temperature"`
	MaxTokens          int           `json:"max_tokens"`
	Stream             bool          `json:"stream"`
	ChatTemplateKwargs struct {
		EnableThinking bool `json:"enable_thinking"`
	} `json:"chat_template_kwargs"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Client calls the Index-Translate /chat/completions endpoint.
type Client struct {
	BaseURL     string
	Model       string
	MaxTokens   int
	Temperature float64
	UserAgent   string
	HTTP        *http.Client
	// RetryDelay is the base backoff between attempts (tests may set it to 0).
	RetryDelay time.Duration
}

// NewClient constructs a Client with the same defaults as call_api.py.
func NewClient(baseURL, model string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultAPIBase
	}
	if model == "" {
		model = DefaultModel
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		MaxTokens:   4096,
		Temperature: 0.0,
		UserAgent:   DefaultUserAgent,
		HTTP:        &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: netproxy.URL}},
		RetryDelay:  800 * time.Millisecond,
	}
}

// Endpoint returns the full chat/completions URL.
func (c *Client) Endpoint() string { return c.BaseURL + "/chat/completions" }

func (c *Client) buildRequest(prompt string) (chatRequest, error) {
	content, err := json.Marshal(prompt)
	if err != nil {
		return chatRequest{}, err
	}
	var req chatRequest
	req.Model = c.Model
	req.Messages = []chatMessage{{Role: "user", Content: content}}
	req.Temperature = c.Temperature
	req.MaxTokens = c.MaxTokens
	req.Stream = false
	// Keep reasoning OFF: with thinking enabled the CoT text can replace the
	// translation in the response (see call_api.py::call_completion).
	req.ChatTemplateKwargs.EnableThinking = false
	return req, nil
}

// BuildRequestJSON returns the exact JSON body that would be POSTed. Useful
// for --dry-run and for debugging custom API bases.
func (c *Client) BuildRequestJSON(prompt string) ([]byte, error) {
	req, err := c.buildRequest(prompt)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(req, "", "  ")
}

// Complete sends one non-streaming request and returns cleaned translation text.
func (c *Client) Complete(ctx context.Context, prompt string) (string, error) {
	req, err := c.buildRequest(prompt)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	httpReq.Header.Set("User-Agent", ua)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", &APIError{Err: err}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", &APIError{Err: err, StatusCode: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode API response: %w (body: %.200s)", err, respBody)
	}
	if parsed.Error != nil {
		return "", &APIError{StatusCode: resp.StatusCode, Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 || len(parsed.Choices[0].Message.Content) == 0 {
		return "", fmt.Errorf("API response contains no translation (body: %.200s)", respBody)
	}
	raw, err := parseContent(parsed.Choices[0].Message.Content)
	if err != nil {
		return "", err
	}
	return StripThink(raw), nil
}

// parseContent accepts both a plain string and an OpenAI-style array of
// content parts, concatenating all text parts.
func parseContent(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("unexpected message.content shape: %s", raw)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

// CompleteWithRetry retries transient network failures, 429 and 5xx
// responses with exponential backoff.
func (c *Client) CompleteWithRetry(ctx context.Context, prompt string, retries int) (string, error) {
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		text, err := c.Complete(ctx, prompt)
		if err == nil {
			return text, nil
		}
		last = err
		if attempt == retries || !retryable(err) {
			break
		}
		delay := c.RetryDelay // 0 means "no wait", useful for tests
		if delay < 0 {
			delay = 800 * time.Millisecond
		}
		for i := 0; i < attempt; i++ {
			delay *= 2
			if delay > 8*time.Second {
				delay = 8 * time.Second
				break
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
	}
	return "", last
}

func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return false // decoding/context errors are not worth retrying blindly
}
