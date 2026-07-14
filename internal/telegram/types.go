package telegram

// BotInfo is the normalized identity returned by Validate.
type BotInfo struct {
	AccountID   string
	BotID       string
	BotUserID   string
	DisplayName string
	BotName     string
}

// getMeResponse models the Telegram /getMe response.
// On success, ok=true and result carries the bot User.
type getMeResponse struct {
	OK     bool    `json:"ok"`
	Result botUser `json:"result"`
}

// botUser is the Telegram User object as returned by getMe.
type botUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// sendMessageResponse models the Telegram /sendMessage response.
type sendMessageResponse struct {
	OK     bool              `json:"ok"`
	Result sendMessageResult `json:"result"`
}

type sendMessageResult struct {
	MessageID int64 `json:"message_id"`
}

// Update is the Telegram Update object delivered to the webhook.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message is the Telegram Message object.
type Message struct {
	MessageID int64           `json:"message_id"`
	From      *User           `json:"from,omitempty"`
	Chat      Chat            `json:"chat"`
	Text      string          `json:"text,omitempty"`
	Entities  []MessageEntity `json:"entities,omitempty"`
}

// MessageEntity is a Telegram special entity in the message text. A "mention"
// entity covers an @username substring; a "text_mention" entity carries an
// explicit User (used for users without a username).
type MessageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *User  `json:"user,omitempty"`
}

// User is a Telegram user or bot.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat is a Telegram chat object.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
}
