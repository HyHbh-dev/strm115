package resource

import (
	"strings"
	"testing"
)

// 用户把频道「显示名」粘进 tg_channels 的真实配置：
// 按空格拆会变成 Torrent / Search / Bot / 🔎 四个不存在的频道，
// 每个都真打一次 t.me，白占并发槽还拖慢种子站。
func TestNormalizeChannelsDropsDisplayName(t *testing.T) {
	got := NormalizeChannels("QukanMovie,yunpanquark,quarkshare,Torrent Search Bot 🔎")
	want := []string{"QukanMovie", "yunpanquark", "quarkshare"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("频道清单 = %v，期望 %v", got, want)
	}
}

// 纯 ASCII 的空格分隔仍然是合法写法，不能被上面的规则误伤。
func TestNormalizeChannelsKeepsSpaceSeparated(t *testing.T) {
	got := NormalizeChannels("QukanMovie yunpanquark, quarkshare")
	if strings.Join(got, ",") != "QukanMovie,yunpanquark,quarkshare" {
		t.Fatalf("空格分隔被破坏了: %v", got)
	}
}

func TestNormalizeChannelsPreviewLink(t *testing.T) {
	got := NormalizeChannels("@QukanMovie, https://t.me/s/yunpanquark/, 🔎")
	if strings.Join(got, ",") != "QukanMovie,yunpanquark" {
		t.Fatalf("预览页链接 / 纯 emoji 清洗不对: %v", got)
	}
}

func TestNormalizeSitesStillWorks(t *testing.T) {
	got := NormalizeSites("tpb, nyaa, uindex, therarbg, 不认识的站")
	if strings.Join(got, ",") != "tpb,nyaa,uindex,therarbg" {
		t.Fatalf("站点清单 = %v", got)
	}
}

// 「如何投稿：」这种说明帖也会命中频道内搜索，但它不能入库，
// 摆进结果只是白占一个序号按钮。
func TestParseChannelSkipsNoticePost(t *testing.T) {
	page := `<main>` +
		`<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="QukanMovie/1">` +
		`<div class="tgme_widget_message_text">如何投稿： 沙丘：预言 资源投稿说明</div></div></div>` +
		`<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="QukanMovie/2">` +
		`<div class="tgme_widget_message_text">沙丘：预言（2024）更新至05集 ` +
		`<a href="https://pan.quark.cn/s/abc">夸克</a></div></div></div>` +
		`</main>`
	items := parseChannel(page)
	if len(items) != 1 {
		t.Fatalf("应只剩 1 条资源帖，实际 %d: %+v", len(items), items)
	}
	if !strings.Contains(items[0].Title, "沙丘") {
		t.Errorf("留下的不是资源帖: %q", items[0].Title)
	}
}

// 标题像公告、但帖子里真有资源链接时不能丢。
func TestParseChannelKeepsNoticeLikePostWithLink(t *testing.T) {
	page := `<main>` +
		`<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="QukanMovie/3">` +
		`<div class="tgme_widget_message_text">关于沙丘：预言 的合集 ` +
		`<a href="https://115.com/s/swhbjqa3zrk?password=8013">115</a></div></div></div>` +
		`</main>`
	items := parseChannel(page)
	if len(items) != 1 || items[0].ShareCode != "swhbjqa3zrk" {
		t.Fatalf("带 115 分享的帖子被误删: %+v", items)
	}
}
