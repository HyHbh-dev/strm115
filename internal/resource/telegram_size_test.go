package resource

import "testing"

const (
	testGiB = int64(1) << 30
	testMiB = int64(1) << 20
)

// 用函数而不是常量表达式：Go 不允许把非整数浮点常量直接转成 int64。
func gb(v float64) int64 { return int64(v * float64(testGiB)) }
func mb(v float64) int64 { return int64(v * float64(testMiB)) }

// 普通 TG 资源帖不带 💾 图标，体积藏在文件名方括号、标签或裸文本里。
// 这几种写法以前 Size 恒为 0，前端体积列是「—」、体积筛选也失效。
func TestSizeFromText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int64
	}{
		{"方括号 GB", "沙丘2 2160P [8.9GB] 简体内嵌", gb(8.9)},
		{"方括号 G 省略 B", "片名 1080P[2.1G]", gb(2.1)},
		{"方括号 GiB", "片名 [1.4 GiB]", gb(1.4)},
		{"中文标签", "文件大小：1.4 GiB\n简体中字", gb(1.4)},
		{"英文标签", "Size: 750MB", mb(750)},
		{"容量标签", "容量 1.5 GB", gb(1.5)},
		{"裸 GB", "片名 2.1 GB 简体中字", gb(2.1)},
		{"分辨率不误判", "片名 [1080P][x264][HDR10]", 0},
		{"年份分辨率不误判", "片名 2024 2160p WEB-DL", 0},
		{"空文本", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sizeFromText(c.text)
			if c.want == 0 {
				if got != 0 {
					t.Fatalf("不该解析出体积，拿到 %d", got)
				}
				return
			}
			// 浮点换算允许 1 字节误差。
			if got < c.want-1 || got > c.want+1 {
				t.Fatalf("want %d, got %d", c.want, got)
			}
		})
	}
}

func TestApplyHuntMetaIconSizeWins(t *testing.T) {
	// 图标字段是结构化数据，比正文里随便一个方括号可信，不能被覆盖。
	it := &Item{}
	applyHuntMeta(it, "📜 Results for Dune\n💾 1.4 GiB\n🟢 12\n片名 [8.9GB]")
	want := parseSizeText("1.4 GiB")
	if it.Size != want {
		t.Fatalf("图标体积应优先：want %d, got %d", want, it.Size)
	}
	if it.Seeders != 12 {
		t.Fatalf("做种数没解析出来：%d", it.Seeders)
	}
}

func TestParseChannelFillsSize(t *testing.T) {
	// 预览页里链接是 <a href>，resourceLinks 从原始 HTML 块里抽 href，
	// 不是从纯文本里认 URL —— 测试 HTML 得按真实结构写。
	page := `<div class="tgme_widget_message_wrap"><div class="tgme_widget_message_text">` +
		`奥德赛 2026 2160P [8.9GB]<br><a href="https://pan.quark.cn/s/abcdef">下载</a>` +
		`</div></div></main><footer>尾部</footer>`
	items := parseChannel(page)
	if len(items) != 1 {
		t.Fatalf("想解析出 1 条，拿到 %d 条", len(items))
	}
	if items[0].Size <= 0 {
		t.Fatal("TG 帖子里的 [8.9GB] 没有填进 Item.Size")
	}
	if len(items[0].Links) == 0 {
		t.Fatal("正文里的网盘链接不应丢失")
	}
}
