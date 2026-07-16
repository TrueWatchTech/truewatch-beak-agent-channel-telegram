package telegram

// BotInfo is the normalized identity returned by Validate.
type BotInfo struct {
	AccountID   string
	BotID       string
	BotUserID   string
	Username    string
	DisplayName string
	BotName     string
}

// getMeResponse models the Telegram /getMe response.
// On success, ok=true and result carries the bot User.
type getMeResponse struct {
	OK          bool    `json:"ok"`
	Result      botUser `json:"result"`
	ErrorCode   int     `json:"error_code,omitempty"`
	Description string  `json:"description,omitempty"`
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
	OK          bool              `json:"ok"`
	Result      sendMessageResult `json:"result"`
	ErrorCode   int               `json:"error_code,omitempty"`
	Description string            `json:"description,omitempty"`
}

type sendMessageResult struct {
	MessageID int64 `json:"message_id"`
}

// Update is the Telegram Update object delivered to the webhook.
type Update struct {
	UpdateID              int64    `json:"update_id"`
	Message               *Message `json:"message,omitempty"`
	EditedMessage         *Message `json:"edited_message,omitempty"`
	ChannelPost           *Message `json:"channel_post,omitempty"`
	EditedChannelPost     *Message `json:"edited_channel_post,omitempty"`
	BusinessMessage       *Message `json:"business_message,omitempty"`
	EditedBusinessMessage *Message `json:"edited_business_message,omitempty"`
}

// Message is the Telegram Message object.
type Message struct {
	MessageID            int64                `json:"message_id"`
	MessageThreadID      int64                `json:"message_thread_id,omitempty"`
	DirectMessagesTopic  *DirectMessagesTopic `json:"direct_messages_topic,omitempty"`
	BusinessConnectionID string               `json:"business_connection_id,omitempty"`
	Date                 int64                `json:"date,omitempty"`
	From                 *User                `json:"from,omitempty"`
	SenderChat           *Chat                `json:"sender_chat,omitempty"`
	SenderBusinessBot    *User                `json:"sender_business_bot,omitempty"`
	Chat                 Chat                 `json:"chat"`
	ReplyToMessage       *Message             `json:"reply_to_message,omitempty"`
	Text                 string               `json:"text,omitempty"`
	Entities             []MessageEntity      `json:"entities,omitempty"`
	Caption              string               `json:"caption,omitempty"`
	CaptionEntities      []MessageEntity      `json:"caption_entities,omitempty"`
	Photo                []PhotoSize          `json:"photo,omitempty"`
	Document             *Document            `json:"document,omitempty"`
	Video                *Media               `json:"video,omitempty"`
	Audio                *Media               `json:"audio,omitempty"`
	Voice                *Media               `json:"voice,omitempty"`
	Animation            *Media               `json:"animation,omitempty"`
	Sticker              *Media               `json:"sticker,omitempty"`
}

// DirectMessagesTopic identifies one user topic inside a channel direct
// messages chat. Telegram requires its topic_id on every outbound reply.
type DirectMessagesTopic struct {
	TopicID int64 `json:"topic_id"`
	User    *User `json:"user,omitempty"`
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
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Chat is a Telegram chat object.
type Chat struct {
	ID               int64  `json:"id"`
	Type             string `json:"type"` // "private", "group", "supergroup", "channel"
	Title            string `json:"title,omitempty"`
	Username         string `json:"username,omitempty"`
	FirstName        string `json:"first_name,omitempty"`
	LastName         string `json:"last_name,omitempty"`
	IsDirectMessages bool   `json:"is_direct_messages,omitempty"`
}

type PhotoSize struct {
	FileID       string `json:"file_id,omitempty"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

type Document struct {
	FileID       string `json:"file_id,omitempty"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
}

type Media struct {
	FileID       string `json:"file_id,omitempty"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
}

type setMessageReactionResponse struct {
	OK          bool   `json:"ok"`
	Result      bool   `json:"result"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
}
