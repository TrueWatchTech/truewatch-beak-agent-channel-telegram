package beaktelegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

func TestInbound_UTF16MentionAfterEmoji(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 201,
		"message": map[string]any{
			"message_id": 301,
			"from":       map[string]any{"id": 999, "first_name": "Alice"},
			"chat":       map[string]any{"id": -200, "type": "group", "title": "Ops"},
			"text":       "😀 hi @testbot",
			"entities": []map[string]any{{
				"type": "mention", "offset": 6, "length": 8,
			}},
		},
	})
	if res.Ignored || res.Inbound == nil {
		t.Fatalf("expected inbound message, got %#v", res)
	}
	if !res.Inbound.MentionedMe || res.Inbound.Text != "😀 hi" {
		t.Fatalf("mention normalization failed: %#v", res.Inbound)
	}
	if len(res.Inbound.Mentions) != 1 || res.Inbound.Mentions[0].ID != testBotUsername {
		t.Fatalf("mention identities=%#v", res.Inbound.Mentions)
	}
}

func TestInbound_PureBotMentionIsFollowUp(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 202,
		"message": map[string]any{
			"message_id": 302,
			"from":       map[string]any{"id": 999, "first_name": "Alice"},
			"chat":       map[string]any{"id": -200, "type": "group"},
			"text":       "@testbot",
			"entities":   []map[string]any{{"type": "mention", "offset": 0, "length": 8}},
		},
	})
	if res.Ignored || res.Inbound == nil || !res.Inbound.MentionedMe || res.Inbound.Text != "" {
		t.Fatalf("pure mention must reach Beak as a follow-up: %#v", res)
	}
}

func TestInbound_CaptionedPhoto(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 203,
		"message": map[string]any{
			"message_id": 303,
			"from":       map[string]any{"id": 999, "first_name": "Alice"},
			"chat":       map[string]any{"id": -200, "type": "group"},
			"caption":    "inspect this",
			"photo":      []map[string]any{{"file_id": "photo-1", "width": 10, "height": 10}},
		},
	})
	if res.Ignored || res.Inbound == nil || res.Inbound.Text != "inspect this" {
		t.Fatalf("captioned media must expose its caption: %#v", res)
	}
	if res.Inbound.Raw["message_type"] != "photo" {
		t.Fatalf("message_type=%#v", res.Inbound.Raw["message_type"])
	}
}

func TestInbound_ThreadReplyAndDisplayNames(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 204,
		"message": map[string]any{
			"message_id":        304,
			"message_thread_id": 77,
			"date":              1720000000,
			"from":              map[string]any{"id": 999, "first_name": "Alice", "last_name": "Wu"},
			"chat":              map[string]any{"id": -200, "type": "supergroup", "title": "Ops Room"},
			"text":              "what happened?",
			"reply_to_message": map[string]any{
				"message_id":        300,
				"message_thread_id": 77,
				"date":              1719990000,
				"from":              map[string]any{"id": 888, "first_name": "Bob"},
				"chat":              map[string]any{"id": -200, "type": "supergroup", "title": "Ops Room"},
				"text":              "original incident",
			},
		},
	})
	in := res.Inbound
	if res.Ignored || in == nil {
		t.Fatalf("expected inbound: %#v", res)
	}
	if in.ThreadID != "77" || in.ChatDisplayName != "Ops Room" || in.SenderDisplayName != "Alice Wu" {
		t.Fatalf("identity/thread mismatch: %#v", in)
	}
	if in.ChatIdentity.ID != "-200" || in.ChatIdentity.IDType != "telegram_chat_id" {
		t.Fatalf("chat identity=%#v", in.ChatIdentity)
	}
	if in.ReferencedMessage == nil || in.ReferencedMessage.MessageID != "300" || in.ReferencedMessage.Text != "original incident" || in.ReferencedMessage.SenderDisplayName != "Bob" {
		t.Fatalf("referenced message=%#v", in.ReferencedMessage)
	}
}

