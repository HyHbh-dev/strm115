package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Kind 消息类型。IYUU / 通用 Webhook 会按各渠道 Types 勾选过滤，其余渠道不区分。
type Kind string

const (
	KindIngest    Kind = "ingest"    // 入库
	KindOrganize  Kind = "organize"  // 整理
	KindTransfer  Kind = "transfer"  // 下载转存
	KindSubscribe Kind = "subscribe" // 订阅
	KindDelete    Kind = "delete"    // 删除
	KindEmby      Kind = "emby"      // 媒体服务器事件
	KindCheckin   Kind = "checkin"   // 签到
	KindTask      Kind = "task"      // 定时任务
	KindSystem    Kind = "system"    // 系统
	KindBot       Kind = "bot"       // 机器人回复：永远不推 IYUU / Webhook
)

// KindNames 是类型中文名，写进 Webhook 默认载荷的 type_name 字段。
var KindNames = map[Kind]string{
	KindIngest: "入库", KindOrganize: "整理", KindTransfer: "下载转存",
	KindSubscribe: "订阅", KindDelete: "删除", KindEmby: "媒体服务器事件",
	KindCheckin: "签到", KindTask: "定时任务", KindSystem: "系统", KindBot: "机器人回复",
}

// Switch 兼容布尔 / 字符串 / 数字的开关，方便直接吃旧配置里的 "true"。
type Switch bool

// On 返回开关状态。
func (s Switch) On() bool { return bool(s) }

// UnmarshalJSON 支持 true、1、"true"、"on"、"yes"。
func (s *Switch) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return s.set(v)
}

// UnmarshalYAML 让 gopkg.in/yaml.v3 也能解析字符串开关（不引入该依赖）。
func (s *Switch) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	return s.set(v)
}

func (s *Switch) set(v any) error {
	switch t := v.(type) {
	case bool:
		*s = Switch(t)
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "on", "yes", "enabled":
			*s = true
		default:
			*s = false
		}
	case float64:
		*s = t != 0
	case int:
		*s = t != 0
	case nil:
		*s = false
	default:
		return fmt.Errorf("notify: 开关值类型不支持: %T", v)
	}
	return nil
}

// Config 全部渠道共用一份配置。零值表示所有渠道关闭。
// 字段 tag 对齐 115-Station 的 message 配置，可直接反序列化原有 JSON / YAML。
type Config struct {
	Wecom      WecomConfig      `json:"wecom" yaml:"wecom"`
	DingTalk   DingTalkConfig   `json:"dingtalk" yaml:"dingtalk"`
	TG         TGConfig         `json:"tg" yaml:"tg"`
	Feishu     FeishuConfig     `json:"feishu" yaml:"feishu"`
	QQOneBot   QQOneBotConfig   `json:"qq_onebot" yaml:"qq_onebot"`
	QQOfficial QQOfficialConfig `json:"qq_official" yaml:"qq_official"`
	Iyuu       IyuuConfig       `json:"iyuu" yaml:"iyuu"`
	Webhook    WebhookConfig    `json:"webhook" yaml:"webhook"`

	// HTTPTimeoutSec 单次请求超时秒数，0 = 15 秒。
	HTTPTimeoutSec int `json:"http_timeout_sec,omitempty" yaml:"http_timeout_sec,omitempty"`
	// ProxyURL 全局 HTTP 代理（Telegram 海外部署常用），空 = 直连。
	ProxyURL string `json:"proxy_url,omitempty" yaml:"proxy_url,omitempty"`
	// IyuuGapSec IYUU 两次发送最小间隔秒数，0 = 5 秒（IYUU 连发会被拒）。
	IyuuGapSec int `json:"iyuu_gap_sec,omitempty" yaml:"iyuu_gap_sec,omitempty"`
}

// WecomConfig 企业微信自建应用。推送用 CorpID/Secret/AgentID；
// Token/EncodingAESKey 只在启用双向机器人回调时才需要。
type WecomConfig struct {
	Enabled        Switch `json:"enabled" yaml:"enabled"`
	CorpID         string `json:"corp_id" yaml:"corp_id"`
	Secret         string `json:"secret" yaml:"secret"`
	AgentID        string `json:"agent_id" yaml:"agent_id"`
	APIURL         string `json:"api_url" yaml:"api_url"`
	Token          string `json:"token" yaml:"token"`
	EncodingAESKey string `json:"encoding_aes_key" yaml:"encoding_aes_key"`
}

// APIBase 返回企微 API 根地址，未配置时用官方地址。
func (c WecomConfig) APIBase() string {
	if s := strings.TrimRight(strings.TrimSpace(c.APIURL), "/"); s != "" {
		return s
	}
	return "https://qyapi.weixin.qq.com"
}

// DingTalkConfig 钉钉群自定义机器人。Secret 非空时自动加签；
// Keyword 非空时自动前置到正文，用于通过「自定义关键词」安全校验。
type DingTalkConfig struct {
	Enabled   Switch   `json:"enabled" yaml:"enabled"`
	Webhook   string   `json:"webhook" yaml:"webhook"`
	Secret    string   `json:"secret" yaml:"secret"`
	Keyword   string   `json:"keyword" yaml:"keyword"`
	AtMobiles []string `json:"at_mobiles,omitempty" yaml:"at_mobiles,omitempty"`
	AtAll     bool     `json:"at_all,omitempty" yaml:"at_all,omitempty"`
}

