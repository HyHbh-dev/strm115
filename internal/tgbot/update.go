package tgbot

// Telegram Bot API 的入站结构。
//
// 只声明这个 bot 真正读的字段：Bot API 的 Update 有十几种变体，
// 全量声明一遍除了让代码变长没有任何好处 —— 没声明的字段会被忽略。

// Update 是 getUpdates 返回的一条更新。
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	EditedMessage *Message       `json:"edited_message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Message 只保留能触发动作的字段。
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	Date      int64  `json:"date"`
}

// Text 返回可处理的文本：普通消息在 Text，图片/文件在 Caption。
func (m *Message) Content() string {
	if m == nil {
		return ""
	}
	if s := m.Text; s != "" {
		return s
	}
	return m.Caption
}

// Chat 是会话。私聊是 ID=用户ID，群聊是负数。
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // private / group / supergroup / channel
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// Label 用来在回复里称呼这个会话。
func (c Chat) Label() string {
	switch {
	case c.Title != "":
		return c.Title
	case c.Username != "":
		return "@" + c.Username
	case c.FirstName != "":
		return c.FirstName
	}
	return "你"
}

// User 是发送者。
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// CallbackQuery 是内联键盘按钮被点。
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// InlineKeyboard 是内联键盘：外层是行，内层是这一行的按钮。
type InlineKeyboard [][]Button

// Button 一个按钮。CallbackData 上限 64 字节，所以里面只放序号，
// 真正的数据在内存里的会话状态中。
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// BotCommand 是 / 菜单里的一条。
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}
