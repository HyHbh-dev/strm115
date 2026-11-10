package notify

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// iyuuBaseURL 抽成变量便于单元测试替换成 httptest 服务。
var iyuuBaseURL = "https://iyuu.cn"

// iyuuChannel IYUU（爱语飞飞）微信公众号推送。
// IYUU 连发会被拒，因此全局限速（默认 5 秒一条）；按 Types 勾选过滤。
type iyuuChannel struct{}

func (iyuuChannel) Name() string { return "iyuu" }

func (iyuuChannel) Enabled(cfg *Config) bool {
	return cfg.Iyuu.Enabled.On() && strings.TrimSpace(cfg.Iyuu.Token) != ""
}

func (iyuuChannel) Send(ctx context.Context, cfg *Config, do Doer, m Message) error {
	if !m.Force && !kindWanted(cfg.Iyuu.Types, m.Kind) {
		return errSkip
	}
	gap := 5 * time.Second
	if cfg.IyuuGapSec > 0 {
		gap = time.Duration(cfg.IyuuGapSec) * time.Second
	}
	if err := iyuuLimiter.wait(ctx, gap); err != nil {
		return err
	}
	title, desp := strings.TrimSpace(m.Title), strings.TrimSpace(m.Content)
	if title == "" {
		// 标题空则拿正文首行当标题，否则微信里只看到一条空通知。
		title = firstLine(desp)
		desp = strings.TrimSpace(strings.TrimPrefix(desp, firstLine(desp)))
	}
	form := url.Values{"text": {title}, "desp": {desp}}
	to := iyuuBaseURL + "/" + url.PathEscape(strings.TrimSpace(cfg.Iyuu.Token)) + ".send"
	body, status, err := postForm(ctx, do, to, form, nil)
	if err != nil {
		return fmt.Errorf("iyuu: %w", err)
	}
	if e := apiError(body, status); e != nil {
		return fmt.Errorf("iyuu: %w", e)
	}
	return nil
}
