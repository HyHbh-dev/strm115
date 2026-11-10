package resource

import "testing"

func TestDropAds(t *testing.T) {
	items := []Item{
		{Title: "Dune.Part.Two.2024.2160p.UHD.BluRay", Size: 60 << 30, Seeders: 100},
		{Title: "最新网址发布页 点击进入", Size: 12 << 20},
		{Title: "Dune.Part.Two.2024.1080p.WEB-DL", Size: 12 << 30, Seeders: 20},
		{Title: "某站广告 加微信", Size: 5 << 30},                 // 体积正常但标题是广告
		{Title: "Dune.Part.Two.2024.720p", Size: 40 << 20}, // 体积过小
		{Title: "Dune.Part.Two.2024.TG 文字帖"},               // 没有体积，保留
	}
	got, dropped := dropAds(items)
	if dropped != 3 {
		t.Fatalf("应过滤 3 条，实际 %d: %+v", dropped, got)
	}
	if len(got) != 3 {
		t.Fatalf("应剩 3 条，实际 %d: %+v", len(got), got)
	}
	for _, it := range got {
		if it.Title == "最新网址发布页 点击进入" || it.Title == "某站广告 加微信" || it.Size == 40<<20 {
			t.Errorf("广告没被过滤: %+v", it)
		}
	}
}

// TestSortItemsBySizeThenSeeders 同等相关度下体积大的在前，体积相同看热度。
func TestSortItemsBySizeThenSeeders(t *testing.T) {
	items := []Item{
		{Title: "Dune Part Two 720p", Size: 3 << 30, Seeders: 900},
		{Title: "Dune Part Two 2160p", Size: 60 << 30, Seeders: 10},
		{Title: "Dune Part Two 1080p", Size: 12 << 30, Seeders: 500},
		{Title: "Dune Part Two TG 帖"},
	}
	sortItems(items, "Dune Part Two")
	for i := 1; i < len(items); i++ {
		if items[i-1].Size < items[i].Size {
			t.Fatalf("体积没有降序: %+v", items)
		}
	}
	if items[len(items)-1].Size != 0 {
		t.Fatalf("体积未知的应沉底: %+v", items)
	}
}
