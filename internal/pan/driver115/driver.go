// Package driver115 把 SheltonZhu/115driver 适配成本项目的 pan.Driver。
//
// 这一层只做三件事：
//  1. 字段翻译（driver.File → pan.Entry）
//  2. 错误归类（把 115 的中文报错归成 pan.ErrRisk / ErrAuth / ErrTransient …）
//  3. 凭证的存取格式（JSON）
//
// 不在这里做节流、重试、并发控制——那些归 pan.Manager，
// 否则每加一个网盘都要重写一遍限流逻辑。
package driver115

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"strm115/internal/pan"

	driver "github.com/SheltonZhu/115driver/pkg/driver"
)

const Kind = "115"

// 115 对 UA 很敏感：网页版 cookie 用网页 UA，客户端 UA 可能被要求重新校验。
// 这里取一个稳定的 Chrome UA，浏览器 UA 在 115 侧最不容易触发二次验证。
const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

func init() { pan.Register(Kind, Open) }

// Open 由 pan.Open 调用。
func Open(ctx context.Context, acct pan.Account) (pan.Driver, error) {
	var cr driver.Credential
	if err := json.Unmarshal([]byte(acct.Credential), &cr); err != nil {
		return nil, pan.NewErr(pan.ErrAuth, "打开账号", "凭证格式错误", err)
	}
	if cr.UID == "" || cr.CID == "" || cr.SEID == "" {
		return nil, pan.NewErr(pan.ErrAuth, "打开账号", "凭证缺少 UID/CID/SEID，请重新登录", nil)
	}
	c := driver.New(driver.UA(ua))
	c.ImportCredential(&cr)
	return &Client{c: c}, nil
}

type Client struct {
	c *driver.Pan115Client

	// 离线下载要的 sign/time 与 uid，都按账号缓存一次（见 offline.go）。
	mu   sync.Mutex
	auth offlineAuth
	uid  int64
}

func (c *Client) Kind() string { return Kind }

func (c *Client) Close() error {
	// driver 没有需要显式关闭的资源；保留接口以便将来加连接池清理
	c.c.Client.GetClient().CloseIdleConnections()
	return nil
}

func (c *Client) ListPage(ctx context.Context, dirID string, offset, limit int) ([]pan.Entry, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	files, err := c.c.ListPage(dirID, int64(offset), int64(limit))
	if err != nil {
		return nil, 0, classify("列目录", err)
	}
	out := make([]pan.Entry, 0, len(*files))
	for i := range *files {
		out = append(out, toEntry(&(*files)[i]))
	}
	// driver 的 ListPage 不返回总数；返回 -1 表示「未知」，
	// 调用方靠「本页是否满」判断还要不要继续翻。
	total := -1
	return out, total, nil
}

func (c *Client) Stat(ctx context.Context, id string) (pan.Entry, error) {
	f, err := c.c.GetFile(id)
	if err != nil {
		return pan.Entry{}, classify("查条目", err)
	}
	if f == nil {
		return pan.Entry{}, pan.NewErr(pan.ErrNotFound, "查条目", "返回空", nil)
	}
	return toEntry(f), nil
}

func (c *Client) DownloadURL(ctx context.Context, pickCode, userAgent string) (string, error) {
	if pickCode == "" {
		return "", pan.NewErr(pan.ErrNotFound, "取直链", "pickcode 为空", nil)
	}
	// 空 UA 必须原样透传，**不要**兜底成 ua 常量。
	//
	// 115 的 CDN 直链是按「申请直链时带的 UA」签发的：签名对不上就是
	// 403 invalid signature。而 Emby 的 HttpClient 抓 .strm 时压根不发 UA。
	// 以前这里把空 UA 换成 Chrome 的 UA，等于签了一张客户端永远出示不了的票——
	// 结果是 Emby 网页和客户端全都播不了，日志里只有一句 Forbidden。
	//
	// 115driver 的 DownloadWithUA(pickCode, "") 会走 applyEmptyUAHandling，
	// 真的一个 UA 头都不发（issue #80），这样签出来的链接和不发 UA 的客户端才对得上。
	info, err := c.c.DownloadWithUA(pickCode, userAgent)
	if err != nil {
		return "", classify("取直链", err)
	}
	if info == nil || info.Url.Url == "" {
		return "", pan.NewErr(pan.ErrNotFound, "取直链", "115 未返回下载地址（文件可能已删除或违规）", nil)
	}
	return info.Url.Url, nil
}

