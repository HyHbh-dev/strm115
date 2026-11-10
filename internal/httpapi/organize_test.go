package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"strm115/internal/organize"
	"strm115/internal/store"
)

// newOrganizeFixture 造一个「一个账号 + 一个电影库 + 一个剧集库」的最小环境。
func newOrganizeFixture(t *testing.T) (*Server, *store.Store, int64, int64, int64) {
	t.Helper()
	s, st := newServerWithStore(t)
	ctx := context.Background()

	acctID, err := st.CreateAccount(ctx, "115", "主号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	movieID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100", LocalPath: t.TempDir(),
		CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tvID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "剧集", RemoteCID: "200", LocalPath: t.TempDir(),
		CollectionType: "tvshows", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, st, acctID, movieID, tvID
}

// 配置要能存下来、读回来，并且默认值合理。
func TestOrganizeConfigRoundTrip(t *testing.T) {
	s, _, acctID, movieID, tvID := newOrganizeFixture(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/organize/config", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读配置失败 %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Config    organize.Config  `json:"config"`
		Libraries []map[string]any `json:"libraries"`
		Accounts  []map[string]any `json:"accounts"`
		Stats     map[string]int   `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Libraries) != 2 || len(got.Accounts) != 1 {
		t.Fatalf("媒体库/账号清单不对: %+v %+v", got.Libraries, got.Accounts)
	}
	// 没配过时也要给出可用的默认值，否则前端输入框全是 0
	if got.Config.Batch != 30 {
		t.Errorf("默认 batch 应为 30，实际 %d", got.Config.Batch)
	}

	body := map[string]any{"config": organize.Config{
		Enabled: true, SourceCID: "999", SourceName: "待整理",
		MovieLibID: movieID, TVLibID: tvID, AccountID: acctID,
		Batch: 10, MinSizeMB: 500, SyncAfter: true,
	}}
	rec, _ = doJSON(t, h, http.MethodPut, "/api/organize/config", body, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodGet, "/api/organize/config", nil, cookies)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Config.SourceCID != "999" || got.Config.MovieLibID != movieID || got.Config.Batch != 10 {
		t.Fatalf("配置没存住: %+v", got.Config)
	}
}

// 源目录和目标库属于不同 115 账号时，移动在网盘侧根本不可能成功，
// 必须在保存时就说清楚，而不是等用户点「整理」才失败。
func TestOrganizeConfigRejectsCrossAccount(t *testing.T) {
	s, st, _, _, _ := newOrganizeFixture(t)
	ctx := context.Background()
	other, err := st.CreateAccount(ctx, "115", "小号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	otherLib, err := st.CreateLibrary(ctx, store.Library{
		AccountID: other, Name: "别人的电影", RemoteCID: "300", LocalPath: t.TempDir(),
		CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	h := s.Handler()
	cookies := loginForTest(t, h)
	rec, _ := doJSON(t, h, http.MethodPut, "/api/organize/config", map[string]any{
		"config": organize.Config{Enabled: true, SourceCID: "1", AccountID: 1, MovieLibID: otherLib},
	}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("跨账号应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "跨账号") {
		t.Fatalf("错误说明不对: %s", rec.Body.String())
	}
}

// 没配源目录 / 没启用时，扫描与执行都要明确拒绝，不能去动网盘。
func TestOrganizeScanNeedsConfig(t *testing.T) {
	s, _, _, _, _ := newOrganizeFixture(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/organize/scan", nil, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没配源目录应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "待整理") {
		t.Fatalf("提示应指向「待整理」目录: %s", rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/organize/run", map[string]any{}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没启用应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 识别试算走的是真实识别器，页面上看到的预览必须和实际整理一致。
func TestOrganizePreview(t *testing.T) {
	s, _, _, _, _ := newOrganizeFixture(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	cases := []struct{ name, wantDir, wantKind string }{
		{"沙丘2.2024.2160p.mkv", "沙丘2 (2024)", organize.KindMovie},
		{"漫长的季节.2023.S01E01.mkv", "漫长的季节 (2023)/Season 1", organize.KindSeries},
	}
	for _, c := range cases {
		rec, _ := doJSON(t, h, http.MethodPost, "/api/organize/preview", map[string]any{"name": c.name}, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 试算失败 %d: %s", c.name, rec.Code, rec.Body.String())
		}
		var out struct {
			Item organize.Item `json:"item"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Item.Dir != c.wantDir || out.Item.Kind != c.wantKind {
			t.Errorf("%s: got dir=%q kind=%s，want dir=%q kind=%s",
				c.name, out.Item.Dir, out.Item.Kind, c.wantDir, c.wantKind)
		}
	}

	// 空名字要报错，不能返回一个空结果让界面显示「已识别」
	rec, _ := doJSON(t, h, http.MethodPost, "/api/organize/preview", map[string]any{"name": "  "}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空名字应 400，实际 %d", rec.Code)
	}
}

// 台账：能列、能标忽略，忽略后不再出现在扫描的「待处理」里。
func TestOrganizeRecordsAndIgnore(t *testing.T) {
	s, st, acctID, _, _ := newOrganizeFixture(t)
	ctx := context.Background()
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec := store.OrganizeRecord{
		AccountID: acctID, FileID: "f1", Name: "沙丘2.2024.mkv", Size: 5 << 30,
		Kind: organize.KindMovie, Title: "沙丘2", Year: "2024", Confidence: 85,
		Reason: "片名 · 年份", Target: "电影/沙丘2 (2024)/沙丘2.2024.mkv",
		Status: store.OrganizeMoved,
	}
	if err := st.UpsertOrganizeRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec2 := store.OrganizeRecord{
		AccountID: acctID, FileID: "f2", Name: "坏文件.mkv",
		Status: store.OrganizeFailed, Message: "115 报错",
	}
	if err := st.UpsertOrganizeRecord(ctx, rec2); err != nil {
		t.Fatal(err)
	}

	prec, _ := doJSON(t, h, http.MethodGet, "/api/organize/records", nil, cookies)
	if prec.Code != http.StatusOK {
		t.Fatalf("列台账失败 %d: %s", prec.Code, prec.Body.String())
	}
	var list struct {
		Items []store.OrganizeRecord `json:"items"`
		Total int                    `json:"total"`
		Stats map[string]int         `json:"stats"`
	}
	if err := json.Unmarshal(prec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("应有 2 条记录，实际 total=%d len=%d", list.Total, len(list.Items))
	}
	if list.Stats[store.OrganizeMoved] != 1 || list.Stats[store.OrganizeFailed] != 1 {
		t.Fatalf("状态统计不对: %+v", list.Stats)
	}

	// 按状态过滤
	prec, _ = doJSON(t, h, http.MethodGet, "/api/organize/records?status=failed", nil, cookies)
	_ = json.Unmarshal(prec.Body.Bytes(), &list)
	if list.Total != 1 || list.Items[0].FileID != "f2" {
		t.Fatalf("按状态过滤不对: %+v", list)
	}

	// 忽略失败的那条
	id := list.Items[0].ID
	prec, _ = doJSON(t, h, http.MethodPost, "/api/organize/records/"+itoa(id)+"/ignore", nil, cookies)
	if prec.Code != http.StatusOK {
		t.Fatalf("忽略失败 %d: %s", prec.Code, prec.Body.String())
	}
	got, err := st.OrganizeRecord(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.OrganizeIgnored {
		t.Fatalf("状态没改成 ignored: %s", got.Status)
	}

	// 忽略过的文件不该再出现在「待处理」清单里
	statuses, err := st.OrganizeStatuses(ctx, acctID)
	if err != nil {
		t.Fatal(err)
	}
	if !store.OrganizeDone(statuses["f2"]) {
		t.Fatalf("忽略后应算已处理: %+v", statuses)
	}
	if !store.OrganizeDone(statuses["f1"]) {
		t.Fatalf("已移动的也应算已处理: %+v", statuses)
	}
}

// 清空台账只删记录，不影响别的库的表。
func TestOrganizeClearRecords(t *testing.T) {
	s, st, acctID, _, _ := newOrganizeFixture(t)
	ctx := context.Background()
	h := s.Handler()
	cookies := loginForTest(t, h)

	for _, id := range []string{"a", "b"} {
		_ = st.UpsertOrganizeRecord(ctx, store.OrganizeRecord{
			AccountID: acctID, FileID: id, Name: id + ".mkv", Status: store.OrganizeSkipped,
		})
	}
	prec, _ := doJSON(t, h, http.MethodPost, "/api/organize/records/clear", map[string]any{}, cookies)
	if prec.Code != http.StatusOK {
		t.Fatalf("清空失败 %d: %s", prec.Code, prec.Body.String())
	}
	if !strings.Contains(prec.Body.String(), `"removed":2`) {
		t.Fatalf("应删除 2 条: %s", prec.Body.String())
	}
	stats, _ := st.OrganizeStats(ctx)
	if len(stats) != 0 {
		t.Fatalf("清空后不该有统计: %+v", stats)
	}
}

// 老配置里没有 auto_ingest 字段，默认当作「入库后自动整理」开着：
// 这条链本来就是整理最该有的样子，不默认开等于新用户永远踩不到。
func TestOrganizeAutoIngestDefaultsOn(t *testing.T) {
	s, st, acctID, movieID, tvID := newOrganizeFixture(t)
	ctx := context.Background()

	cfg := s.loadOrganizeConfig(ctx)
	if !cfg.AutoIngestOn() {
		t.Fatal("没配过时「入库后自动整理」应默认开着")
	}

	// 用户主动关掉之后要能存住，不能被 Filled 又掰回来
	off := false
	raw, _ := json.Marshal(organize.Config{
		Enabled: true, SourceCID: "999", AccountID: acctID,
		MovieLibID: movieID, TVLibID: tvID, AutoIngest: &off,
	})
	if err := st.SetSetting(ctx, settingOrganizeConfig, string(raw)); err != nil {
		t.Fatal(err)
	}
	if s.loadOrganizeConfig(ctx).AutoIngestOn() {
		t.Fatal("存了 false 之后不该又变成 true")
	}
}

// 入库后能不能自动整理，取决于开关 + 目标库 + 资源保存目录三者是否齐。
func TestOrganizerIngestGates(t *testing.T) {
	s, st, acctID, movieID, tvID := newOrganizeFixture(t)
	ctx := context.Background()

	if s.organizerIngestOn(ctx) {
		t.Fatal("默认没启用整理，不该允许自动整理")
	}
	if err := st.SetSetting(ctx, "res_save_cid", "3555360008763868604"); err != nil {
		t.Fatal(err)
	}
	cfg := organize.Config{
		Enabled: true, SourceCID: "999", AccountID: acctID,
		MovieLibID: movieID, TVLibID: tvID,
	}
	raw, _ := json.Marshal(cfg)
	if err := st.SetSetting(ctx, settingOrganizeConfig, string(raw)); err != nil {
		t.Fatal(err)
	}
	if !s.organizerIngestOn(ctx) {
		t.Fatal("开关、目标库、资源保存目录都齐了，应该允许自动整理")
	}

	// 只开了开关、没配目标库 → 整理无从落位，别去动文件
	raw, _ = json.Marshal(organize.Config{Enabled: true, SourceCID: "999"})
	_ = st.SetSetting(ctx, settingOrganizeConfig, string(raw))
	if s.organizerIngestOn(ctx) {
		t.Fatal("没配任何目标库时不该自动整理")
	}
}

// 两轮整理撞车时必须拒绝第二轮，而不是排队去挪同一批文件。
func TestRunOrganizeRejectsConcurrent(t *testing.T) {
	s, _, _, _, _ := newOrganizeFixture(t)
	cfg := s.loadOrganizeConfig(context.Background())
	cfg.Enabled = true
	cfg.SourceCID = "999"

	rule := organize.Rule{ID: "r1", AccountID: cfg.AccountID, SourceCID: "999", MovieLibID: cfg.MovieLibID}
	s.organizeMu.Lock()
	_, err := s.runOrganizeRules(context.Background(), cfg, []organize.Rule{rule}, nil, false)
	s.organizeMu.Unlock()
	if !errors.Is(err, errOrganizeBusy) {
		t.Fatalf("应返回「上一轮还在跑」，实际 %v", err)
	}
}
