package play

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"strm115/internal/pan"
)

func TestStripExt(t *testing.T) {
	cases := map[string]string{
		"abc123":        "abc123",
		"abc123.mkv":    "abc123.mkv", // 不是已知视频扩展名就原样保留
		"abc123def.MKV": "abc123def",  // 大小写不敏感
	}
	// mkv 在视频白名单里，必须被剥掉
	if got := stripExt("abc123.mkv"); got != "abc123" {
		t.Errorf("stripExt(abc123.mkv) = %q", got)
	}
	if got := stripExt("abc123"); got != "abc123" {
		t.Errorf("stripExt(abc123) = %q", got)
	}
	if got := stripExt("abc.xyz"); got != "abc.xyz" {
		t.Errorf("未知扩展名不该剥: %q", got)
	}
	_ = cases
}

func TestCacheKeyIncludesUA(t *testing.T) {
	c := newLinkCache(time.Minute)
	c.put("pc1", "UA-A", "http://cdn/a")
	if _, ok := c.get("pc1", "UA-A"); !ok {
		t.Fatal("同 UA 应命中")
	}
	if _, ok := c.get("pc1", "UA-B"); ok {
		t.Fatal("不同 UA 绝不能命中：115 直链按 UA 签发，复用会 403")
	}
}

func TestCacheExpiry(t *testing.T) {
	c := newLinkCache(time.Millisecond)
	c.put("pc", "ua", "http://cdn/x")
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.get("pc", "ua"); ok {
		t.Fatal("过期项不该命中")
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(2, time.Minute)
	if !l.allow("1.1.1.1") || !l.allow("1.1.1.1") {
		t.Fatal("前两次应放行")
	}
	if l.allow("1.1.1.1") {
		t.Fatal("第三次应被限流")
	}
	if !l.allow("2.2.2.2") {
		t.Fatal("别的 IP 不该受影响")
	}
}

func TestIPLimiterWindowReset(t *testing.T) {
	l := newIPLimiter(1, 20*time.Millisecond)
	l.allow("x")
	if l.allow("x") {
		t.Fatal("窗口内应限流")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.allow("x") {
		t.Fatal("窗口过后应恢复")
	}
}

// fakeDriver 只实现播放链路用到的那几个方法。
type fakeDriver struct {
	account int64
	lastUA  string
	calls   *[]int64
}

func (d *fakeDriver) Kind() string { return "fake" }
func (d *fakeDriver) ListPage(ctx context.Context, dirID string, offset, limit int) ([]pan.Entry, int, error) {
	return nil, 0, nil
}
func (d *fakeDriver) Stat(ctx context.Context, id string) (pan.Entry, error) {
	return pan.Entry{ID: id}, nil
}
func (d *fakeDriver) DownloadURL(ctx context.Context, pc, ua string) (string, error) {
	d.lastUA = ua
	return "https://cdn/" + pc + "/" + strconv.FormatInt(d.account, 10), nil
}
func (d *fakeDriver) Mkdir(ctx context.Context, parent, name string) (string, error) { return "", nil }
func (d *fakeDriver) Move(ctx context.Context, to string, ids ...string) error       { return nil }
func (d *fakeDriver) Rename(ctx context.Context, id, name string) error              { return nil }
func (d *fakeDriver) Remove(ctx context.Context, ids ...string) error                { return nil }
func (d *fakeDriver) Ping(ctx context.Context) error                                 { return nil }
func (d *fakeDriver) Close() error                                                   { return nil }

// 一个库挂两个网盘之后，播放地址必须能自己说明「这是哪家的文件」。
// 台帐命中时按台账；台账没有时按 .strm 里的 q/ 前缀选账号。
func TestQuarkPrefixRoutesToQuarkAccount(t *testing.T) {
	var opened []int64
	open := func(ctx context.Context, accountID int64) (*pan.Manager, error) {
		opened = append(opened, accountID)
		drv := &fakeDriver{account: accountID}
		return pan.NewManager(drv, pan.Account{ID: accountID, Kind: "fake"},
			pan.Options{MinGap: time.Millisecond, MaxAttempts: 1, Logf: func(string, ...any) {}}), nil
	}
	noLedger := func(ctx context.Context, pc string) (int64, bool) { return 0, false }
	byKind := func(ctx context.Context, kind string) (int64, bool) {
		if kind == "quarktv" {
			return 2, true
		}
		return 1, true
	}
	p := New(open, noLedger, byKind, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 夸克：/d/q/{fid}.mkv
	req := httptest.NewRequest(http.MethodGet, "/d/q/40fdec135a084516ac8d66ea19dddbb9.mkv", nil)
	req.Header.Set("User-Agent", "Emby/4.10.2")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("应 302，实际 %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/40fdec135a084516ac8d66ea19dddbb9/2") {
		t.Errorf("夸克地址应走账号 2，实际 %s", loc)
	}

	// 115：历史格式 /d/{pickcode}.mkv，没有前缀就走兜底账号
	req2 := httptest.NewRequest(http.MethodGet, "/d/bf4l2rr2j5vfa8s5l.mkv", nil)
	rec2 := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec2, req2)
	if loc := rec2.Header().Get("Location"); !strings.HasSuffix(loc, "/bf4l2rr2j5vfa8s5l/1") {
		t.Errorf("无前缀地址应走兜底账号 1，实际 %s", loc)
	}
	if len(opened) != 2 || opened[0] != 2 || opened[1] != 1 {
		t.Errorf("打开账号的顺序不对: %v", opened)
	}
}

// 台账命中优先于前缀：文件被转存过/换了账号时，台账才是事实。
func TestLedgerBeatsKindPrefix(t *testing.T) {
	open := func(ctx context.Context, accountID int64) (*pan.Manager, error) {
		drv := &fakeDriver{account: accountID}
		return pan.NewManager(drv, pan.Account{ID: accountID, Kind: "fake"},
			pan.Options{MinGap: time.Millisecond, MaxAttempts: 1, Logf: func(string, ...any) {}}), nil
	}
	ledger := func(ctx context.Context, pc string) (int64, bool) { return 7, pc == "fid" }
	byKind := func(ctx context.Context, kind string) (int64, bool) { return 2, true }
	p := New(open, ledger, byKind, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/d/q/fid.mkv", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/fid/7") {
		t.Errorf("台账命中时应用台账的账号，实际 %s", loc)
	}
}