func TestInbound_ChannelPostPrefersSenderChat(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 205,
		"channel_post": map[string]any{
			"message_id":  305,
			"from":        map[string]any{"id": 777000, "is_bot": true, "first_name": "Channel"},
			"sender_chat": map[string]any{"id": -900, "type": "channel", "title": "Alerts"},
			"chat":        map[string]any{"id": -900, "type": "channel", "title": "Alerts"},
			"text":        "service degraded",
		},
	})
	if res.Ignored || res.Type != "channel_post" || res.Inbound == nil || res.Inbound.SenderID != "-900" || res.Inbound.ChatDisplayName != "Alerts" {
		t.Fatalf("channel post normalization failed: %#v", res)
	}
}

func TestInbound_BusinessMessagePreservesConnectionAndFiltersSelfEcho(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 206,
		"business_message": map[string]any{
			"message_id":             306,
			"business_connection_id": "bc:1",
			"from":                   map[string]any{"id": 999, "first_name": "Customer"},
			"chat":                   map[string]any{"id": 4242, "type": "private", "first_name": "Customer"},
			"text":                   "need help",
		},
	})
	if res.Ignored || res.Type != "business_message" || res.Inbound == nil {
		t.Fatalf("business message normalization failed: %#v", res)
	}
	wantChatID := telegramInboundChatID("bc:1", "4242", "")
	if res.Inbound.ChatID != wantChatID || res.Inbound.ChatIdentity.IDType != "telegram_business_chat_id" {
		t.Fatalf("business chat identity=%#v", res.Inbound)
	}
	if res.Inbound.Raw["business_connection_id"] != "bc:1" || res.Inbound.Raw["telegram_chat_id"] != "4242" {
		t.Fatalf("business routing metadata=%#v", res.Inbound.Raw)
	}

	self := inboundJSON(t, map[string]any{
		"update_id": 207,
		"business_message": map[string]any{
			"message_id":             307,
			"business_connection_id": "bc:1",
			"from":                   map[string]any{"id": 999, "first_name": "Business"},
			"sender_business_bot":    map[string]any{"id": 123456, "is_bot": true, "first_name": "Test Bot"},
			"chat":                   map[string]any{"id": 4242, "type": "private"},
			"text":                   "our own reply",
		},
	})
	if !self.Ignored || self.Reason != "self_echo" {
		t.Fatalf("business self echo was not filtered: %#v", self)
	}
}

func TestSend_BusinessMessageRestoresPlatformTarget(t *testing.T) {
	var payload map[string]any
	client := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode send payload: %v", err)
		}
		return testJSONResponseObj(map[string]any{"ok": true, "result": map[string]any{"message_id": 501}})
	})}
	account := sdkAccount("acct-1")
	result, err := (Connector{}).Send(context.Background(), sdk.Runtime{
		Account: account, Accounts: []sdk.ChannelAccount{account}, HTTPClient: client, AccountStore: newFakeSDKAccountStore(),
	}, sdk.OutboundMessage{
		AccountUUID: "acct-1", ChatType: sdk.ChatTypeDirect,
		MessageUUID: "message-business-1", ChatID: telegramInboundChatID("bc:1", "4242", ""), Text: "reply",
	})
	if err != nil {
		t.Fatalf("send business reply: %v", err)
	}
	if payload["chat_id"] != "4242" || payload["business_connection_id"] != "bc:1" || result.MessageID != "501" {
		t.Fatalf("business send payload=%#v result=%#v", payload, result)
	}
}

func TestInbound_DirectMessagesTopicUsesIsolatedCommonIdentity(t *testing.T) {
	res := inboundJSON(t, map[string]any{
		"update_id": 208,
		"message": map[string]any{
			"message_id": 308,
			"from":       map[string]any{"id": 999, "first_name": "Customer"},
			"chat":       map[string]any{"id": -900, "type": "channel", "title": "Support", "is_direct_messages": true},
			"direct_messages_topic": map[string]any{
				"topic_id": 88,
				"user":     map[string]any{"id": 999, "first_name": "Customer"},
			},
			"text": "need help",
		},
	})
	if res.Ignored || res.Inbound == nil {
		t.Fatalf("direct messages topic normalization failed: %#v", res)
	}
	wantChatID := telegramInboundChatID("", "-900", "88")
	if res.Inbound.ChatType != sdk.ChatTypeDirect || res.Inbound.ChatID != wantChatID || res.Inbound.ThreadID != directThreadIDPrefix+"88" {
		t.Fatalf("direct messages identity=%#v", res.Inbound)
	}
	if res.Inbound.ChatIdentity.IDType != "telegram_direct_messages_chat_id" {
		t.Fatalf("chat identity=%#v", res.Inbound.ChatIdentity)
	}
}

