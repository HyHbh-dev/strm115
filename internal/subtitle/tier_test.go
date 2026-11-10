package subtitle

import "testing"

// TestSourceTier 片源名的分档：枪版 / WEB / 蓝光，认不出来按中间档。
func TestSourceTier(t *testing.T) {
	for _, c := range []struct {
		name string
		want int
	}{
		{"The.Odyssey.2026.2160p.UHD.Blu-ray.Remux.HDR.HEVC-CiNEPHiLES", TierHigh},
		{"The.Odyssey.2026.1080p.BluRay.x264", TierHigh},
		{"The.Odyssey.2026.BDRip.x264", TierHigh},
		{"The.Odyssey.2026.1080p.WEB-DL.DDP5.1", TierMid},
		{"The.Odyssey.2026.1080p.WEBRip.x264", TierMid},
		{"The.Odyssey.2026.1080p.HDTV", TierMid},
		{"The.Odyssey.2026.1080p.CAM.x264-OnlyFlix", TierLow},
		{"The.Odyssey.2026.HDCAM", TierLow},
		{"The.Odyssey.2026.HDTS", TierLow},
		{"The.Odyssey.2026.TELESYNC.x264", TierLow},
		{"The.Odyssey.2026.HDTC", TierLow},
		{"The.Odyssey.2026.HC-ESub", TierLow},
		{"Odyssey_简体中文版", TierMid},
		{"", TierMid},
	} {
		if got := sourceTier(c.name); got != c.want {
			t.Errorf("%s → %d，期望 %d", c.name, got, c.want)
		}
	}
}

// TestHasWordBoundary 不能把 DTS 里的 ts、CAMERA 里的 cam 当成枪版。
func TestHasWordBoundary(t *testing.T) {
	for _, c := range []struct {
		s, w string
		want bool
	}{
		{"dts-hd.ma.5.1", "ts", false},
		{"camera.obscura", "cam", false},
		{"hdcam", "cam", false}, // 连写的整体词由 hdcam 自己命中
		{"movie.cam.x264", "cam", true},
		{"movie.hdts", "ts", false},
		{"movie.hdts", "hdts", true},
		{"1080p-hc-esub", "hc", true},
	} {
		if got := hasWord(c.s, c.w); got != c.want {
			t.Errorf("hasWord(%q, %q) → %v，期望 %v", c.s, c.w, got, c.want)
		}
	}
}

// TestRankedScreenCopy release 或文件名任一写着枪版，就算枪版。
func TestRankedScreenCopy(t *testing.T) {
	if !(Ranked{Candidate: Candidate{Release: "X.2026.1080p.CAM"}}).ScreenCopy() {
		t.Error("release 写 CAM 应判为枪版")
	}
	if !(Ranked{Candidate: Candidate{Name: "X.2026.HDTS.srt"}}).ScreenCopy() {
		t.Error("文件名写 HDTS 应判为枪版")
	}
	if (Ranked{Candidate: Candidate{Release: "X.2026.2160p.BluRay"}}).ScreenCopy() {
		t.Error("蓝光不该判成枪版")
	}
}
