package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"strm115/internal/playback"
)

func TestPlayMaxDevicesSetting(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 2},      // 没配过：默认 2 台
		{"0", 0},     // 0 = 不限制
		{"4", 4},
		{"99", 2},    // 超范围回默认
		{"abc", 2},   // 非数字回默认
		{"-1", 2},
	} {
		if err := st.SetSetting(ctx, "play_max_devices", tc.raw); err != nil {
			t.Fatal(err)
		}
		if got := s.playMaxDevices(ctx); got != tc.want {
			t.Errorf("play_max_devices=%q → %d，期望 %d", tc.raw, got, tc.want)
		}
	}
}

// 设备列表接口要能列出刚起播的设备，并支持手动释放。
func TestPlaybackDevicesAPI(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)
	const key = "dev:test-playback-device"
	defer playback.Default.Release(key)

	if err := playback.Default.Acquire(context.Background(), playback.Session{
		Key: key, Label: "测试播放器", IP: "192.168.1.20", ItemID: "437",
	}); err != nil {
		t.Fatal(err)
	}

	rec, _ := doJSON(t, h, http.MethodGet, "/api/playback/devices", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("设备列表失败 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Limit      int `json:"limit"`
		Count      int `json:"count"`
		TTLMinutes int `json:"ttl_minutes"`
		Devices    []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Limit != 2 || out.TTLMinutes <= 0 || out.Count < 1 {
		t.Fatalf("返回结构不对：%+v", out)
	}
	found := false
	for _, d := range out.Devices {
		if d.Key == key && d.Label == "测试播放器" {
			found = true
		}
	}
	if !found {
		t.Fatalf("刚起播的设备没出现在列表里：%+v", out.Devices)
	}

	rel, _ := doJSON(t, h, http.MethodDelete, "/api/playback/devices/"+key, nil, cookies)
	if rel.Code != http.StatusOK {
		t.Fatalf("释放失败 %d: %s", rel.Code, rel.Body.String())
	}
	if playback.Default.Release(key) {
		t.Fatal("释放接口没有真的把设备删掉")
	}
}