func (c *Client) Mkdir(ctx context.Context, parentID, name string) (string, error) {
	id, err := c.c.Mkdir(parentID, name)
	if err != nil {
		return "", classify("建目录", err)
	}
	return id, nil
}

func (c *Client) Move(ctx context.Context, toDirID string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return classify("移动", c.c.Move(toDirID, ids...))
}

func (c *Client) Rename(ctx context.Context, id, name string) error {
	return classify("改名", c.c.Rename(id, name))
}

func (c *Client) Remove(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return classify("删除", c.c.Delete(ids...))
}

func (c *Client) Ping(ctx context.Context) error {
	// CookieCheck 只查状态码，不会踢掉其它设备。不要换成 LoginCheck。
	return classify("校验凭证", c.c.CookieCheck())
}

// 离线下载相关实现见 offline.go。

func toEntry(f *driver.File) pan.Entry {
	e := pan.Entry{
		ID:       f.FileID,
		ParentID: f.ParentID,
		Name:     f.Name,
		IsDir:    f.IsDirectory,
		Size:     f.Size,
		PickCode: f.PickCode,
		SHA1:     strings.ToLower(f.Sha1),
	}
	if !f.UpdateTime.IsZero() {
		e.ModTime = f.UpdateTime
	} else {
		e.ModTime = f.CreateTime
	}
	return e
}

// ==================== 错误归类 ====================
//
// 115 的错误大多数是中文文案，没有稳定错误码。这里的匹配串是长久观察下来的特征词，
// 改之前请先确认：把风控判成普通错误会导致自动重试 → 风控窗口变长 → 封号。

func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	switch {
	case errors.Is(err, driver.ErrBadCookie),
		errors.Is(err, driver.ErrNotLogin),
		errors.Is(err, driver.ErrCredentialInvalid),
		errors.Is(err, driver.ErrSessionExited),
		errors.Is(err, driver.ErrDoesLoggedOut):
		return pan.NewErr(pan.ErrAuth, op, "登录已失效，请重新扫码或更新 Cookie", err)

	case errors.Is(err, driver.ErrNotExist),
		errors.Is(err, driver.ErrPickCodeNotExist),
		errors.Is(err, driver.ErrDownloadFileNotExistOrHasDeleted):
		return pan.NewErr(pan.ErrNotFound, op, "目标不存在", err)

	// —— 风控特征 ——
	case strings.Contains(msg, "服务器开小差"),
		strings.Contains(msg, "访问过于频繁"),
		strings.Contains(msg, "操作频繁"),
		strings.Contains(msg, "请稍后再试"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(low, "rate limit"):
		return pan.NewErr(pan.ErrRisk, op, "被 115 限流："+msg, err)

	// —— 临时故障特征 ——
	case strings.Contains(low, "timeout"),
		strings.Contains(low, "connection reset"),
		strings.Contains(low, "eof"),
		strings.Contains(low, "no such host"),
		strings.Contains(low, "tls"),
		strings.Contains(msg, "系统繁忙"),
		strings.Contains(msg, "网络"):
		return pan.NewErr(pan.ErrTransient, op, msg, err)
	}

	if strings.Contains(msg, "405") || strings.Contains(msg, "500") ||
		strings.Contains(msg, "502") || strings.Contains(msg, "503") {
		return pan.NewErr(pan.ErrTransient, op, msg, err)
	}

	// 认不出来的按临时故障处理：会重试，但也会在 MaxAttempts 后停下。
	return pan.NewErr(pan.ErrTransient, op, msg, err)
}

// ==================== 登录 ====================

