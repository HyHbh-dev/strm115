package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TelegramBot 是 getMe 的结果，用来在界面上确认「这 Token 是谁」。
type TelegramBot struct {
	ID       int64  `json:"id"`
	Name     string `json:"first_name"`
	Username string `json:"username"`
}

// TelegramChat 是从 getUpdates 里挖出来的一个会话，够填 Chat ID 就行。
type TelegramChat struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // private / group / supergroup / channel
	Title    string `json:"title"`
	Username string `json:"username"`
	// Name 是给人看的名字：私聊取 first_name，群/频道取 title。
	Name string `json:"name"`
	// Text 最后一条消息的片段，帮用户确认是哪个群。
	Text string `json:"text,omitempty"`
}

// TelegramDiscover 一次探测的全部结果。
type TelegramDiscover struct {
	Bot TelegramBot `json:"bot"`
	// Chats 去重后的会话，最近的排在前面。
	Chats []TelegramChat `json:"chats"`
	// Webhook 为真说明 Bot 设了 webhook，Telegram 会拒绝 getUpdates（409）。
	Webhook bool `json:"webhook"`
	// Hint 是给界面直接显示的一句话，比让人自己猜错在哪强。
	Hint string `json:"hint"`
}

// DiscoverTelegram 用 Token 调 getMe + getUpdates，把可用的 Chat ID 列出来。
//
// 为什么值得单独做一个接口：填 Chat ID 是配置 Telegram 时唯一需要「另一个工具」
// 的环节——用户得自己去 getUpdates 里翻 JSON。这里代劳之后，界面上只剩
// 「给 Bot 发条消息 → 点一下 → 选会话」。
//
// 走 Client 的 Doer 而不是新建 http.Client：代理配置（出海部署必备）只有一份。
func (c *Client) DiscoverTelegram(ctx context.Context, token string) (TelegramDiscover, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return TelegramDiscover{}, fmt.Errorf("telegram: 先填 Bot Token")
	}
	_, do := c.snapshot()
	base := "https://api.telegram.org/bot" + token + "/"

	out := TelegramDiscover{Chats: []TelegramChat{}}

	// getMe：Token 错、被吊销都在这一步暴露，早于后面翻消息。
	body, status, err := getJSON(ctx, do, base+"getMe")
	if err != nil {
		return out, fmt.Errorf("telegram: %s", sanitizeErr(err))
	}
	var me struct {
		OK          bool        `json:"ok"`
		Description string      `json:"description"`
		Result      TelegramBot `json:"result"`
	}
	if err := json.Unmarshal(body, &me); err != nil || !me.OK {
		return out, telegramAPIError(status, me.Description, body)
	}
	out.Bot = me.Result

	// 只取最近 100 条：够覆盖「刚发的那条消息」，也不至于把历史全拉回来。
	q := url.Values{}
	q.Set("limit", "100")
	q.Set("timeout", "0")
	q.Set("allowed_updates", `["message","channel_post","my_chat_member","edited_message"]`)
	body, status, err = getJSON(ctx, do, base+"getUpdates?"+q.Encode())
	if err != nil {
		return out, fmt.Errorf("telegram: %s", sanitizeErr(err))
	}
	var ups struct {
		OK          bool       `json:"ok"`
		ErrorCode   int        `json:"error_code"`
		Description string     `json:"description"`
		Result      []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &ups); err != nil {
		return out, fmt.Errorf("telegram: 返回不是 JSON: %s", truncate(string(body), 120))
	}
	if !ups.OK {
		// 409 = 设了 webhook，消息被推到 webhook 去了，getUpdates 拿不到。
		if ups.ErrorCode == http.StatusConflict {
			out.Webhook = true
			out.Hint = "Bot 设置了 Webhook，Telegram 不再把消息放进 getUpdates。先删掉 webhook 再试。"
			return out, nil
		}
		return out, telegramAPIError(status, ups.Description, body)
	}

	seen := map[string]bool{}
	for _, u := range ups.Result {
		ch := u.chat()
		if ch.ID == 0 {
			continue
		}
		id := fmt.Sprintf("%d", ch.ID)
		if seen[id] {
			continue
		}
		seen[id] = true
		name := ch.Title
		if name == "" {
			name = strings.TrimSpace(ch.FirstName + " " + ch.LastName)
		}
		if name == "" {
			name = ch.Username
		}
		out.Chats = append(out.Chats, TelegramChat{
			ID: id, Type: ch.Type, Title: ch.Title,
			Username: ch.Username, Name: name, Text: u.text(),
		})
	}
	// getUpdates 从旧到新，界面上把刚发过消息的会话排前面更顺手。
	for i, j := 0, len(out.Chats)-1; i < j; i, j = i+1, j-1 {
		out.Chats[i], out.Chats[j] = out.Chats[j], out.Chats[i]
	}

	switch {
	case len(out.Chats) == 0:
		out.Hint = fmt.Sprintf("还没收到任何消息。先在 Telegram 里给 @%s 发一句「hi」，再点一次。", out.Bot.Username)
	case len(out.Chats) == 1:
		out.Hint = "找到一个会话，点「用这个」填进 Chat ID。"
	default:
		out.Hint = "找到多个会话，选你要接收通知的那个。"
	}
	return out, nil
}

// telegramAPIError 把 Telegram 的 error_code / description 翻译成一句人话。
func telegramAPIError(status int, desc string, body []byte) error {
	if desc == "" {
		desc = truncate(strings.TrimSpace(string(body)), 160)
	}
	if status == http.StatusUnauthorized || strings.Contains(desc, "Unauthorized") {
		return fmt.Errorf("telegram: Token 不对或已被吊销")
	}
	return fmt.Errorf("telegram: %s", desc)
}

// tgUpdate 只解出定位会话需要的字段，其余一概不管。
type tgUpdate struct {
	Message      *tgMessage `json:"message"`
	EditedMsg    *tgMessage `json:"edited_message"`
	ChannelPost  *tgMessage `json:"channel_post"`
	MyChatMember *struct {
		Chat tgChat `json:"chat"`
	} `json:"my_chat_member"`
}

type tgMessage struct {
	Text string `json:"text"`
	Chat tgChat `json:"chat"`
}

type tgChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

func (u tgUpdate) chat() tgChat {
	for _, m := range []*tgMessage{u.Message, u.EditedMsg, u.ChannelPost} {
		if m != nil {
			return m.Chat
		}
	}
	if u.MyChatMember != nil {
		return u.MyChatMember.Chat
	}
	return tgChat{}
}

func (u tgUpdate) text() string {
	for _, m := range []*tgMessage{u.Message, u.EditedMsg, u.ChannelPost} {
		if m != nil {
			return truncate(strings.TrimSpace(m.Text), 60)
		}
	}
	return ""
}
