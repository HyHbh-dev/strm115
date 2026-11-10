package tgbot

import (
	"fmt"
	"strings"
	"testing"

	"strm115/internal/resource"
)

func tgItem(i int) resource.Item {
	return resource.Item{Title: fmt.Sprintf("115 分享 %d", i), Kind: "tg", Source: "@QukanMovie", ShareCode: fmt.Sprintf("code%d", i)}
}

func torrentItem(i int) resource.Item {
	return resource.Item{Title: fmt.Sprintf("Dune Prophecy S01E%02d", i), Kind: "torrent", Source: "海盗湾", Magnet: fmt.Sprintf("magnet:?xt=urn:btih:%d", i)}
}

func countTorrent(items []resource.Item) int {
	n := 0
	for _, it := range items {
		if it.Kind == "torrent" {
			n++
		}
	}
	return n
}

// 回归：一个 TG 频道一次给 10 条时，直接截前 8 条会把 3 条磁力
// 全挤到列表外，用户看到的就是「怎么没有种子站结果」。
// 现在整轮结果都留着分页，交错排序保证每一屏都有磁力。
func TestBalanceOrderKeepsTorrentsWhenTGDominates(t *testing.T) {
	var all []resource.Item
	for i := 0; i < 10; i++ {
		all = append(all, tgItem(i))
	}
	for i := 0; i < 3; i++ {
		all = append(all, torrentItem(i))
	}
	got := balanceOrder(all)
	if len(got) != len(all) {
		t.Fatalf("排序不该丢条目：%d != %d", len(got), len(all))
	}
	firstPage := got[:pageSize]
	if n := countTorrent(firstPage); n != 3 {
		t.Errorf("第一屏就该有 3 条磁力，实际 %d", n)
	}
}

func TestBalanceOrderInterleavesAndKeepsOrder(t *testing.T) {
	var all []resource.Item
	for i := 0; i < 4; i++ {
		all = append(all, tgItem(i))
		all = append(all, torrentItem(i))
	}
	got := balanceOrder(all)
	for i := range got {
		if i%2 == 0 && got[i].Kind != "torrent" {
			t.Errorf("第 %d 条应为种子站，实际 %s", i+1, got[i].Kind)
		}
		if i%2 == 1 && got[i].Kind != "tg" {
			t.Errorf("第 %d 条应为 TG，实际 %s", i+1, got[i].Kind)
		}
	}
	if got[0].Title != "Dune Prophecy S01E00" || got[2].Title != "Dune Prophecy S01E01" {
		t.Errorf("队内相关性顺序被破坏了: %v", got)
	}
}

func TestBalanceOrderOneSideEmpty(t *testing.T) {
	var onlyTG []resource.Item
	for i := 0; i < 12; i++ {
		onlyTG = append(onlyTG, tgItem(i))
	}
	if got := balanceOrder(onlyTG); len(got) != 12 || countTorrent(got) != 0 {
		t.Errorf("只有 TG 时应原样保留: %+v", got)
	}
}

func sites(failed int, counts ...int) []resource.Status {
	var out []resource.Status
	for i, c := range counts {
		out = append(out, resource.Status{Key: fmt.Sprintf("site%d", i), Kind: "torrent", OK: true, Count: c})
	}
	for i := 0; i < failed; i++ {
		out = append(out, resource.Status{Key: fmt.Sprintf("bad%d", i), Kind: "torrent", OK: false, Err: "timeout"})
	}
	return out
}

// 连得上但 0 条，Status.OK 是 true —— 以前机器人一个字都不提，
// 用户只看到一屏 TG 分享，自然以为种子站没搜。
func TestSiteSummaryZeroHits(t *testing.T) {
	res := &resource.Result{Sources: sites(0, 0, 0, 0, 0)}
	line := siteSummary(res)
	if !strings.Contains(line, "都没匹配") || !strings.Contains(line, "4 个站") {
		t.Errorf("0 条时要说清楚: %q", line)
	}
}

func TestSiteSummaryPartialFailure(t *testing.T) {
	res := &resource.Result{Sources: sites(2, 0, 0)}
	line := siteSummary(res)
	if !strings.Contains(line, "2 个没连上") {
		t.Errorf("失败源要单列: %q", line)
	}
}

func TestSiteSummaryWithHits(t *testing.T) {
	res := &resource.Result{
		Sources: sites(0, 6, 0, 0, 0),
		Items:   []resource.Item{torrentItem(1), torrentItem(2)},
	}
	line := siteSummary(res)
	if !strings.Contains(line, "1/4") || !strings.Contains(line, "2 条") {
		t.Errorf("有结果时要报命中数: %q", line)
	}
}

func TestRenderResultsMentionsSiteStatus(t *testing.T) {
	res := &resource.Result{
		Query:   "沙丘：预言",
		Sources: sites(0, 0, 0, 0, 0),
		Items:   []resource.Item{tgItem(1), tgItem(2)},
	}
	msg := renderResults((&pick{query: "沙丘：预言", res: res, items: res.Items}).view())
	if !strings.Contains(msg, "种子站") || !strings.Contains(msg, "都没匹配") {
		t.Errorf("消息里应写明种子站状态:\n%s", msg)
	}
}

// /name 的结果要能直接复制：英文名放在 <code> 里，中文名只作确认。
func TestRenderNameLookupSingle(t *testing.T) {
	got := renderNameLookup("沙丘2", []TitleHit{
		{Title: "Dune: Part Two", Original: "Dune: Part Two", Year: "2024", Kind: "movie"},
	})
	if !strings.Contains(got, "<code>Dune: Part Two</code>") {
		t.Fatalf("英文名应在 code 里：%q", got)
	}
	if !strings.Contains(got, "2024") {
		t.Fatalf("应带年份：%q", got)
	}
}

// 日韩片：英文名与原始语言名不同，两个都要显示，且英文名在前。
func TestRenderNameLookupKeepsOriginal(t *testing.T) {
	got := renderNameLookup("君の名は", []TitleHit{
		{Title: "Your Name.", Original: "君の名は。", Year: "2016", Kind: "movie"},
	})
	if !strings.Contains(got, "<code>Your Name.</code>") {
		t.Fatalf("英文名应是 code 里的 Your Name.：%q", got)
	}
	if !strings.Contains(got, "君の名は。") {
		t.Fatalf("原始名应作为备注显示：%q", got)
	}
}

// 多条候选用编号列出，且提示可以继续用 /search 入库。
func TestRenderNameLookupMultiple(t *testing.T) {
	got := renderNameLookup("dune", []TitleHit{
		{Title: "Dune", Year: "2021", Kind: "movie"},
		{Title: "Dune: Part Two", Year: "2024", Kind: "movie"},
	})
	if !strings.Contains(got, "2 个") || !strings.Contains(got, "1.") || !strings.Contains(got, "2.") {
		t.Fatalf("多条应带编号：%q", got)
	}
	if !strings.Contains(got, "/search") {
		t.Fatalf("应提示入库用 /search：%q", got)
	}
}
