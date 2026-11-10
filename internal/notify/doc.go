// Package notify 是零外部依赖的消息推送库：把一条消息扇出到多个 IM / Webhook
// 渠道，所有渠道都用标准库 net/http 直接调用官方 HTTP API。
//
// 内置渠道：企业微信自建应用、钉钉自定义机器人、Telegram、飞书群机器人、
// QQ OneBot、QQ 官方机器人、IYUU（爱语飞飞）、通用 Webhook。
//
// 在 strm115 里它负责「干完了告诉你」：同步结束、离线下载完成或失败。
// 配置存 settings 表的 notify_config 键（JSON），由 httpapi 层读写；
// 渠道密钥在读取时打码，前端拿不到明文。
//
// 最简用法：
//
//	c := notify.New(notify.Config{
//	    DingTalk: notify.DingTalkConfig{Enabled: true, Webhook: "https://oapi.dingtalk.com/robot/send?access_token=...", Secret: "SEC..."},
//	})
//	results := c.Send(context.Background(), notify.Message{
//	    Kind: notify.KindIngest, Title: "入库完成", Content: "沙丘 2 已入库",
//	})
//
// 配置结构刻意与 115-Station 的 message 配置保持同名同 tag（wecom / tg /
// feishu / qq_onebot / qq_official / iyuu / webhook），旧配置可以直接反序列化；
// Enabled 字段使用 Switch 类型，布尔值、字符串 "true"、数字 1 都能识别。
//
// 对外只暴露 Client.Send / SendRich；HTTP 接口由 internal/httpapi 统一提供，
// 这里不再自带一套 REST handler。接新渠道时实现 Channel 接口并调用 Register，
// 所有网络调用都走 Doer 接口，便于用 httptest 做单元测试。
package notify
