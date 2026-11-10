package watchhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"strm115/internal/netproxy"
)

// 默认上游。WatchHub 是 Stremio 插件，Cinemeta 是它的元数据来源，
// 两者都是公开接口，不需要 key。
const (
	DefaultAddonBase = "https://watchhub.strem.io"
	DefaultMetaBase  = "https://v3-cinemeta.strem.io"
	userAgent        = "strm115/1.0 (Go)"
)

// Client 是 WatchHub / Cinemeta 的只读客户端，可安全并发使用。
//
// manifest 只拉一次并缓存：它的内容（支持的 types、idPrefixes）是所有查询的
// 前提，反复拉没有意义，还会让上游多一次请求。
type Client struct {
	httpClient *http.Client
	retries    int
	addonBase  string
	metaBase   string

	mu       sync.Mutex
	manifest *Manifest
}

// NewClient 用默认上游和 20 秒超时创建客户端。
func NewClient() *Client {
	return NewClientWith(DefaultAddonBase, DefaultMetaBase)
}

// NewClientWith 指定上游地址（自建镜像 / 测试用）。
func NewClientWith(addonBase, metaBase string) *Client {
	if strings.TrimSpace(addonBase) == "" {
		addonBase = DefaultAddonBase
	}
	if strings.TrimSpace(metaBase) == "" {
		metaBase = DefaultMetaBase
	}
	return &Client{
		httpClient: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: netproxy.URL}},
		retries:    2,
		addonBase:  strings.TrimRight(addonBase, "/"),
		metaBase:   strings.TrimRight(metaBase, "/"),
	}
}

// get 返回响应体与 Content-Type；4xx（除 408/429）不重试，5xx/网络错误会重试。
func (c *Client) get(ctx context.Context, url string) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "*/*")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		contentType := resp.Header.Get("Content-Type")
		_ = resp.Body.Close()

		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
				resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
				return nil, "", lastErr
			}
			continue
		}
		return body, contentType, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("请求失败: %s", url)
	}
	return nil, "", lastErr
}

// GetJSON 取一个 JSON 接口并解码到 dst。
func (c *Client) GetJSON(ctx context.Context, url string, dst any) error {
	body, _, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return fmt.Errorf("解析 JSON 失败 (%s): %w；响应开头 %q", url, err, preview)
	}
	return nil
}
