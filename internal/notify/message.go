package notify

import (
	"strings"
	"time"
)

// Message 一条待推送消息。
type Message struct {
	Kind    Kind   `json:"kind,omitempty"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Image   string `json:"image,omitempty"` // http(s) 海报地址
	Link    string `json:"link,omitempty"`
	// ImageFile 是本地图片路径。只有能直传文件的渠道（Telegram）用得上：
	// 家用部署的海报没有公网地址，靠 URL 发图必然失败，读盘上传才靠谱。
	// 其它渠道看到这个字段直接忽略，退回纯文本。
	ImageFile string    `json:"-"`
	At        time.Time `json:"time,omitempty"`
	// Force 测试用：忽略 IYUU / Webhook 的类型勾选，强制发送。
	Force bool `json:"force,omitempty"`
}

// Text 返回标题 + 正文合成的一段文本。
func (m Message) Text() string {
	return strings.TrimSpace(strings.TrimSpace(m.Title) + "\n" + strings.TrimSpace(m.Content))
}

// normalized 补齐时间、清洗非 http 的图片地址。
func (m Message) normalized() Message {
	if m.At.IsZero() {
		m.At = time.Now()
	}
	if m.Kind == "" {
		m.Kind = KindSystem
	}
	if !strings.HasPrefix(m.Image, "http://") && !strings.HasPrefix(m.Image, "https://") {
		m.Image = ""
	}
	return m
}

// Article 图文卡片单篇（企业微信 news 最多 8 篇）。
type Article struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	PicURL      string `json:"picurl,omitempty"`
	URL         string `json:"url,omitempty"`
}

// Result 单个渠道的发送结果。
type Result struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}
