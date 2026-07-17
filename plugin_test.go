package beaktelegram

import "testing"

type fakeAPI struct {
	channel Channel
}

func (f *fakeAPI) RegisterChannel(channel Channel) error {
	f.channel = channel
	return nil
}

func TestPluginMetadata(t *testing.T) {
	api := &fakeAPI{}
	if err := Register(api); err != nil {
		t.Fatal(err)
	}
	meta := api.channel.Metadata()
	if meta.ID != ID || meta.Platform != Platform {
		t.Fatalf("metadata=%+v", meta)
	}
}

func TestPluginSettingsSchema(t *testing.T) {
	schema := Channel{}.SettingsSchema()
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("schema=%+v", schema)
	}
	if _, ok := schema.Properties["bot_token"]; !ok {
		t.Fatalf("settings schema missing field bot_token")
	}
	if _, ok := schema.Properties["webhook_secret"]; ok {
		t.Fatal("settings schema must not expose SDK-generated webhook_secret")
	}
	required := make(map[string]bool, len(schema.Required))
	for _, key := range schema.Required {
		required[key] = true
	}
	if !required["bot_token"] || len(required) != 1 {
		t.Fatalf("required=%v must contain only bot_token", schema.Required)
	}
	// SettingsSchema must be config-driven: no Lark-specific or backend keys.
	for _, banned := range []string{"app_id", "app_secret", "verification_token", "encrypt_key", "bot_open_id", "brand", "base_url", "callback_url"} {
		if _, ok := schema.Properties[banned]; ok {
			t.Fatalf("settings schema leaks unexpected field %q", banned)
		}
	}
}
