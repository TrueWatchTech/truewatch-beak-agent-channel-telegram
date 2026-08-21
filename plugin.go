package beaktelegram

import (
	"context"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

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
	MediaKinds     []string
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
		Media:          true,
		MediaKinds:     []string{sdk.MediaKindImage, sdk.MediaKindFile, sdk.MediaKindAudio, sdk.MediaKindVideo, sdk.MediaKindSticker},
		BlockStreaming: false,
	}
}

func (Channel) SettingsSchema() SettingsSchema {
	return SettingsSchema{
		Type:                 "object",
		AdditionalProperties: false,
		Required: []string{
			"bot_token",
		},
		Properties: map[string]any{
			"bot_token": map[string]any{
				"type":        "string",
				"title":       "Bot Token",
				"description": "Bot token issued by BotFather when the bot was created (format: 123456:ABCdef...)",
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
