package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// postJSON POST 一个 JSON 请求体，返回响应体与状态码。
func postJSON(ctx context.Context, do Doer, to string, payload any, headers map[string]string) ([]byte, int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("序列化请求失败: %w", err)
	}
	return doRequest(ctx, do, http.MethodPost, to, "application/json", b, headers)
}

// getJSON GET 一个地址，返回响应体与状态码。Telegram 的查会话接口用得到。
func getJSON(ctx context.Context, do Doer, to string) ([]byte, int, error) {
	return doRequest(ctx, do, http.MethodGet, to, "", nil, nil)
}

// postForm POST 一个表单，返回响应体与状态码。
func postForm(ctx context.Context, do Doer, to string, form url.Values, headers map[string]string) ([]byte, int, error) {
	return doRequest(ctx, do, http.MethodPost, to, "application/x-www-form-urlencoded", []byte(form.Encode()), headers)
}

func doRequest(ctx context.Context, do Doer, method, to, ctype string, body []byte, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, to, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("构造请求失败: %w", err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("User-Agent", "strm115/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := do.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, nil
}

// secretPatterns 日志脱敏：URL / 请求里的凭据不要写进日志文件。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(access_token|corpsecret|secret|token|sign|key)=([^&\s"']+)`),
}

// sanitize 把敏感查询参数打码。
func sanitize(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, "${1}=***")
	}
	return s
}

// truncate 按 rune 截断，避免切出半个汉字。
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	return sanitize(err.Error())
}

// sleepCtx 可被 ctx 取消的等待，返回 false 表示 ctx 已结束。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// jsonInner 转成 JSON 字符串字面量并去掉两头引号，用于模板填充。
func jsonInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// fillTemplate 把 {{name}} 替换成转义后的值。
func fillTemplate(tpl string, fields map[string]string, esc func(string) string) string {
	pairs := make([]string, 0, len(fields)*2)
	for k, v := range fields {
		pairs = append(pairs, "{{"+k+"}}", esc(v))
	}
	return strings.NewReplacer(pairs...).Replace(tpl)
}

// apiError 从 {"errcode":..,"errmsg":..} 结构里提取错误。
func apiError(body []byte, status int) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("HTTP %d: %s", status, sanitize(truncate(strings.TrimSpace(string(body)), 160)))
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	if r.ErrCode != 0 {
		return fmt.Errorf("errcode %d: %s", r.ErrCode, r.ErrMsg)
	}
	if r.Code != 0 {
		return fmt.Errorf("code %d: %s", r.Code, r.Msg)
	}
	return nil
}

// rateLimiter 串行限速：保证相邻两次发送间隔不小于 gap。
type rateLimiter struct {
	mu   sync.Mutex
	last time.Time
	gap  time.Duration
}

func newRateLimiter(gap time.Duration) *rateLimiter { return &rateLimiter{gap: gap} }

func (l *rateLimiter) wait(ctx context.Context, gap time.Duration) error {
	if gap <= 0 {
		gap = 5 * time.Second
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := gap - time.Since(l.last); w > 0 {
		t := time.NewTimer(w)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	l.last = time.Now()
	return nil
}

var iyuuLimiter = newRateLimiter(5 * time.Second)
