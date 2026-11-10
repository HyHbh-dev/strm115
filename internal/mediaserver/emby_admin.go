package mediaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Admin 封装「首次部署」才需要的那几个 Emby 接口：完成启动向导、管理员登录、
// 创建 API Key。
//
// 为什么不并进 Server 接口：这三件事都是**一次性**的，而且鉴权方式完全不同
// —— 向导接口在完成之前免鉴权，登录要账号密码，日常建库/刷新用 API Key。
// 混在一起会让 Server 这个抽象带上永远用不到的方法。
//
// 所有接口行为都在 Emby 4.10.2 容器里实测过，注释里标了真实状态码。
type Admin struct {
	base string
	hc   *http.Client
}

// NormalizeBase 把用户输入的地址收拾成 base URL。
// 三种常见写法都要能吃下：http://host:8096、host:8096、http://host:8096/emby
func NormalizeBase(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, "/emby")
	if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	return u
}

func NewAdmin(rawURL string) *Admin {
	return &Admin{base: NormalizeBase(rawURL), hc: &http.Client{Timeout: 30 * time.Second}}
}

// clientHeader 是 Emby 要求的客户端标识。登录接口不认 X-Emby-Token，
// 只认这个头，缺了会返回 400 而不是 401 —— 报错信息很不直观。
const clientHeader = `MediaBrowser Client="strm115", Device="bootstrap", DeviceId="strm115-bootstrap", Version="1.0.0"`

type PublicInfo struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

// call 发一个请求，返回状态码与响应体。
//
// 特意不用「非 2xx 就返回 error」的写法：向导的判定逻辑本身就依赖具体状态码
// （200 = 向导未完成，401 = 已完成），提前把状态码变成错误就没法区分了。
func (a *Admin) call(ctx context.Context, method, path string, q url.Values, token string, in, out any) (int, []byte, error) {
	u := a.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	} else if method == http.MethodPost {
		// Emby 的部分 POST 接口不接受空 body 也要求 Content-Length，给个空 reader 最稳
		body = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Emby-Authorization", clientHeader)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Emby-Token", token)
	}

	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("连接 Emby 失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if out != nil && len(data) > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, data, fmt.Errorf("解析 Emby 响应失败: %w", err)
		}
	}
	return resp.StatusCode, data, nil
}

func embyErr(what string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if msg == "" {
		return fmt.Errorf("%s 失败（HTTP %d）", what, status)
	}
	return fmt.Errorf("%s 失败（HTTP %d）: %s", what, status, msg)
}

// PublicInfo 拿服务器信息。Emby 启动期间会拒绝连接，所以这个函数也是「探活」。
func (a *Admin) PublicInfo(ctx context.Context) (PublicInfo, error) {
	var info PublicInfo
	status, body, err := a.call(ctx, http.MethodGet, "/System/Info/Public", nil, "", nil, &info)
	if err != nil {
		return info, err
	}
	if status != http.StatusOK {
		return info, embyErr("读取 Emby 信息", status, body)
	}
	return info, nil
}

