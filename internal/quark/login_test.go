package quark

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 网页扫码登录：取 token → 画二维码 → 轮询拿 ticket → 换 Cookie。
// 真机流程要用户拿手机扫，这里用假网关把时序钉住。
func TestWebQRLoginFullFlow(t *testing.T) {
	var polls atomic.Int32
	var mu sync.Mutex
	var startRID string
	var pollRIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getTokenForQrcodeLogin"):
			if r.URL.Query().Get("client_id") != "532" {
				t.Errorf("client_id 不对: %s", r.URL.RawQuery)
			}
			if r.Header.Get("Origin") != "https://pan.quark.cn" {
				t.Errorf("取 token 缺 Origin 头")
			}
			mu.Lock()
			startRID = r.URL.Query().Get("request_id")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":2000000,"data":{"members":{"token":"QRTOKEN"}}}`))
		case strings.HasSuffix(r.URL.Path, "/getServiceTicketByQrcodeToken"):
			if r.URL.Query().Get("token") != "QRTOKEN" {
				t.Errorf("轮询没带上 token: %s", r.URL.RawQuery)
			}
			mu.Lock()
			pollRIDs = append(pollRIDs, r.URL.Query().Get("request_id"))
			mu.Unlock()
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"status":50004001,"message":"Query result is empty"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":2000000,"data":{"members":{"service_ticket":"ST"}}}`))
		case strings.HasSuffix(r.URL.Path, "/account/info"):
			if r.URL.Query().Get("st") != "ST" || r.URL.Query().Get("lw") != "scan" {
				t.Errorf("换 Cookie 的参数不对: %s", r.URL.RawQuery)
			}
			http.SetCookie(w, &http.Cookie{Name: "__pus", Value: "P", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "S", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "__uid", Value: "U", Path: "/"})
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldCAS, oldScan, oldInfo := quarkCASBase, quarkScanURL, quarkAccountInfo
	quarkCASBase = srv.URL + "/cas/ajax"
	quarkScanURL = srv.URL + "/scan"
	quarkAccountInfo = srv.URL + "/account/info"
	t.Cleanup(func() {
		quarkCASBase, quarkScanURL, quarkAccountInfo = oldCAS, oldScan, oldInfo
	})

	q := NewQRLogin()
	qrURL, err := q.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 官方二维码内容只带 token；多带 client_id / ssb 会让夸克 App 走已下线的入口。
	if !strings.HasPrefix(qrURL, srv.URL+"/scan?token=QRTOKEN") {
		t.Errorf("二维码 URL 不对: %s", qrURL)
	}
	if strings.Contains(qrURL, "client_id") || strings.Contains(qrURL, "ssb") {
		t.Errorf("二维码 URL 带了多余参数: %s", qrURL)
	}

	if _, done, err := q.Poll(t.Context()); err != nil || done {
		t.Fatalf("第一次该是「等待扫码」: done=%v err=%v", done, err)
	}
	cookie, done, err := q.Poll(t.Context())
	if err != nil || !done {
		t.Fatalf("第二次该扫码成功: done=%v err=%v", done, err)
	}
	for _, k := range []string{"__pus=P", "__puus=S", "__uid=U"} {
		if !strings.Contains(cookie, k) {
			t.Errorf("Cookie 缺 %s: %s", k, cookie)
		}
	}

	// 最关键的协议约束：request_id 生成一次、全程复用。
	mu.Lock()
	defer mu.Unlock()
	if startRID == "" {
		t.Fatal("取 token 没带 request_id")
	}
	if len(pollRIDs) != 2 {
		t.Fatalf("轮询次数 = %d，期望 2", len(pollRIDs))
	}
	for i, rid := range pollRIDs {
		if rid != startRID {
			t.Errorf("第 %d 次轮询的 request_id=%q，与取 token 时的 %q 不一致", i+1, rid, startRID)
		}
	}
}

// 实测：未扫码是 50004001 / Query result is empty（继续等），
// 约 130 秒后变 50004002 / Token Not Found（必须换码）。
// 以前两者都被当成「继续等待」，用户就一直盯着一张废码，手机上报「登录请求过期」。
func TestWebQRLoginDetectsExpired(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getTokenForQrcodeLogin"):
			_, _ = w.Write([]byte(`{"status":2000000,"data":{"members":{"token":"T"}}}`))
		case strings.HasSuffix(r.URL.Path, "/getServiceTicketByQrcodeToken"):
			switch polls.Add(1) {
			case 1:
				_, _ = w.Write([]byte(`{"status":50004001,"message":"Query result is empty"}`))
			case 2:
				_, _ = w.Write([]byte(`{"status":50004002,"message":"Token Not Found"}`))
			default:
				_, _ = w.Write([]byte(`{"status":50001002,"message":"登录请求过期"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldCAS, oldScan, oldInfo := quarkCASBase, quarkScanURL, quarkAccountInfo
	quarkCASBase, quarkScanURL, quarkAccountInfo = srv.URL+"/cas/ajax", srv.URL+"/scan", srv.URL+"/account/info"
	t.Cleanup(func() { quarkCASBase, quarkScanURL, quarkAccountInfo = oldCAS, oldScan, oldInfo })

	q := NewQRLogin()
	if _, err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, done, err := q.Poll(t.Context()); err != nil || done {
		t.Fatalf("50004001 是正常等待，不该判过期: done=%v err=%v", done, err)
	}
	if _, done, err := q.Poll(t.Context()); !errors.Is(err, ErrQRExpired) || done {
		t.Fatalf("50004002 要判过期并换码: done=%v err=%v", done, err)
	}
	if _, _, err := q.Poll(t.Context()); !errors.Is(err, ErrQRExpired) {
		t.Fatalf("中文「登录请求过期」也要判出来: %v", err)
	}
}

func TestWebQRLoginRejectsAnonymousCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getTokenForQrcodeLogin"):
			_, _ = w.Write([]byte(`{"data":{"members":{"token":"T"}}}`))
		case strings.HasSuffix(r.URL.Path, "/getServiceTicketByQrcodeToken"):
			_, _ = w.Write([]byte(`{"status":2000000,"data":{"members":{"service_ticket":"ST"}}}`))
		default:
			// 只给一个无关的 cookie，不该被当成登录态存进账号。
			http.SetCookie(w, &http.Cookie{Name: "traceid", Value: "x", Path: "/"})
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	oldCAS, oldScan, oldInfo := quarkCASBase, quarkScanURL, quarkAccountInfo
	quarkCASBase, quarkScanURL, quarkAccountInfo = srv.URL+"/cas/ajax", srv.URL+"/scan", srv.URL+"/account/info"
	t.Cleanup(func() { quarkCASBase, quarkScanURL, quarkAccountInfo = oldCAS, oldScan, oldInfo })

	q := NewQRLogin()
	if _, err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, done, err := q.Poll(t.Context()); err == nil || done {
		t.Errorf("匿名 Cookie 必须报错，实际 done=%v err=%v", done, err)
	}
}

// 接口改版/风控时不能拿一个空 token 去画码，得把上游 message 带出来。
func TestWebQRStartReportsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":50001001,"message":"请求太频繁"}`))
	}))
	defer srv.Close()
	old := quarkCASBase
	quarkCASBase = srv.URL + "/cas/ajax"
	t.Cleanup(func() { quarkCASBase = old })

	if _, err := NewQRLogin().Start(t.Context()); err == nil || !strings.Contains(err.Error(), "请求太频繁") {
		t.Fatalf("该把上游错误透出来，实际 %v", err)
	}
}