func TestSend_DirectMessagesTopicRestoresPlatformRoute(t *testing.T) {
	var payload map[string]any
	client := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode send payload: %v", err)
		}
		return testJSONResponseObj(map[string]any{"ok": true, "result": map[string]any{"message_id": 502}})
	})}
	account := sdkAccount("acct-1")
	result, err := (Connector{}).Send(context.Background(), sdk.Runtime{
		Account: account, Accounts: []sdk.ChannelAccount{account}, HTTPClient: client, AccountStore: newFakeSDKAccountStore(),
	}, sdk.OutboundMessage{
		AccountUUID: "acct-1", MessageUUID: "message-direct-topic-1", ChatType: sdk.ChatTypeDirect,
		ChatID: telegramInboundChatID("", "-900", "88"), ThreadID: directThreadIDPrefix + "88", Text: "reply",
	})
	if err != nil {
		t.Fatalf("send direct messages reply: %v", err)
	}
	if payload["chat_id"] != "-900" || payload["direct_messages_topic_id"] != float64(88) || result.MessageID != "502" {
		t.Fatalf("direct messages send payload=%#v result=%#v", payload, result)
	}
	if _, exists := payload["message_thread_id"]; exists {
		t.Fatalf("direct messages send used forum thread field: %#v", payload)
	}
}

func TestInbound_EditedMessageDoesNotCreateSecondAgentTurn(t *testing.T) {
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()
	body, err := json.Marshal(map[string]any{
		"update_id": 209,
		"edited_message": map[string]any{
			"message_id": 309,
			"from":       map[string]any{"id": 999, "first_name": "Alice"},
			"chat":       map[string]any{"id": -200, "type": "group"},
			"text":       "edited text",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.HandleWebhook(context.Background(), makeRuntime(gw, store), sdkAccount("acct-1"), body)
	if err != nil {
		t.Fatalf("handle edited message: %v", err)
	}
	if !result.Ignored || result.Reason != "edited_message_not_supported" || gw.messageCount() != 0 {
		t.Fatalf("edited result=%#v messages=%d", result, gw.messageCount())
	}
}

func TestSend_MultipartRetryResumesAfterPersistedPart(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		if requests == 2 {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":502,"description":"temporary"}`)),
			}, nil
		}
		messageID := 500 + requests
		return testJSONResponseObj(map[string]any{"ok": true, "result": map[string]any{"message_id": messageID}})
	})}
	account := sdkAccount("acct-multipart")
	store := newFakeSDKAccountStore()
	runtime := sdk.Runtime{Account: account, Accounts: []sdk.ChannelAccount{account}, HTTPClient: client, AccountStore: store}
	req := sdk.OutboundMessage{
		AccountUUID: account.UUID, MessageUUID: "message-multipart-1", ChatID: "-200", Text: strings.Repeat("a", 5000),
	}
	if _, err := (Connector{}).Send(context.Background(), runtime, req); err == nil {
		t.Fatal("first multipart attempt should fail on the second part")
	}
	result, err := (Connector{}).Send(context.Background(), runtime, req)
	if err != nil {
		t.Fatalf("resume multipart send: %v", err)
	}
	messageIDs, _ := result.Raw["message_ids"].([]string)
	if requests != 3 || len(messageIDs) != 2 || messageIDs[0] != "501" || messageIDs[1] != "503" {
		t.Fatalf("requests=%d result=%#v", requests, result)
	}
	if result.Raw["resumed"] != true {
		t.Fatalf("resumed diagnostic missing: %#v", result.Raw)
	}
}

func TestInbound_DedupeFallbackIncludesChatID(t *testing.T) {
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()
	runtime := makeRuntime(gw, store)
	account := sdkAccount("acct-1")
	for _, chatID := range []int64{-201, -202} {
		body := telegramUpdateBuilder{UpdateID: 0, MessageID: 500, ChatID: chatID, ChatType: "group", FromID: 999, Text: "same id"}.bytes()
		result, err := c.HandleWebhook(context.Background(), runtime, account, body)
		if err != nil || result.Ignored {
			t.Fatalf("chat %d should not collide: result=%#v err=%v", chatID, result, err)
		}
	}
	if got := gw.messageCount(); got != 2 {
		t.Fatalf("message count=%d want 2", got)
	}
}

