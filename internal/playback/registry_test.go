package playback

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testRegistry(limit int) *Registry {
	r := New()
	r.SetWait(0) // 单测不等，满了立刻返回
	if limit >= 0 {
		r.SetLimitFunc(func() int { return limit })
	}
	return r
}

func TestRegistryLimitsConcurrentDevices(t *testing.T) {
	r := testRegistry(2)
	ctx := context.Background()
	for _, key := range []string{"dev:phone", "dev:tv"} {
		if err := r.Acquire(ctx, Session{Key: key, Label: key}); err != nil {
			t.Fatalf("前两台应该都能播，%s: %v", key, err)
		}
	}
	if err := r.Acquire(ctx, Session{Key: "dev:pad", Label: "pad"}); !errors.Is(err, ErrFull) {
		t.Fatalf("第三台应被拦，实际 %v", err)
	}
	if got := len(r.Snapshot()); got != 2 {
		t.Fatalf("被拦的不该占名额，实际 %d 台", got)
	}
	if !r.Release("dev:tv") {
		t.Fatal("释放失败")
	}
	if err := r.Acquire(ctx, Session{Key: "dev:pad", Label: "pad"}); err != nil {
		t.Fatalf("释放后应该能补上，实际 %v", err)
	}
}

// 同一台设备拖进度条会再发一次静态请求，不能把一台算成两台。
func TestRegistrySameDeviceRefreshes(t *testing.T) {
	r := testRegistry(1)
	ctx := context.Background()
	if err := r.Acquire(ctx, Session{Key: "dev:phone", ItemID: "437"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Acquire(ctx, Session{Key: "dev:phone", ItemID: "438"}); err != nil {
		t.Fatalf("同一设备重复起播不该被拦：%v", err)
	}
	devs := r.Snapshot()
	if len(devs) != 1 || devs[0].ItemID != "438" {
		t.Fatalf("应刷新同一行而不是新增：%+v", devs)
	}
}

// 302 直连收不到结束事件，靠 TTL 把「早就不看了」的设备清掉。
func TestRegistryExpiresAfterTTL(t *testing.T) {
	r := testRegistry(1)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	if err := r.Acquire(ctx, Session{Key: "dev:phone"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Acquire(ctx, Session{Key: "dev:tv"}); !errors.Is(err, ErrFull) {
		t.Fatalf("还没过期应该仍然满，实际 %v", err)
	}
	now = now.Add(DefaultTTL + time.Minute)
	if err := r.Acquire(ctx, Session{Key: "dev:tv"}); err != nil {
		t.Fatalf("过期后应该能进，实际 %v", err)
	}
	if got := len(r.Snapshot()); got != 1 {
		t.Fatalf("过期的设备应被清掉，实际 %d", got)
	}
}

func TestRegistryUnlimitedStillTracksDevices(t *testing.T) {
	r := testRegistry(0)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := r.Acquire(ctx, Session{Key: string(rune('a' + i))}); err != nil {
			t.Fatalf("不限制时不该拦：%v", err)
		}
	}
	if got := len(r.Snapshot()); got != 4 {
		t.Fatalf("不限制也要能列出设备，实际 %d", got)
	}
}

func TestKeyFor(t *testing.T) {
	a := KeyFor("ios-1", "192.168.1.5", "SenPlayer")
	if a != "dev:ios-1" {
		t.Fatalf("有 DeviceId 时应优先用它，实际 %q", a)
	}
	b := KeyFor("", "192.168.1.5", "SenPlayer")
	if b == KeyFor("", "192.168.1.6", "SenPlayer") {
		t.Fatal("不同 IP 不该算同一台")
	}
	if strings.Contains(a, "/") || strings.Contains(b, "/") {
		t.Fatalf("设备 key 不能直接放进 URL 路径：%q %q", a, b)
	}
	if got := KeyFor("a/b c", "", ""); got != "dev:a_b_c" {
		t.Fatalf("非法字符要替换掉，实际 %q", got)
	}
}
