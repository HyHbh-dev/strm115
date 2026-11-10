package driverquarktv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"strm115/internal/pan"
)

// 夸克移动/改名/删除走网页接口，必须带上网页 Cookie；这里用假服务器验
// 请求体与任务轮询，不碰真账号。
func TestWebMutations(t *testing.T) {
	type call struct {
		path string
		body map[string]any
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		calls = append(calls, call{path: r.URL.Path, body: body})

		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__puus=webcookie") {
			t.Errorf("网页接口必须带 Cookie，实际 %q", got)
		}
		switch r.URL.Path {
		case "/file":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fid":"dir-1"}}`))
		case "/file/move", "/file/delete":
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"task-1"}}`))
		case "/task":
			// 夸克完成时 task_status 可能是 null，只看 status
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":2,"task_status":null,"message":"ok"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()

	old := webBase
	webBase = srv.URL
	defer func() { webBase = old }()

	cr := Credential{RefreshToken: "RT", WebCookie: "__puus=webcookie"}
	c := New(cr)
	c.access = "token"

	ctx := context.Background()
	fid, err := c.Mkdir(ctx, "0", "电影")
	if err != nil || fid != "dir-1" {
		t.Fatalf("建目录失败: fid=%q err=%v", fid, err)
	}
	if err := c.Move(ctx, "dir-1", "f1", "f2"); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if err := c.Rename(ctx, "f1", "新名字.mkv"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if err := c.Remove(ctx, "f1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	byPath := map[string]call{}
	for _, c := range calls {
		byPath[c.path] = c
	}
	move, ok := byPath["/file/move"]
	if !ok {
		t.Fatalf("没有发出移动请求: %+v", calls)
	}
	if got := move.body["to_pdir_fid"]; got != "dir-1" {
		t.Errorf("移动目标目录不对: %v", got)
	}
	if list, _ := move.body["filelist"].([]any); len(list) != 2 {
		t.Errorf("移动应该带两个 fid: %v", move.body["filelist"])
	}
	ren, ok := byPath["/file/rename"]
	if !ok {
		t.Fatalf("没有发出改名请求: %+v", calls)
	}
	if ren.body["fid"] != "f1" || ren.body["file_name"] != "新名字.mkv" {
		t.Errorf("改名请求体不对: %v", ren.body)
	}
	del, ok := byPath["/file/delete"]
	if !ok {
		t.Fatalf("没有发出删除请求: %+v", calls)
	}
	if list, _ := del.body["filelist"].([]any); len(list) != 1 || list[0] != "f1" {
		t.Errorf("删除请求体不对: %v", del.body)
	}
}

// 网页 Cookie 失效时要报 ErrAuth，并且提示怎么修 —— 这个错误会直接显示在
// 整理台账里，用户得知道去重新添加账号。
func TestWebCookieExpiredIsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":31001,"message":"登录已失效"}`))
	}))
	defer srv.Close()
	old := webBase
	webBase = srv.URL
	defer func() { webBase = old }()

	c := New(Credential{RefreshToken: "RT", WebCookie: "__puus=x"})
	_, err := c.Mkdir(context.Background(), "0", "x")
	if !errors.Is(err, pan.ErrAuth) {
		t.Fatalf("应该 ErrAuth，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "重新添加") {
		t.Errorf("错误信息要告诉用户怎么办，实际 %q", err.Error())
	}
}