// WaitReady 等 Emby 起来。容器刚启动时 8096 会先接受连接再返回 502/503，
// 只看端口通不通不够，必须以 PublicInfo 成功为准。
func (a *Admin) WaitReady(ctx context.Context, wait time.Duration) (PublicInfo, error) {
	deadline := time.Now().Add(wait)
	var last error
	for {
		info, err := a.PublicInfo(ctx)
		if err == nil {
			return info, nil
		}
		last = err
		if time.Now().After(deadline) {
			return PublicInfo{}, fmt.Errorf("等待 Emby 就绪超时（%s）: %w", wait, last)
		}
		select {
		case <-ctx.Done():
			return PublicInfo{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// WizardCompleted 判断启动向导是否已经走过。
//
// 注意：Emby 4.10 的 /System/Info/Public **不返回** StartupWizardCompleted 字段
// （4.8 时代是返回的），所以不能靠它判断。这里用向导配置接口的状态码：
//
//	200 = 向导还能访问 → 未完成
//	401 = 已被鉴权挡住 → 已完成
//
// 两种情况都在真容器上验证过。
func (a *Admin) WizardCompleted(ctx context.Context) (bool, error) {
	status, body, err := a.call(ctx, http.MethodGet, "/Startup/Configuration", nil, "", nil, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return true, nil
	default:
		return false, embyErr("检查 Emby 启动向导状态", status, body)
	}
}

// CompleteWizard 走完启动向导四步，建出第一个管理员。
//
// 四步的实测结果：
//
//	POST /Startup/Configuration  → 204
//	POST /Startup/User           → 200 {"{}"}
//	POST /Startup/RemoteAccess   → 204
//	POST /Startup/Complete       → 204
func (a *Admin) CompleteWizard(ctx context.Context, user, pass, uiCulture, country string) error {
	if uiCulture == "" {
		uiCulture = "zh-CN"
	}
	if country == "" {
		country = "CN"
	}

	q := url.Values{}
	q.Set("UICulture", uiCulture)
	q.Set("MetadataCountryCode", country)
	q.Set("PreferredMetadataLanguage", uiCulture)
	if status, body, err := a.call(ctx, http.MethodPost, "/Startup/Configuration", q, "", nil, nil); err != nil {
		return err
	} else if status >= 300 {
		return embyErr("设置 Emby 向导配置", status, body)
	}

	q = url.Values{}
	q.Set("name", user)
	q.Set("password", pass)
	// 400 已存在用户时继续：说明有人抢在前面建了账号，后面登录能过就行
	if status, body, err := a.call(ctx, http.MethodPost, "/Startup/User", q, "", nil, nil); err != nil {
		return err
	} else if status >= 300 && status != http.StatusBadRequest {
		return embyErr("创建 Emby 管理员", status, body)
	}

	q = url.Values{}
	q.Set("EnableRemoteAccess", "true")
	q.Set("EnableAutomaticPortMapping", "false") // 别去动路由器 UPnP
	if status, body, err := a.call(ctx, http.MethodPost, "/Startup/RemoteAccess", q, "", nil, nil); err != nil {
		return err
	} else if status >= 300 {
		return embyErr("设置 Emby 远程访问", status, body)
	}

	if status, body, err := a.call(ctx, http.MethodPost, "/Startup/Complete", nil, "", nil, nil); err != nil {
		return err
	} else if status >= 300 {
		return embyErr("完成 Emby 启动向导", status, body)
	}
	return nil
}

// Authenticate 用管理员账号换一个访问 token。
// 刚完成向导时 Emby 可能还要一两秒才接受登录，调用方负责重试。
func (a *Admin) Authenticate(ctx context.Context, user, pass string) (string, error) {
	in := map[string]string{"Username": user, "Pw": pass}
	var out struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			ID string `json:"Id"`
		} `json:"User"`
	}
	status, body, err := a.call(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, "", in, &out)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", embyErr("登录 Emby", status, body)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("Emby 登录成功但没有返回 token")
	}
	return out.AccessToken, nil
}

type apiKey struct {
	AppName     string `json:"AppName"`
	AccessToken string `json:"AccessToken"`
}

// EnsureAPIKey 拿到一个名为 appName 的 API Key，没有就建一个。
//
// 关键坑：POST /Auth/Keys 返回 **204 且没有响应体**（实测），拿不到 key 本身，
// 必须再 GET /Auth/Keys 按 AppName 找回来。而且列表是 {"Items":[...]} 包装的，
// 不是裸数组。直接按文档猜测会写出「建了但没有 key」的代码。
func (a *Admin) EnsureAPIKey(ctx context.Context, adminToken, appName string) (string, error) {
	find := func() (string, error) {
		var list struct {
			Items []apiKey `json:"Items"`
		}
		status, body, err := a.call(ctx, http.MethodGet, "/Auth/Keys", nil, adminToken, nil, &list)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", embyErr("读取 Emby API Key 列表", status, body)
		}
		for _, k := range list.Items {
			if strings.EqualFold(k.AppName, appName) && k.AccessToken != "" {
				return k.AccessToken, nil
			}
		}
		return "", nil
	}

	// 先找：重复执行 bootstrap 时不能每次都新建一堆 key
	if key, err := find(); err != nil {
		return "", err
	} else if key != "" {
		return key, nil
	}

	q := url.Values{}
	q.Set("App", appName)
	status, body, err := a.call(ctx, http.MethodPost, "/Auth/Keys", q, adminToken, nil, nil)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", embyErr("创建 Emby API Key", status, body)
	}

	key, err := find()
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("已请求创建 API Key，但在列表里没找到名为 %q 的项", appName)
	}
	return key, nil
}
