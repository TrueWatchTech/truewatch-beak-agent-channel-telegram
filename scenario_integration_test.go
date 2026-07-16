package beaktelegram

import (
	"context"
	"testing"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

// TestScenario_DirectAndGroupDoNotShareSession: the same account receiving a
// direct message and a group message must get distinct sessions.
func TestScenario_DirectAndGroupDoNotShareSession(t *testing.T) {
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()
	account := sdkAccount("acct-1")
	rt := makeRuntime(gw, store)

	direct, err := c.HandleWebhook(context.Background(), rt, account, telegramUpdateBuilder{
		UpdateID: 1, MessageID: 201, ChatID: 100, ChatType: "private",
		FromID: 999, Text: "hi",
	}.bytes())
	if err != nil {
		t.Fatalf("direct: %v", err)
	}

	group, err := c.HandleWebhook(context.Background(), rt, account, telegramUpdateBuilder{
		UpdateID: 2, MessageID: 202, ChatID: -300, ChatType: "group",
		FromID: 999, Text: "hello",
	}.bytes())
	if err != nil {
		t.Fatalf("group: %v", err)
	}

	if direct.SessionUUID == "" || group.SessionUUID == "" {
		t.Fatalf("missing session uuids: %q %q", direct.SessionUUID, group.SessionUUID)
	}
	if direct.SessionUUID == group.SessionUUID {
		t.Fatalf("direct and group must not share session: %q", direct.SessionUUID)
	}
}

// TestScenario_TwoAccountsSameGroupDoNotShareSession guards the core contract:
// session identity includes the account uuid, so two bots in the same group
// must not share a Beak session.
func TestScenario_TwoAccountsSameGroupDoNotShareSession(t *testing.T) {
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()

	// Same group chat, same sender, same message content — but different bot accounts.
	body := telegramUpdateBuilder{
		UpdateID: 3, MessageID: 203, ChatID: -400, ChatType: "group",
		FromID: 999, Text: "hello team",
	}.bytes()

	accountA := sdk.ChannelAccount{
		UUID:     "acct-A",
		Platform: Platform,
		Credential: map[string]any{
			"account_id": "111111",
			"bot_id":     "111111",
			"bot_token":  testBotToken,
		},
	}
	accountB := sdk.ChannelAccount{
		UUID:     "acct-B",
		Platform: Platform,
		Credential: map[string]any{
			"account_id": "222222",
			"bot_id":     "222222",
			"bot_token":  testBotToken,
		},
	}

	resA, err := c.HandleWebhook(context.Background(), makeRuntime(gw, store), accountA, body)
	if err != nil {
		t.Fatalf("account A: %v", err)
	}
	resB, err := c.HandleWebhook(context.Background(), makeRuntime(gw, store), accountB, body)
	if err != nil {
		t.Fatalf("account B: %v", err)
	}
	if resA.SessionUUID == "" || resB.SessionUUID == "" {
		t.Fatalf("missing session uuids: %q %q", resA.SessionUUID, resB.SessionUUID)
	}
	if resA.SessionUUID == resB.SessionUUID {
		t.Fatalf("two accounts in the same group must not share session: %q", resA.SessionUUID)
	}
}

// TestScenario_DuplicateEventNotDuplicated: replaying the same event must be
// ignored and must not create a second Beak message.
func TestScenario_DuplicateEventNotDuplicated(t *testing.T) {
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()
	account := sdkAccount("acct-1")
	rt := makeRuntime(gw, store)

	body := telegramUpdateBuilder{
		UpdateID: 4, MessageID: 204, ChatID: -400, ChatType: "group",
		FromID: 999, Text: "once",
	}.bytes()

	first, err := c.HandleWebhook(context.Background(), rt, account, body)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Ignored {
		t.Fatalf("first delivery must not be ignored")
	}

	second, err := c.HandleWebhook(context.Background(), rt, account, body)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Ignored || second.Reason != "duplicate" {
		t.Fatalf("second delivery must be ignored as duplicate, got ignored=%v reason=%q", second.Ignored, second.Reason)
	}
	if got := gw.messageCount(); got != 1 {
		t.Fatalf("expected exactly 1 message created, got %d", got)
	}
}

// TestScenario_MarkdownOutboundSentOrDegraded: a markdown outbound message is
// delivered and returns the platform message id.
func TestScenario_MarkdownOutboundSentOrDegraded(t *testing.T) {
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()
	account := sdkAccount("acct-1")
	rt := makeRuntime(gw, store, account)
	rt.HTTPClient = httpClientReturning(sendMessageOK(999))

	res, err := c.Send(context.Background(), rt, sdk.OutboundMessage{
		AccountUUID: "acct-1",
		ChatID:      "-400",
		Text:        "*bold* message",
		Format:      "markdown",
	})
	if err != nil {
		t.Fatalf("send markdown: %v", err)
	}
	if res.MessageID == "" {
		t.Fatalf("expected a message id from sendMessage")
	}
}
