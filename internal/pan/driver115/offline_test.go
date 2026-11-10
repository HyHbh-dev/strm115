package driver115

import (
	"errors"
	"strings"
	"testing"

	"strm115/internal/pan"
)

// 这组测试盯的是「115 回包 → 通用错误分类」的翻译。
// 分错类的代价很具体：把业务拒绝当临时故障重试，等于朝同一个坑连撞三次；
// 把风控当普通错误，就是在帮人把自己号封了。

func TestDedupURIs(t *testing.T) {
	got := dedupURIs([]string{"  magnet:?xt=urn:btih:aa  ", "", "magnet:?xt=urn:btih:aa", "magnet:?xt=urn:btih:bb"})
	if len(got) != 2 || got[0] != "magnet:?xt=urn:btih:aa" || got[1] != "magnet:?xt=urn:btih:bb" {
		t.Fatalf("去重结果不对: %#v", got)
	}
	if n := len(dedupURIs(nil)); n != 0 {
		t.Fatalf("空输入应返回空切片，得到 %d 项", n)
	}
}

func TestOfflineRejectRiskVerify(t *testing.T) {
	err := offlineReject(offlineAddResp{Errno: 911, ErrorMsg: "需要验证"})
	if !errors.Is(err, pan.ErrRisk) {
		t.Fatalf("911 应归为 ErrRisk，实际 %v", pan.KindOf(err))
	}
}

func TestOfflineRejectBadURL(t *testing.T) {
	err := offlineReject(offlineAddResp{Errno: 10008, ErrorMsg: "链接无效"})
	if !errors.Is(err, pan.ErrRejected) {
		t.Fatalf("10008 应归为 ErrRejected，实际 %v", pan.KindOf(err))
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "链接无效") {
		t.Fatalf("错误信息应带上 115 的原因，得到 %q", got)
	}
}

func TestOfflineRejectUnknownKeepsErrno(t *testing.T) {
	err := offlineReject(offlineAddResp{Errcode: 10004, ErrorMsg: "任务数已达上限"})
	if !errors.Is(err, pan.ErrRejected) {
		t.Fatalf("未知业务码应归为 ErrRejected，实际 %v", pan.KindOf(err))
	}
	if !strings.Contains(err.Error(), "10004") {
		t.Fatalf("未知业务码要留在信息里便于排查，得到 %q", err.Error())
	}
}

func TestOfflineRejectEmptyMessage(t *testing.T) {
	err := offlineReject(offlineAddResp{})
	if !errors.Is(err, pan.ErrRejected) || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("空回包也要给出可读原因，得到 %v", err)
	}
}

func TestFindErrno(t *testing.T) {
	if v := findErrno(0, 0, 99); v != 99 {
		t.Fatalf("应取第一个非零值，得到 %d", v)
	}
	if v := findErrno(0, 0); v != 0 {
		t.Fatalf("全零应返回 0，得到 %d", v)
	}
}

func TestOfflineStatus(t *testing.T) {
	cases := map[int]pan.OfflineStatus{
		2:  pan.OfflineDone,
		1:  pan.OfflineRunning,
		-1: pan.OfflineFailed,
		0:  pan.OfflineWaiting,
		9:  pan.OfflineWaiting,
	}
	for code, want := range cases {
		if got := offlineStatus(code); got != want {
			t.Fatalf("状态码 %d 应为 %s，得到 %s", code, want, got)
		}
	}
}

func TestClampPercent(t *testing.T) {
	cases := map[float64]float64{-5: 0, 0: 0, 95.69: 95.69, 100: 100, 9569: 100}
	for in, want := range cases {
		if got := clampPercent(in); got != want {
			t.Fatalf("clampPercent(%v) = %v，期望 %v", in, got, want)
		}
	}
}
