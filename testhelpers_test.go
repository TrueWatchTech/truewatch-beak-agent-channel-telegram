package beaktelegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/TrueWatch/beak-agent-channel-telegram/sdk"
)

const (
	testBotToken      = "123456:ABCdef-test-token"
	testWebhookSecret = "test-webhook-secret"
	testBotID         = int64(123456)
	testBotIDStr      = "123456"
	testBotUsername   = "testbot"
)

// fakeSDKGateway records EnsureChatSession / CreateMessage calls.
type fakeSDKGateway struct {
	mu           sync.Mutex
	chatSessions []sdk.EnsureChatSessionRequest
	messages     []sdk.CreateMessageRequest
}

func (g *fakeSDKGateway) EnsureChannel(_ context.Context, _ sdk.EnsureChannelRequest) (string, error) {
	return "channel-1", nil
}

func (g *fakeSDKGateway) EnsureChannelLinkSession(_ context.Context, req sdk.EnsureChannelLinkSessionRequest) (string, error) {
	return "link-" + req.AccountUUID, nil
}

func (g *fakeSDKGateway) EnsureChatSession(_ context.Context, req sdk.EnsureChatSessionRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.chatSessions = append(g.chatSessions, req)
	return "session-" + req.AccountUUID + "-" + req.ChatType + "-" + req.ChatID, nil
}

func (g *fakeSDKGateway) CreateMessage(_ context.Context, req sdk.CreateMessageRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.messages = append(g.messages, req)
	return "message-" + strconv.Itoa(len(g.messages)), nil
}

func (g *fakeSDKGateway) StreamSession(_ context.Context, _ sdk.StreamSessionRequest, _ func(sdk.StreamEvent) error) error {
	return nil
}

func (g *fakeSDKGateway) AgentParticipantID() string { return "agent:agent-1" }

func (g *fakeSDKGateway) BridgeParticipantID(platform string) string {
	return sdk.BridgeParticipantID(platform)
}

func (g *fakeSDKGateway) messageCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.messages)
}

// fakeSDKAccountStore is an in-memory AccountStore.
type fakeSDKAccountStore struct {
	mu     sync.Mutex
	states map[string]map[string]any
}

func newFakeSDKAccountStore() *fakeSDKAccountStore {
	return &fakeSDKAccountStore{states: make(map[string]map[string]any)}
}

func (s *fakeSDKAccountStore) SaveChannelAccountState(_ context.Context, accountUUID string, state map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make(map[string]any, len(state))
	for key, value := range state {
		copied[key] = value
	}
	s.states[accountUUID] = copied
	return nil
}

func (s *fakeSDKAccountStore) LoadChannelAccountState(_ context.Context, accountUUID string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[accountUUID]
	copied := make(map[string]any, len(state))
	for key, value := range state {
		copied[key] = value
	}
	return copied, nil
}

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// testJSONResponseObj builds a 200 response whose body is the JSON encoding of v.
func testJSONResponseObj(v any) (*http.Response, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(data))),
	}, nil
}

// httpClientReturning returns an *http.Client that answers every request with
// the JSON encoding of v.
func httpClientReturning(v any) *http.Client {
	return &http.Client{Transport: testRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return testJSONResponseObj(v)
	})}
}

// getMeOK returns the JSON that Telegram's /getMe returns for a valid token.
func getMeOK() map[string]any {
	return map[string]any{
		"ok": true,
		"result": map[string]any{
			"id":         testBotID,
			"is_bot":     true,
			"first_name": "Test Bot",
			"username":   testBotUsername,
		},
	}
}

// sdkAccount builds a ChannelAccount whose credential + state carry the bot
// identity produced by ValidateCredential.
func sdkAccount(uuid string) sdk.ChannelAccount {
	return sdk.ChannelAccount{
		UUID:     uuid,
		Platform: Platform,
		Credential: map[string]any{
			"account_id":     testBotIDStr,
			"bot_id":         testBotIDStr,
			"bot_token":      testBotToken,
			"webhook_secret": testWebhookSecret,
		},
		State: map[string]any{
			"bot_id":       testBotIDStr,
			"bot_username": testBotUsername,
		},
	}
}

func makeRuntime(gw sdk.Gateway, store sdk.AccountStore, accounts ...sdk.ChannelAccount) sdk.Runtime {
	rt := sdk.Runtime{
		WorkspaceUUID: "ws-1",
		Channel:       sdk.Channel{UUID: "channel-1", Platform: Platform},
		Gateway:       gw,
		AccountStore:  store,
	}
	if len(accounts) > 0 {
		rt.Account = accounts[0]
		rt.Accounts = accounts
	}
	return rt
}

// telegramUpdate builds a minimal Telegram Update JSON body.
type telegramUpdateBuilder struct {
	UpdateID  int64
	MessageID int64
	ChatID    int64
	ChatType  string // "private", "group", "supergroup"
	FromID    int64
	FromIsBot bool
	Text      string
}

func (b telegramUpdateBuilder) bytes() []byte {
	from := map[string]any{
		"id":         b.FromID,
		"is_bot":     b.FromIsBot,
		"first_name": "User",
	}
	chat := map[string]any{
		"id":   b.ChatID,
		"type": b.ChatType,
	}
	msg := map[string]any{
		"message_id": b.MessageID,
		"from":       from,
		"chat":       chat,
		"text":       b.Text,
	}
	update := map[string]any{
		"update_id": b.UpdateID,
		"message":   msg,
	}
	out, _ := json.Marshal(update)
	return out
}

// signedWebhookRequest builds an *http.Request with the correct
// X-Telegram-Bot-Api-Secret-Token header.
func signedWebhookRequest(secret string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	return req
}

// sendMessageOK returns the JSON that Telegram's /sendMessage returns on success.
func sendMessageOK(msgID int64) map[string]any {
	return map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": msgID,
		},
	}
}

// Silence unused-import warnings by ensuring fmt is referenced.
var _ = fmt.Sprintf