// TGConfig Telegram Bot。
type TGConfig struct {
	Enabled Switch `json:"enabled" yaml:"enabled"`
	Token   string `json:"token" yaml:"token"`
	ChatID  string `json:"chat_id" yaml:"chat_id"`
}

// FeishuConfig 飞书 / Lark 群自定义机器人，Secret 非空时附加签名。
type FeishuConfig struct {
	Enabled Switch `json:"enabled" yaml:"enabled"`
	Webhook string `json:"webhook" yaml:"webhook"`
	Secret  string `json:"secret" yaml:"secret"`
}

// QQOneBotConfig NapCat / Lagrange / LLOneBot 暴露的 HTTP API。
type QQOneBotConfig struct {
	Enabled    Switch `json:"enabled" yaml:"enabled"`
	URL        string `json:"url" yaml:"url"`
	Token      string `json:"token" yaml:"token"`
	TargetType string `json:"target_type" yaml:"target_type"` // group / private
	Target     string `json:"target" yaml:"target"`
}

// QQOfficialConfig QQ 官方机器人（q.qq.com）。
type QQOfficialConfig struct {
	Enabled   Switch `json:"enabled" yaml:"enabled"`
	AppID     string `json:"app_id" yaml:"app_id"`
	Secret    string `json:"secret" yaml:"secret"`
	GroupID   string `json:"group_id" yaml:"group_id"`
	LastMsgID string `json:"last_msg_id,omitempty" yaml:"last_msg_id,omitempty"`
}

// IyuuConfig IYUU（爱语飞飞）微信公众号推送。Types 空 = 全推。
type IyuuConfig struct {
	Enabled Switch   `json:"enabled" yaml:"enabled"`
	Token   string   `json:"token" yaml:"token"`
	Types   []string `json:"types,omitempty" yaml:"types,omitempty"`
}

// WebhookConfig 通用 Webhook。Body 为空时 POST 默认 JSON；
// 模板可用 {{type}} {{type_name}} {{title}} {{content}} {{text}} {{image}} {{link}} {{time}}。
type WebhookConfig struct {
	Enabled Switch   `json:"enabled" yaml:"enabled"`
	URL     string   `json:"url" yaml:"url"`
	Method  string   `json:"method" yaml:"method"` // POST / GET
	Body    string   `json:"body" yaml:"body"`
	Types   []string `json:"types,omitempty" yaml:"types,omitempty"`
}

// Validate 检查「已启用但必填项缺失」的配置，返回合并后的错误说明。
// 未启用渠道不检查，便于只配部分渠道。
func (c Config) Validate() error {
	var errs []string
	add := func(cond bool, format string, a ...any) {
		if cond {
			errs = append(errs, fmt.Sprintf(format, a...))
		}
	}
	if c.Wecom.Enabled.On() {
		add(c.Wecom.CorpID == "", "wecom: 缺 corp_id")
		add(c.Wecom.Secret == "", "wecom: 缺 secret")
	}
	if c.DingTalk.Enabled.On() {
		add(strings.TrimSpace(c.DingTalk.Webhook) == "", "dingtalk: 缺 webhook")
	}
	if c.TG.Enabled.On() {
		add(c.TG.Token == "", "tg: 缺 token")
		add(c.TG.ChatID == "", "tg: 缺 chat_id")
	}
	if c.Feishu.Enabled.On() {
		add(strings.TrimSpace(c.Feishu.Webhook) == "", "feishu: 缺 webhook")
	}
	if c.QQOneBot.Enabled.On() {
		add(strings.TrimSpace(c.QQOneBot.URL) == "", "qq_onebot: 缺 url")
		add(strings.TrimSpace(c.QQOneBot.Target) == "", "qq_onebot: 缺 target")
	}
	if c.QQOfficial.Enabled.On() {
		add(c.QQOfficial.AppID == "", "qq_official: 缺 app_id")
		add(c.QQOfficial.Secret == "", "qq_official: 缺 secret")
		add(c.QQOfficial.GroupID == "", "qq_official: 缺 group_id")
	}
	if c.Iyuu.Enabled.On() {
		add(strings.TrimSpace(c.Iyuu.Token) == "", "iyuu: 缺 token")
	}
	if c.Webhook.Enabled.On() {
		add(strings.TrimSpace(c.Webhook.URL) == "", "webhook: 缺 url")
	}
	if len(errs) > 0 {
		return fmt.Errorf("notify: 配置不完整: %s", strings.Join(errs, "; "))
	}
	return nil
}

// kindWanted 类型过滤：机器人回复一律不推；未勾选 = 全推。
func kindWanted(types []string, k Kind) bool {
	if k == KindBot {
		return false
	}
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if t == string(k) {
			return true
		}
	}
	return false
}
