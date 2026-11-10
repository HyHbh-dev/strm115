package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"strm115/internal/store"
)

// 空库时这一页也要能打开：没有任务、没有历史，而不是 500。
func TestTasksEmpty(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/tasks", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if items, ok := out["running"].([]any); !ok || len(items) != 0 {
		t.Fatalf("running 应是空数组，实际 %v", out["running"])
	}
	sum, _ := out["summary"].(map[string]any)
	if sum["running"].(float64) != 0 {
		t.Fatalf("running 计数应为 0，实际 %v", sum["running"])
	}
}

// 历史读的是 sync_runs；清空只删已结束的，正在跑的那行必须留着。
func TestTaskHistoryAndClear(t *testing.T) {
	s, st := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)
	ctx := context.Background()

	done, err := st.StartSyncRun(ctx, 1, "电影", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSyncRun(ctx, done, "done", 10, 8, 1, 1, 0, "完成"); err != nil {
		t.Fatal(err)
	}
	failed, _ := st.StartSyncRun(ctx, 2, "剧集", "auto")
	_ = st.FinishSyncRun(ctx, failed, "error", 5, 2, 0, 3, 0, "115 返回 401")

	rec, _ := doJSON(t, h, http.MethodGet, "/api/tasks/history?limit=10", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	items, _ := out["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("应该有 2 条历史，实际 %d", len(items))
	}
	// 倒序：最近的一轮在最上面
	first, _ := items[0].(map[string]any)
	if first["library_name"] != "剧集" {
		t.Fatalf("历史应按时间倒序，第一条是 %v", first["library_name"])
	}
	if out["total"].(float64) != 2 {
		t.Fatalf("total 应为 2，实际 %v", out["total"])
	}

	// 再写一条「正在跑」的：清空时它必须留下，否则用户会以为任务凭空消失了。
	if _, err := st.StartSyncRun(ctx, 3, "动漫", "manual"); err != nil {
		t.Fatal(err)
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/tasks/clear", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空失败 %d: %s", rec.Code, rec.Body.String())
	}
	if n := decodeBody(t, rec)["deleted"].(float64); n != 2 {
		t.Fatalf("应该删掉 2 条已结束的，实际 %v", n)
	}

	rec, _ = doJSON(t, h, http.MethodGet, "/api/tasks/history", nil, cookies)
	items, _ = decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("清空后应只剩正在跑的那条，实际 %d", len(items))
	}
}

// 失败的轮次要能在汇总里看到：这一项回答的是「有没有东西一直在坏」。
func TestTasksSummaryFailed(t *testing.T) {
	s, st := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)
	ctx := context.Background()

	id, _ := st.StartSyncRun(ctx, 1, "电影", "manual")
	if err := st.FinishSyncRun(ctx, id, "error", 3, 0, 0, 3, 0, "115 返回 401"); err != nil {
		t.Fatal(err)
	}

	rec, _ := doJSON(t, h, http.MethodGet, "/api/tasks", nil, cookies)
	sum, _ := decodeBody(t, rec)["summary"].(map[string]any)
	if sum["failed_24h"].(float64) != 1 {
		t.Fatalf("24 小时内应统计到 1 次失败，实际 %v", sum["failed_24h"])
	}
	if msg, _ := sum["last_error"].(string); msg == "" {
		t.Fatal("应该带一条失败原因，方便直接显示在页面上")
	}
}

// 取消一个没在跑的任务要如实报 409 —— 前端据此把按钮变灰，
// 而不是让用户点了没反应还以为自己点错了。
func TestTaskCancelNotRunning(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/tasks/cancel",
		map[string]any{"kind": "sync", "library_id": 1}, cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("应返回 409，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/tasks/cancel",
		map[string]any{"kind": "nope", "library_id": 1}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知类型应返回 400，实际 %d", rec.Code)
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/tasks/cancel",
		map[string]any{"kind": "sync"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 library_id 应返回 400，实际 %d", rec.Code)
	}
}

// 重跑的库不存在时给 404，不能悄悄起一个任务。
func TestTaskRunUnknownLibrary(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/tasks/run",
		map[string]any{"kind": "sync", "library_id": 999}, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应返回 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/tasks/run",
		map[string]any{"kind": "subtitle", "library_id": 1}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("不支持的类型应返回 400，实际 %d", rec.Code)
	}
}

// running 里的每一行都要能被前端直接渲染：字段名错了页面就是一片空白，
// 而这种错编译期发现不了。
func TestTaskRowJSONKeys(t *testing.T) {
	row := taskRow{
		ID: "sync:1", Kind: "sync", Label: "同步", LibraryID: 1, Library: "电影",
		Phase: "scan", Done: 3, Total: 10, Percent: -1, Message: "扫描中",
		StartedAt: time.Now().Format(time.RFC3339), Cancelable: true,
		Detail: map[string]int{"files": 10},
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "kind", "label", "library_id", "library",
		"phase", "done", "total", "percent", "message", "started_at", "cancelable"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺字段 %q：%s", k, string(b))
		}
	}
	// 同步的 total 是「已发现的文件数」，百分比无意义，必须是 -1 而不是 0：
	// 0% 会让进度条看起来卡住了。
	if m["percent"].(float64) != -1 {
		t.Errorf("同步任务 percent 应为 -1，实际 %v", m["percent"])
	}
}

var _ = store.SyncRun{}
