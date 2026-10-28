package beaktelegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

type interactionTestGateway struct {
	*fakeSDKGateway
	action sdk.InteractionAction
}

func (g *interactionTestGateway) HandleInteraction(_ context.Context, a sdk.InteractionAction) (*sdk.InteractionResult, error) {
	g.action = a
	return &sdk.InteractionResult{State: "approved"}, nil
}

func TestTelegramInteractionSendAndAuthenticatedCallback(t *testing.T) {
	account := sdkAccount("acct-1")
	gateway := &interactionTestGateway{fakeSDKGateway: &fakeSDKGateway{}}
	runtime := makeRuntime(gateway, newFakeSDKAccountStore(), account)
	var payload map[string]any
	runtime.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendMessage") {
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			return testJSONResponseObj(map[string]any{"ok": true, "result": map[string]any{"message_id": 321}})
		}
		return testJSONResponseObj(map[string]any{"ok": true, "result": true})
	})}
	card := &sdk.InteractionCard{RequestID: "approval_123", Revision: 3, Kind: "tool_approval", State: "pending", Title: "Approval", Summary: "Run operation", Actions: []sdk.InteractionButton{{ID: "approve", Label: "Approve"}, {ID: "reject", Label: "Reject"}}}
	result, err := NewConnector().Send(context.Background(), runtime, sdk.OutboundMessage{AccountUUID: account.UUID, ChatID: "-100", Interaction: card})
	if err != nil || result.MessageID != "321" {
		t.Fatalf("send: %#v %v", result, err)
	}
	markup, ok := payload["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("missing keyboard: %#v", payload)
	}
	rows := markup["inline_keyboard"].([]any)
	button := rows[0].([]any)[0].(map[string]any)
	token := button["callback_data"].(string)
	body, _ := json.Marshal(map[string]any{"update_id": 42, "callback_query": map[string]any{"id": "callback-1", "data": token, "from": map[string]any{"id": 777}, "message": map[string]any{"message_id": 321, "chat": map[string]any{"id": -100, "type": "supergroup"}}}})
	connector := Connector{}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	if _, err := connector.HandleWebhookRequest(context.Background(), runtime, account, request); err == nil {
		t.Fatal("unsigned callback accepted")
	}
	if gateway.action.ActorID != "" {
		t.Fatal("unauthenticated actor reached host")
	}
	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", testWebhookSecret)
	if _, err := connector.HandleWebhookRequest(context.Background(), runtime, account, request); err != nil {
		t.Fatal(err)
	}
	if gateway.action.ActorID != "777" || gateway.action.ChatID != "-100" || gateway.action.RequestID != "approval_123" || gateway.action.Revision != 3 {
		t.Fatalf("incorrect platform identity: %#v", gateway.action)
	}
	if gateway.messageCount() != 0 {
		t.Fatal("callback turned into a user chat message")
	}
}

func TestTelegramInteractionPreservesBusinessAndTopicIdentity(t *testing.T) {
	account := sdkAccount("acct-1")
	for _, message := range []string{
		`{"message_id":321,"business_connection_id":"business-1","chat":{"id":100,"type":"private"}}`,
		`{"message_id":321,"direct_messages_topic":{"topic_id":88},"chat":{"id":100,"type":"private"}}`,
	} {
		gateway := &interactionTestGateway{fakeSDKGateway: &fakeSDKGateway{}}
		runtime := makeRuntime(gateway, newFakeSDKAccountStore(), account)
		runtime.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return testJSONResponseObj(map[string]any{"ok": true, "result": true})
		})}
		body := `{"callback_query":{"id":"callback-1","data":"approval_123:3:approve","from":{"id":777},"message":` + message + `}}`
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("X-Telegram-Bot-Api-Secret-Token", testWebhookSecret)
		if _, err := (Connector{}).HandleWebhookRequest(context.Background(), runtime, account, request); err != nil {
			t.Fatal(err)
		}
		expected := telegramInboundChatID("business-1", "100", "")
		if strings.Contains(message, "direct_messages_topic") {
			expected = telegramInboundChatID("", "100", "88")
		}
		if gateway.action.ChatID != expected {
			t.Fatalf("callback chat %q, expected %q", gateway.action.ChatID, expected)
		}
	}
}

func TestTelegramLongPlanUsesAuthenticatedWebEntry(t *testing.T) {
	account := sdkAccount("acct-1")
	runtime := makeRuntime(&interactionTestGateway{fakeSDKGateway: &fakeSDKGateway{}}, newFakeSDKAccountStore(), account)
	card := &sdk.InteractionCard{RequestID: "plan_123", Revision: 1, Kind: "plan_confirmation", State: "pending", Title: "计划", Summary: strings.Repeat("中文😀", 2500), WebURL: "https://console.example.test/agents/agent/workspace?session_uuid=session", Actions: []sdk.InteractionButton{{ID: "approve", Label: "批准"}}}
	calls := 0
	runtime.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if utf8.RuneCountInString(payload["text"].(string)) > 4096 {
			t.Fatal("text exceeds platform limit")
		}
		rows := payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
		if len(rows) != 1 {
			t.Fatal("truncated plan must be reviewed on Web before deciding")
		}
		button := rows[0].([]any)[0].(map[string]any)
		if button["url"] != card.WebURL || button["callback_data"] != nil {
			t.Fatal("missing Web review entry")
		}
		return testJSONResponseObj(map[string]any{"ok": true, "result": map[string]any{"message_id": 321}})
	})}
	req := sdk.OutboundMessage{AccountUUID: account.UUID, ChatID: "-100", Interaction: card}
	if _, err := NewConnector().Send(context.Background(), runtime, req); err != nil {
		t.Fatal(err)
	}
	card.WebURL = ""
	if _, err := NewConnector().Send(context.Background(), runtime, req); err == nil {
		t.Fatal("oversized plan silently truncated without a review entry")
	}
	if calls != 1 {
		t.Fatal("invalid oversized payload reached Telegram")
	}
}