// CredentialJSON 是从 115 拿到的凭证序列化后的形式，存库用。
func credentialJSON(c *driver.Credential) (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

// QRLogin 是一次扫码登录会话。
type QRLogin struct {
	ID        string
	PNG       []byte
	CreatedAt time.Time

	client *driver.Pan115Client
	sess   *driver.QRCodeSession
	logged bool
}

// StartQRLogin 申请二维码。
func StartQRLogin() (*QRLogin, error) {
	c := driver.New(driver.UA(ua))
	s, err := c.QRCodeStart()
	if err != nil {
		return nil, classify("申请二维码", err)
	}
	png, err := s.QRCode()
	if err != nil {
		return nil, pan.NewErr(pan.ErrTransient, "生成二维码图片", err.Error(), err)
	}
	return &QRLogin{
		PNG:       png,
		CreatedAt: time.Now(),
		client:    c,
		sess:      s,
	}, nil
}

// QRStatus 是给前端的状态值。
type QRStatus string

const (
	QRWaiting  QRStatus = "waiting"  // 未扫码
	QRScanned  QRStatus = "scanned"  // 已扫码，待确认
	QRAllowed  QRStatus = "allowed"  // 已确认 —— 可以换取凭证
	QRExpired  QRStatus = "expired"  // 过期
	QRCanceled QRStatus = "canceled" // 用户取消
)

// Poll 查一次扫码状态。status 为 QRAllowed 时返回凭证 JSON（只会返回一次）。
func (q *QRLogin) Poll(ctx context.Context) (QRStatus, string, string, error) {
	st, err := q.client.QRCodeStatus(q.sess)
	if err != nil {
		return "", "", "", classify("查询扫码状态", err)
	}
	switch {
	case st.IsWaiting():
		return QRWaiting, "等待扫码", "", nil
	case st.IsScanned():
		return QRScanned, "已扫码，请在手机上确认登录", "", nil
	case st.IsExpired():
		return QRExpired, "二维码已过期，请刷新", "", nil
	case st.IsCanceled():
		return QRCanceled, "已取消登录", "", nil
	case st.IsAllowed():
		if q.logged {
			return QRAllowed, "已登录", "", nil
		}
		cr, err := q.client.QRCodeLogin(q.sess)
		if err != nil {
			return "", "", "", classify("扫码登录", err)
		}
		q.logged = true
		js, err := credentialJSON(cr)
		if err != nil {
			return "", "", "", pan.NewErr(pan.ErrTransient, "序列化凭证", err.Error(), err)
		}
		return QRAllowed, "登录成功", js, nil
	}
	return QRWaiting, st.Msg, "", nil
}

// LoginWithCookie 用浏览器复制出来的 Cookie 登录。
// 接受的格式：`UID=xx; CID=yy; SEID=zz; KID=ww`（分号或换行分隔均可）。
func LoginWithCookie(ctx context.Context, raw string) (cred string, userName string, err error) {
	cookie := normalizeCookie(raw)
	var cr driver.Credential
	if err := cr.FromCookie(cookie); err != nil {
		return "", "", pan.NewErr(pan.ErrAuth, "解析 Cookie", "缺少 UID / CID / SEID 字段", err)
	}
	c := driver.New(driver.UA(ua))
	c.ImportCredential(&cr)
	if err := c.CookieCheck(); err != nil {
		return "", "", classify("校验 Cookie", err)
	}
	// 顺手取一次用户名，失败不影响登录（只是显示名）
	name := ""
	if u, uerr := c.GetUser(); uerr == nil && u != nil {
		name = u.UserName
	}
	js, err := credentialJSON(&cr)
	return js, name, err
}

// normalizeCookie 容忍常见的复制粘贴噪声：换行、多余空格、URL 编码。
func normalizeCookie(raw string) string {
	raw = strings.ReplaceAll(raw, "\r", "")
	raw = strings.ReplaceAll(raw, "\n", ";")
	raw = strings.ReplaceAll(raw, " ", "")
	raw = strings.ReplaceAll(raw, "％", "%")
	parts := []string{}
	for _, p := range strings.Split(raw, ";") {
		p = strings.TrimSpace(p)
		if p == "" || !strings.Contains(p, "=") {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// Verify 检查一份凭证现在是否还能用。
func Verify(ctx context.Context, credJSON string) (string, error) {
	d, err := Open(ctx, pan.Account{Kind: Kind, Credential: credJSON})
	if err != nil {
		return "", err
	}
	defer d.Close()
	c := d.(*Client)
	if err := c.c.CookieCheck(); err != nil {
		return "", classify("校验凭证", err)
	}
	if u, err := c.c.GetUser(); err == nil && u != nil {
		return u.UserName, nil
	}
	return "", nil
}
