package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiClient 是 strm115 管理后台的极小客户端。
//
// 为什么走 HTTP API 而不是直接读写 SQLite：走接口意味着 bootstrap 做的一切
// 和用户在界面上点的是**同一条路径**，将来接口改了、加了校验，bootstrap 立刻
// 跟着一起失败，不会出现「库里数据合法但业务不认」的隐蔽错乱。
type apiClient struct {
	base string
	hc   *http.Client
	log  *slog.Logger
}

func newAPIClient(base string, log *slog.Logger) *apiClient {
	jar, _ := cookiejar.New(nil)
	return &apiClient{base: base, hc: &http.Client{Jar: jar, Timeout: 60 * time.Second}, log: log}
}

var errUnauthorized = errors.New("管理后台鉴权失败")

func (c *apiClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 %s %s 失败: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return fmt.Errorf("%s %s 返回 %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析 %s 响应失败: %w", path, err)
		}
	}
	return nil
}

// WaitReady 轮询 /api/session，等管理后台起来。
func (c *apiClient) WaitReady(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var s map[string]any
		if err := c.call(ctx, http.MethodGet, "/api/session", nil, &s); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 strm115 管理后台就绪超时（%s）", wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Session 读后台状态，用来区分「没登录」和「还没初始化」。
func (c *apiClient) Session(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.call(ctx, http.MethodGet, "/api/session", nil, &out)
	return out, err
}

// Login 用管理员账号密码换会话 cookie。
//
// 这里刻意不做「密码为空就跳过」的判断：后台现在默认就需要登录，
// 跳过只会让后面每个接口都返回 401，错误信息反而更难懂。
func (c *apiClient) Login(ctx context.Context, user, password string) error {
	err := c.call(ctx, http.MethodPost, "/api/login",
		map[string]string{"username": user, "password": password}, nil)
	if err == nil {
		return nil
	}
	if errors.Is(err, errUnauthorized) {
		if s, serr := c.Session(ctx); serr == nil {
			if need, _ := s["setup_needed"].(bool); need {
				return fmt.Errorf("管理后台还没初始化：请先打开 http://<宿主机IP>:6060 设置管理员账号密码，再重跑 bootstrap")
			}
		}
		return fmt.Errorf("登录管理后台失败：ADMIN_USER / ADMIN_PASSWORD 与管理后台里设置的不一致（账号密码可在网页「设置 → 管理员账号」里改）")
	}
	return err
}

func (c *apiClient) Settings(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	err := c.call(ctx, http.MethodGet, "/api/settings", nil, &out)
	return out, err
}

func (c *apiClient) PutSettings(ctx context.Context, kv map[string]string) error {
	return c.call(ctx, http.MethodPut, "/api/settings", kv, nil)
}

type account struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (c *apiClient) Accounts(ctx context.Context) ([]account, error) {
	var out []account
	err := c.call(ctx, http.MethodGet, "/api/accounts", nil, &out)
	return out, err
}

// CookieLogin 用 115 的 Cookie 加一个账号。这是唯一不需要人工扫码的登录方式。
func (c *apiClient) CookieLogin(ctx context.Context, cookie, name string) (account, error) {
	var out account
	in := map[string]string{"cookie": cookie, "name": name}
	err := c.call(ctx, http.MethodPost, "/api/115/cookie", in, &out)
	return out, err
}

type library struct {
	ID             int64  `json:"id"`
	AccountID      int64  `json:"account_id"`
	Name           string `json:"name"`
	RemoteCID      string `json:"remote_cid"`
	LocalPath      string `json:"local_path"`
	CollectionType string `json:"collection_type"`
	ServerName     string `json:"server_name"`
	ServerLibID    string `json:"server_lib_id"`
}

func (c *apiClient) Libraries(ctx context.Context) ([]library, error) {
	var out []library
	err := c.call(ctx, http.MethodGet, "/api/libraries", nil, &out)
	return out, err
}

func (c *apiClient) CreateLibrary(ctx context.Context, in map[string]any) (library, error) {
	var out library
	err := c.call(ctx, http.MethodPost, "/api/libraries", in, &out)
	return out, err
}

type progress struct {
	LibraryID int64  `json:"library_id"`
	Phase     string `json:"phase"`
	Dirs      int    `json:"dirs"`
	Files     int    `json:"files"`
	Written   int    `json:"written"`
	Removed   int    `json:"removed"`
	Failed    int    `json:"failed"`
	Running   bool   `json:"running"`
	Message   string `json:"message"`
	Err       string `json:"err"`
}

func (c *apiClient) Job(ctx context.Context, libID int64) (progress, error) {
	var out progress
	err := c.call(ctx, http.MethodGet, "/api/libraries/"+strconv.FormatInt(libID, 10)+"/job", nil, &out)
	return out, err
}

func (c *apiClient) StartSync(ctx context.Context, libID int64) (progress, error) {
	var out progress
	path := "/api/libraries/" + strconv.FormatInt(libID, 10) + "/sync"
	err := c.call(ctx, http.MethodPost, path, map[string]any{}, &out)
	return out, err
}

// AttachServer 在 Emby 上建库并触发扫描。
func (c *apiClient) AttachServer(ctx context.Context, libID int64) (map[string]any, error) {
	var out map[string]any
	path := "/api/libraries/" + strconv.FormatInt(libID, 10) + "/server"
	err := c.call(ctx, http.MethodPost, path, map[string]any{}, &out)
	return out, err
}

func (c *apiClient) TestServer(ctx context.Context, kind, rawURL, key string) error {
	in := map[string]string{"kind": kind, "url": rawURL, "api_key": key}
	return c.call(ctx, http.MethodPost, "/api/settings/test-server", in, nil)
}

type browseEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// Browse 列网盘某个目录。cid 为空表示根目录。
func (c *apiClient) Browse(ctx context.Context, accountID int64, cid string) ([]browseEntry, error) {
	q := url.Values{}
	if cid != "" {
		q.Set("cid", cid)
	}
	var out struct {
		Entries []browseEntry `json:"entries"`
	}
	path := "/api/accounts/" + strconv.FormatInt(accountID, 10) + "/browse"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	return out.Entries, err
}

// ensure leading scheme，避免用户写 "192.168.1.10:6086" 时拼出非法 URL
func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	u = strings.TrimRight(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	if _, err := url.Parse(u); err != nil {
		return ""
	}
	return u
}