func TestInbound_ConcurrentUpdatesPreserveState(t *testing.T) {
	const count = 20
	c := Connector{}
	gw := &fakeSDKGateway{}
	store := newFakeSDKAccountStore()
	runtime := makeRuntime(gw, store)
	account := sdkAccount("acct-concurrent")

	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			body := telegramUpdateBuilder{
				UpdateID: int64(1000 + index), MessageID: int64(2000 + index),
				ChatID: -300, ChatType: "group", FromID: int64(3000 + index), Text: "hello",
			}.bytes()
			_, err := c.HandleWebhook(context.Background(), runtime, account, body)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent webhook: %v", err)
		}
	}
	if got := gw.messageCount(); got != count {
		t.Fatalf("created messages=%d want %d", got, count)
	}
	saved, _ := store.LoadChannelAccountState(context.Background(), "acct-concurrent")
	seen, _ := saved["inbound_seen"].(map[string]any)
	if len(seen) != count {
		t.Fatalf("inbound_seen=%d want %d; state=%#v", len(seen), count, saved)
	}
	inboundAccountLocks.Lock()
	remainingLocks := len(inboundAccountLocks.items)
	inboundAccountLocks.Unlock()
	if remainingLocks != 0 {
		t.Fatalf("account locks were not released: %d", remainingLocks)
	}
}

func TestInbound_UpdatesHealthAndPreservesHostState(t *testing.T) {
	c := Connector{}
	store := newFakeSDKAccountStore()
	account := sdkAccount("acct-health")
	account.State["runtime_status"] = "running"
	account.State["stream_last_error"] = "old error"
	account.State["stream_last_error_at"] = "2026-01-01T00:00:00Z"
	body := telegramUpdateBuilder{UpdateID: 206, MessageID: 306, ChatID: -200, ChatType: "group", FromID: 999, Text: "hello"}.bytes()
	if _, err := c.HandleWebhook(context.Background(), makeRuntime(&fakeSDKGateway{}, store), account, body); err != nil {
		t.Fatalf("handle webhook: %v", err)
	}
	saved, _ := store.LoadChannelAccountState(context.Background(), "acct-health")
	if saved["stream_connection_state"] != "connected" || saved["stream_last_activity_at"] == nil || saved["stream_last_event_at"] == nil {
		t.Fatalf("runtime health not persisted: %#v", saved)
	}
	if saved["runtime_status"] != "running" {
		t.Fatalf("host state was dropped: %#v", saved)
	}
	if _, exists := saved["stream_last_error"]; exists {
		t.Fatalf("cleared SDK health error was retained: %#v", saved)
	}
}

func TestInbound_IgnoredUpdateDoesNotPersistZeroHealthTimestamps(t *testing.T) {
	store := newFakeSDKAccountStore()
	result, err := (Connector{}).HandleWebhook(
		context.Background(),
		makeRuntime(&fakeSDKGateway{}, store),
		sdkAccount("acct-ignored-health"),
		[]byte(`{"update_id":999}`),
	)
	if err != nil || result == nil || !result.Ignored {
		t.Fatalf("ignored update result=%#v err=%v", result, err)
	}
	saved, _ := store.LoadChannelAccountState(context.Background(), "acct-ignored-health")
	for _, key := range []string{
		"stream_disconnected_at", "stream_last_ping_at", "stream_last_pong_at",
		"stream_last_event_at", "stream_last_error_at", "stream_reconnect_requested_at", "stream_reconnect_error_at",
	} {
		if value, exists := saved[key]; exists {
			t.Fatalf("zero health timestamp %s=%#v must be omitted: %#v", key, value, saved)
		}
	}
}

func TestAcknowledge_Reaction(t *testing.T) {
	var path string
	var payload map[string]any
	client := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		path = req.URL.Path
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode reaction payload: %v", err)
		}
		return testJSONResponseObj(map[string]any{"ok": true, "result": true})
	})}
	account := sdkAccount("acct-1")
	result, err := (Connector{}).Acknowledge(context.Background(), sdk.Runtime{
		Account: account, Accounts: []sdk.ChannelAccount{account}, HTTPClient: client,
	}, sdk.OutboundAck{
		AccountUUID: "acct-1", ChatID: "-200", TargetMessageID: "306", Mode: "reaction", Action: "start", Intent: "thinking",
	})
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if !strings.HasSuffix(path, "/setMessageReaction") || result.Status != "sent" || result.ReactionID != "🤔" {
		t.Fatalf("reaction result=%#v path=%q payload=%#v", result, path, payload)
	}
}

