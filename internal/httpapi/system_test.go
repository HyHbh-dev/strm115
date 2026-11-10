package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 首页的容量卡片：115 走 space_info，夸克没配 Cookie 时如实说明，
// 不能因为一个网盘读不到就让整份系统状态报错。
func TestSystemStatusAccountQuotas(t *testing.T) {
	s, _ := newServerWithStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":true,"data":{"space_info":{
			"all_total":{"size":48395897114699.3,"size_format":"44.02TB"},
			"all_use":{"size":24513164855122,"size_format":"22.29TB"}}}}`))
	}))
	defer srv.Close()
	old := webapi115Base
	webapi115Base = srv.URL
	t.Cleanup(func() { webapi115Base = old })

	ctx := t.Context()
	if _, err := s.saveAccount(ctx, "115", "测试115",
		`{"UID":"u","CID":"c","SEID":"s","KID":"k"}`); err != nil {
		t.Fatal(err)
	}
	// 夸克账号故意不配 web_cookie。
	if _, err := s.saveAccount(ctx, quarkKind, "测试夸克",
		`{"refresh_token":"RT","device_id":"dev"}`); err != nil {
		t.Fatal(err)
	}

	out := s.collectSystem(ctx)
	if len(out.Accounts) != 2 {
		t.Fatalf("该有 2 个网盘账号，实际 %d: %+v", len(out.Accounts), out.Accounts)
	}
	var w115, wq *accountQuota
	for i := range out.Accounts {
		switch out.Accounts[i].Kind {
		case "115":
			w115 = &out.Accounts[i]
		case quarkKind:
			wq = &out.Accounts[i]
		}
	}
	if w115 == nil || !w115.OK || w115.Used != 24513164855122 || w115.Total != 48395897114699 {
		t.Errorf("115 容量不对: %+v", w115)
	}
	if wq == nil || wq.OK || wq.Total != 0 {
		t.Errorf("没配 Cookie 的夸克不该报容量: %+v", wq)
	}
	if wq != nil && wq.Note == "" {
		t.Error("夸克要说明为什么读不到容量")
	}
}

// 115 那边挂了也不能把整个首页拖垮：只在那一行写原因。
func TestQuota115FailureIsSoft(t *testing.T) {
	s, _ := newServerWithStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	old := webapi115Base
	webapi115Base = srv.URL
	t.Cleanup(func() { webapi115Base = old })

	ctx := t.Context()
	if _, err := s.saveAccount(ctx, "115", "测试115",
		`{"UID":"u","CID":"c","SEID":"s","KID":"k"}`); err != nil {
		t.Fatal(err)
	}
	out := s.collectSystem(ctx)
	if len(out.Accounts) != 1 || out.Accounts[0].OK || out.Accounts[0].Note == "" {
		t.Errorf("失败该只影响这一行: %+v", out.Accounts)
	}
}
