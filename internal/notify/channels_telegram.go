package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// telegramChannel Telegram Bot API（sendMessage / sendPhoto）。
// 代理由 Config.ProxyURL 统一注入 Doer，无需单独配置。
type telegramChannel struct{}

func (telegramChannel) Name() string { return "telegram" }

func (telegramChannel) Enabled(cfg *Config) bool {
	return cfg.TG.Enabled.On() && cfg.TG.Token != "" && cfg.TG.ChatID != ""
}

// telegramAPIBase 是 Bot API 根地址。抽成变量只为了测试能指向本地假服务器；
// 生产环境不要改。
var telegramAPIBase = "https://api.telegram.org"

var tgHTMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// photoUploadTimeout 是图片上传的最短时限。文本仍用配置里的超时（默认 15s），
// 只有传图这一条放宽：多花十几秒能换来「封面稳定带上」。
const photoUploadTimeout = 45 * time.Second

// withTimeout 给 Doer 放宽超时。
//
// 只在拿到 *http.Client 时生效（生产与测试都是），其它实现原样返回 ——
// 拿不到具体类型就不该乱猜它的超时语义。
func withTimeout(do Doer, d time.Duration) Doer {
	hc, ok := do.(*http.Client)
	if !ok || (hc.Timeout != 0 && hc.Timeout >= d) {
		return do
	}
	cp := *hc
	cp.Timeout = d
	return &cp
}

func (ch telegramChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	to := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase, cfg.TG.Token)
	payload := map[string]any{
		"chat_id":    cfg.TG.ChatID,
		"text":       tgHTMLEscaper.Replace(m.Text()),
		"parse_mode": "HTML",
	}
	body, status, err := postJSON(ctx, do, to, payload, nil)
	if err != nil {
		return fmt.Errorf("telegram: %s", sanitizeErr(err))
	}
	return tgResult(body, status)
}

// SendRich 发图（caption 带正文），失败退回纯文本。
//
// 两种图源：ImageFile 是本地文件（multipart 直传，家用部署唯一可行的那条），
// Image 是公网 URL（sendPhoto 让 TG 自己去拉）。本地文件优先 —— 它不依赖
// 任何外网可达性，读不出来才退到 URL，都不行就发纯文本。
//
// 退回纯文本时返回 *FallbackError 而不是 nil：正文用户确实收到了，
// 所以不算失败；但「图为什么没发出去」必须能被日志记下来。
func (ch telegramChannel) SendRich(ctx context.Context, cfg *Config, do Doer, m Message) error {
	if m.ImageFile == "" && m.Image == "" {
		return ch.Send(ctx, cfg, do, m)
	}
	plain := Message{Kind: m.Kind, Title: m.Title, Content: m.Content, Force: m.Force}
	if m.ImageFile != "" {
		if err := ch.sendPhotoFile(ctx, cfg, do, m); err == nil {
			return nil
		} else if m.Image == "" {
			if ferr := ch.Send(ctx, cfg, do, plain); ferr != nil {
				return ferr
			}
			return &FallbackError{Reason: err.Error()}
		}
	}
	to := fmt.Sprintf("%s/bot%s/sendPhoto", telegramAPIBase, cfg.TG.Token)
	form := url.Values{
		"chat_id": {cfg.TG.ChatID},
		"photo":   {m.Image},
		"caption": {truncate(tgHTMLEscaper.Replace(m.Text()), 1000)},
	}
	body, status, err := postForm(ctx, do, to, form, nil)
	if err == nil && tgResult(body, status) == nil {
		return nil
	}
	if ferr := ch.Send(ctx, cfg, do, plain); ferr != nil {
		return ferr
	}
	reason := "公网海报地址发不出去"
	if err != nil {
		reason = err.Error()
	}
	return &FallbackError{Reason: reason}
}

// sendPhotoFile 把本地图片作为 multipart 上传到 sendPhoto。
//
// 为什么不一律走 URL：海报在 NAS 的磁盘上，没有公网地址；就算有，也多半是
// 带登录态的管理端接口，TG 的服务器拉不到。直接上传字节是唯一稳的路子。
func (ch telegramChannel) sendPhotoFile(ctx context.Context, cfg *Config, do Doer, m Message) error {
	// 上传比发文本慢得多：慢速国际线路上，默认 15 秒常常不够（实测有 31KB
	// 的海报也会超时，于是卡片无声退回纯文本）。这里只给上传放宽时限，
	// 失败得更慢一点总比「封面时有时无」好排查。
	do = withTimeout(do, photoUploadTimeout)

	f, err := os.Open(m.ImageFile)
	if err != nil {
		return fmt.Errorf("telegram: 读不到封面")
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", cfg.TG.ChatID)
	_ = w.WriteField("parse_mode", "HTML")
	_ = w.WriteField("caption", truncate(tgHTMLEscaper.Replace(m.Text()), 1000))
	part, err := w.CreateFormFile("photo", filepath.Base(m.ImageFile))
	if err != nil {
		return fmt.Errorf("telegram: 组装上传失败")
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("telegram: 读封面失败")
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("telegram: 组装上传失败")
	}

	to := fmt.Sprintf("%s/bot%s/sendPhoto", telegramAPIBase, cfg.TG.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, &buf)
	if err != nil {
		return fmt.Errorf("telegram: 构造请求失败")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", "strm115/1.0")
	resp, err := do.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return tgResult(body, resp.StatusCode)
}

func tgResult(body []byte, status int) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("telegram HTTP %d: %s", status, truncate(strings.TrimSpace(string(body)), 160))
	}
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if json.Unmarshal(body, &r) == nil && !r.OK {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	return nil
}