func TestAcknowledge_StopReactionIsSkipped(t *testing.T) {
	called := false
	client := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return testJSONResponseObj(map[string]any{"ok": true, "result": true})
	})}
	account := sdkAccount("acct-1")
	result, err := (Connector{}).Acknowledge(context.Background(), sdk.Runtime{
		Account: account, Accounts: []sdk.ChannelAccount{account}, HTTPClient: client,
	}, sdk.OutboundAck{
		AccountUUID: "acct-1", ChatID: "-200", TargetMessageID: "306", Mode: "reaction", Action: "stop", Intent: "processing",
	})
	if err != nil {
		t.Fatalf("acknowledge stop: %v", err)
	}
	if called || result.Status != "skipped" || result.Mode != "reaction" {
		t.Fatalf("stop reaction result=%#v called=%v", result, called)
	}
}

func TestTelegramAckEmojiUsesSupportedDefaults(t *testing.T) {
	tests := []struct {
		intent string
		want   string
	}{
		{intent: "processing", want: "👀"},
		{intent: "thinking", want: "🤔"},
		{intent: "typing", want: "👨‍💻"},
		{intent: "done", want: "👍"},
		{intent: "wait", want: "🙏"},
	}
	for _, tt := range tests {
		t.Run(tt.intent, func(t *testing.T) {
			if got := telegramAckEmoji(sdk.OutboundAck{Intent: tt.intent, Action: "start"}); got != tt.want {
				t.Fatalf("emoji=%q want %q", got, tt.want)
			}
		})
	}
}

func TestWebhookErrorsAreStructured(t *testing.T) {
	c := Connector{}
	runtime := makeRuntime(&fakeSDKGateway{}, newFakeSDKAccountStore())
	account := sdkAccount("acct-1")

	_, err := c.HandleWebhookRequest(context.Background(), runtime, account, nil)
	assertWebhookError(t, err, http.StatusBadRequest, "invalid_request_body")

	oversized := signedWebhookRequest(testWebhookSecret, []byte(strings.Repeat("x", maxWebhookRequestBytes+1)))
	_, err = c.HandleWebhookRequest(context.Background(), runtime, account, oversized)
	assertWebhookError(t, err, http.StatusRequestEntityTooLarge, "request_body_too_large")

	malformed := signedWebhookRequest(testWebhookSecret, []byte("{"))
	_, err = c.HandleWebhookRequest(context.Background(), runtime, account, malformed)
	assertWebhookError(t, err, http.StatusBadRequest, "invalid_request_body")

	badSecret := signedWebhookRequest("wrong", telegramUpdateBuilder{UpdateID: 207, MessageID: 307, ChatID: -200, ChatType: "group", FromID: 999, Text: "hello"}.bytes())
	_, err = c.HandleWebhookRequest(context.Background(), runtime, account, badSecret)
	assertWebhookError(t, err, http.StatusForbidden, "channel_webhook_auth_failed")
}

func inboundJSON(t *testing.T, update map[string]any) *EventResult {
	t.Helper()
	body, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	result, err := (Connector{}).HandleWebhook(
		context.Background(),
		makeRuntime(&fakeSDKGateway{}, newFakeSDKAccountStore()),
		sdkAccount("acct-1"),
		body,
	)
	if err != nil {
		t.Fatalf("handle webhook: %v", err)
	}
	return result
}

func assertWebhookError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var webhookErr *sdk.WebhookError
	if !errors.As(err, &webhookErr) {
		t.Fatalf("error %T does not implement structured webhook error: %v", err, err)
	}
	if webhookErr.HTTPStatusCode() != status || webhookErr.ErrorCode() != code {
		t.Fatalf("webhook error status=%d code=%q want status=%d code=%q", webhookErr.HTTPStatusCode(), webhookErr.ErrorCode(), status, code)
	}
}
