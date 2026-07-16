package beaktelegram

import "context"

type API interface {
	RegisterChannel(Channel) error
}

type Plugin struct{}

type Channel struct{}

type Metadata struct {
	ID          string
	Platform    string
	Label       string
	Description string
}

type Capabilities struct {
	DirectChat     bool
	GroupChat      bool
	Text           bool
	Media          bool
	BlockStreaming bool
}

type SettingsSchema struct {
	Type                 string         `json:"type"`
	AdditionalProperties bool           `json:"additionalProperties"`
	Properties           map[string]any `json:"properties"`
	Required             []string       `json:"required,omitempty"`
}

func New() Plugin {
	return Plugin{}
}

func Register(api API) error {
	return New().Register(api)
}

func (Plugin) Register(api API) error {
	return api.RegisterChannel(Channel{})
}

func (Plugin) Channel() Channel {
	return Channel{}
}

func (Channel) Metadata() Metadata {
	return Metadata{
		ID:          ID,
		Platform:    Platform,
		Label:       "Telegram",
		Description: "Telegram connector for Beak channel gateway sessions",
	}
}

func (Channel) Capabilities() Capabilities {
	return Capabilities{
		DirectChat:     true,
		GroupChat:      true,
		Text:           true,
		Media:          false,
		BlockStreaming: false,
	}
}

func (Channel) SettingsSchema() SettingsSchema {
	return SettingsSchema{
		Type:                 "object",
		AdditionalProperties: false,
		Required: []string{
			"bot_token",
			"webhook_secret",
		},
		Properties: map[string]any{
			"bot_token": map[string]any{
				"type":        "string",
				"title":       "Bot Token",
				"description": "Bot token issued by BotFather when the bot was created (format: 123456:ABCdef...)",
				"secret":      true,
			},
			"webhook_secret": map[string]any{
				"type":        "string",
				"title":       "Webhook Secret Token",
				"description": "Secret token sent in X-Telegram-Bot-Api-Secret-Token header for inbound webhook verification (1-256 characters, A-Z a-z 0-9 _ -)",
				"secret":      true,
			},
		},
	}
}

// CheckHealth reports plugin-level readiness. The compatibility layer carries
// no credential, so this is a static check: the connector is registered and
// ready. Per-account credential health is verified by Connector.ValidateCredential.
func (Channel) CheckHealth(context.Context) error {
	return nil
}
