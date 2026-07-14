package beaktelegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	platform "github.com/TrueWatch/beak-agent-channel-telegram/internal/telegram"
	"github.com/TrueWatch/beak-agent-channel-telegram/sdk"
	"github.com/TrueWatch/beak-agent-channel-telegram/state"
)

const (
	ID       = "beak-agent-telegram"
	Platform = "telegram"
)

var ErrCredentialLogin = errors.New("telegram connector uses credential login; create channel account from CredentialSchema")

type Connector struct{}

func NewConnector() sdk.Connector {
	return Connector{}
}

var _ sdk.Connector = Connector{}

// EventResult is returned by the inbound event handler.
type EventResult struct {
	Type        string              `json:"type"`
	Ignored     bool                `json:"ignored,omitempty"`
	Reason      string              `json:"reason,omitempty"`
	SessionUUID string              `json:"session_uuid,omitempty"`
	MessageUUID string              `json:"message_uuid,omitempty"`
	Inbound     *sdk.InboundMessage `json:"inbound,omitempty"`
}

func (Connector) Metadata() sdk.ConnectorMetadata {
	return sdk.ConnectorMetadata{
		ID:          ID,
		Platform:    Platform,
		Label:       "Telegram",
		Description: "Connect Telegram bot accounts to Beak Channel Gateway",
		Capabilities: sdk.Capabilities{
			LoginModes:     []string{sdk.LoginModeCredential},
			Text:           true,
			Media:          false,
			GroupChat:      true,
			DirectChat:     true,
			Stream:         false,
			Webhook:        true,
			BlockStreaming: false,
		},
	}
}

func (Connector) CredentialSchema(context.Context) sdk.CredentialSchema {
	return sdk.CredentialSchema{
		Type:       "object",
		LoginModes: []string{sdk.LoginModeCredential},
		Properties: map[string]sdk.CredentialField{
			"bot_token": {
				Type:        "string",
				Title:       "Bot Token",
				Description: "Bot token issued by BotFather when the bot was created (format: 123456:ABCdef...)",
				Secret:      true,
			},
			"webhook_secret": {
				Type:        "string",
				Title:       "Webhook Secret Token",
				Description: "Optional secret token sent in X-Telegram-Bot-Api-Secret-Token header for inbound webhook verification (1-256 characters, A-Z a-z 0-9 _ -)",
				Secret:      false,
			},
		},
		Required: []string{
			"bot_token",
		},
		AdditionalProperties: false,
	}
}

func (Connector) ValidateCredential(ctx context.Context, req sdk.CredentialValidationRequest) (*sdk.CredentialValidationResult, error) {
	credential := cloneMap(req.Credential)
	stateMap := cloneMap(req.State)

	client := platform.NewClient("", credentialStrings(credential))
	client.HTTPClient = req.Runtime.HTTPClient

	info, err := client.Validate(ctx)
	if err != nil {
		return &sdk.CredentialValidationResult{
			Valid:       false,
			AccountKey:  firstString(credential["account_id"], credential["bot_id"]),
			DisplayName: firstString(credential["display_name"], credential["account_id"]),
			Credential:  credential,
			State:       stateMap,
			Metadata:    map[string]any{"platform": Platform},
			Error:       err.Error(),
		}, nil
	}

	// Persist bot identity to state so inbound ownership / self-echo checks
	// have it without re-calling getMe.
	credential["account_id"] = info.AccountID
	credential["bot_id"] = info.BotID
	stateMap["bot_id"] = info.BotID
	if strings.TrimSpace(info.BotUserID) != "" {
		stateMap["bot_username"] = info.BotUserID
	}
	// Standardized nested identity (in addition to the flat keys above, which
	// self-echo detection still reads) so generic conformance/host tooling can
	// find the bot's identity at a single well-known path.
	if id := firstString(info.BotUserID, info.BotID, info.AccountID); id != "" {
		stateMap["bot_identity"] = map[string]any{"id": id}
	}
	return &sdk.CredentialValidationResult{
		Valid:       true,
		AccountKey:  info.AccountID,
		DisplayName: firstString(info.DisplayName, info.BotName, info.AccountID),
		Credential:  credential,
		State:       stateMap,
		Metadata: map[string]any{
			"platform": Platform,
			"bot_id":   info.BotID,
		},
	}, nil
}

func (Connector) StartLogin(context.Context, sdk.LoginStartRequest) (*sdk.LoginChallenge, error) {
	return nil, ErrCredentialLogin
}

func (Connector) PollLogin(context.Context, sdk.LoginPollRequest) (*sdk.LoginStatus, error) {
	return nil, ErrCredentialLogin
}

func (c Connector) Start(ctx context.Context, runtime sdk.Runtime) error {
	if runtime.Gateway == nil {
		return fmt.Errorf("%s connector requires sdk.Runtime.Gateway", Platform)
	}
	if _, err := runtime.Gateway.EnsureChannel(ctx, sdk.EnsureChannelRequest{
		WorkspaceUUID: runtime.WorkspaceUUID,
		Platform:      Platform,
		Name:          "Telegram",
		Config:        map[string]any{"bridge": ID},
	}); err != nil {
		return err
	}

	store := newConnectorStateStore(runtime.AccountStore)
	for _, account := range runtimeAccountCandidates(runtime) {
		store.seed(account)
		accountUUID := accountKey(account)
		if accountUUID == "" {
			return fmt.Errorf("%s account_uuid or account_id is required", Platform)
		}
		sessionUUID, err := runtime.Gateway.EnsureChannelLinkSession(ctx, sdk.EnsureChannelLinkSessionRequest{
			WorkspaceUUID:       runtime.WorkspaceUUID,
			Platform:            Platform,
			AccountUUID:         accountUUID,
			AgentParticipantID:  runtime.Gateway.AgentParticipantID(),
			BridgeParticipantID: runtime.Gateway.BridgeParticipantID(Platform),
		})
		if err != nil {
			return err
		}
		st, err := store.LoadAccount(ctx, accountUUID)
		if err != nil {
			return err
		}
		if st.ChannelLinkSession != sessionUUID {
			st.ChannelLinkSession = sessionUUID
			if err := store.SaveAccount(ctx, st); err != nil {
				return err
			}
		}
	}
	return nil
}

func (Connector) Send(ctx context.Context, runtime sdk.Runtime, req sdk.OutboundMessage) (*sdk.SendResult, error) {
	account, err := selectRuntimeAccount(runtime, req.AccountUUID)
	if err != nil {
		return nil, err
	}
	accountUUID := accountKey(account)
	if accountUUID == "" {
		return nil, fmt.Errorf("%s outbound account is required", Platform)
	}
	if strings.TrimSpace(req.ChatID) == "" {
		return nil, fmt.Errorf("%s outbound chat_id is required", Platform)
	}
	if strings.TrimSpace(req.Text) == "" {
		return nil, fmt.Errorf("%s outbound text is required", Platform)
	}

	store := newConnectorStateStore(runtime.AccountStore)
	store.seed(account)
	st, err := store.LoadAccount(ctx, accountUUID)
	if err != nil {
		return nil, err
	}

	client := platform.NewClient("", credentialStrings(account.Credential))
	client.HTTPClient = runtime.HTTPClient
	messageID, err := client.SendText(ctx, req.ChatID, req.Text, req.Format, req.Mentions, req.MentionAll)
	if err != nil {
		return nil, err
	}
	if err := store.SaveAccount(ctx, st); err != nil {
		return nil, err
	}
	return &sdk.SendResult{
		Platform:    Platform,
		AccountUUID: accountUUID,
		MessageID:   messageID,
	}, nil
}

func (Connector) Stop(ctx context.Context, account sdk.ChannelAccount) error {
	_ = ctx
	_ = account
	return nil
}

// HandleWebhookRequest implements sdk WebhookRequestConnector. It reads the raw
// body, verifies the X-Telegram-Bot-Api-Secret-Token header, then processes the
// inbound Telegram Update.
func (c Connector) HandleWebhookRequest(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, req *http.Request) (*sdk.WebhookResponse, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("%s read webhook body: %w", Platform, err)
	}

	// Verify webhook secret (constant-time; no-op when secret not configured).
	webhookSecret := stringValue(account.Credential["webhook_secret"])
	headerToken := req.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if err := platform.VerifyWebhookSignature(webhookSecret, headerToken); err != nil {
		return nil, err
	}

	if _, err := c.HandleWebhook(ctx, runtime, account, body); err != nil {
		return nil, err
	}
	// Telegram requires a 2xx; the body is ignored for webhook deliveries.
	return &sdk.WebhookResponse{StatusCode: http.StatusOK}, nil
}

// HandleWebhook parses an already-verified Telegram Update body and runs the
// inbound flow. Separated from HandleWebhookRequest so tests can drive the
// inbound logic directly.
func (c Connector) HandleWebhook(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, body []byte) (*EventResult, error) {
	var update platform.Update
	if err := json.Unmarshal(body, &update); err != nil {
		return nil, fmt.Errorf("%s decode update: %w", Platform, err)
	}
	return c.processUpdate(ctx, runtime, account, &update)
}

// processUpdate implements the inbound standard flow: ownership / self-echo
// filter → text-only filter → chat identity → dedupe → EnsureChatSession +
// CreateMessage + state persistence.
func (c Connector) processUpdate(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, update *platform.Update) (*EventResult, error) {
	accountUUID := accountKey(account)
	if accountUUID == "" {
		return nil, fmt.Errorf("%s account_uuid or account_id is required", Platform)
	}

	// Only message updates are handled; other update types are ignored.
	if update.Message == nil {
		return &EventResult{Type: "update", Ignored: true, Reason: "unsupported_update_type"}, nil
	}
	msg := update.Message

	// Resolve bot ID from credential / state for self-echo detection.
	botIDStr := firstString(account.Credential["bot_id"], account.Credential["account_id"], account.State["bot_id"])
	var botID int64
	if botIDStr != "" {
		if parsed, err := strconv.ParseInt(botIDStr, 10, 64); err == nil {
			botID = parsed
		}
	}

	// 1. Self-echo: drop messages sent by the bot itself, or any bot.
	if msg.From != nil && (msg.From.IsBot || (botID != 0 && msg.From.ID == botID)) {
		return &EventResult{Type: "message", Ignored: true, Reason: "self_echo"}, nil
	}

	// 2. Non-text: ignore messages without text.
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return &EventResult{Type: "message", Ignored: true, Reason: "unsupported_message_type"}, nil
	}

	// 3. Chat identity normalization.
	chatType := sdk.ChatTypeGroup
	if msg.Chat.Type == "private" {
		chatType = sdk.ChatTypeDirect
	}
	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	var senderID string
	if msg.From != nil {
		senderID = strconv.FormatInt(msg.From.ID, 10)
	}
	if senderID == "" {
		return &EventResult{Type: "message", Ignored: true, Reason: "unsupported_message_type"}, nil
	}

	// 4. Dedupe using message_id as primary key.
	messageIDStr := strconv.FormatInt(msg.MessageID, 10)
	dedupeKey := accountUUID + ":message:" + messageIDStr
	stateKey := Platform + ":" + chatType + ":" + chatID

	store := newConnectorStateStore(runtime.AccountStore)
	store.seed(account)
	st, err := store.LoadAccount(ctx, accountUUID)
	if err != nil {
		return nil, err
	}

	if _, ok := st.InboundSeen[dedupeKey]; ok {
		return &EventResult{Type: "message", Ignored: true, Reason: "duplicate", SessionUUID: st.PeerSessions[stateKey]}, nil
	}

	// MentionedMe: scan Telegram message entities. A "mention" entity is an
	// @username substring (compare its slice of the text against the bot's
	// @username from state); a "text_mention" entity carries an explicit User
	// whose ID we match against the bot id.
	botUsername := strings.TrimPrefix(firstString(account.State["bot_username"], account.Credential["bot_username"]), "@")
	mentionedMe := false
	for _, ent := range msg.Entities {
		switch ent.Type {
		case "mention":
			if botUsername != "" && ent.Offset >= 0 && ent.Offset+ent.Length <= len(msg.Text) {
				if strings.EqualFold(strings.TrimPrefix(msg.Text[ent.Offset:ent.Offset+ent.Length], "@"), botUsername) {
					mentionedMe = true
				}
			}
		case "text_mention":
			if ent.User != nil && botID != 0 && ent.User.ID == botID {
				mentionedMe = true
			}
		}
	}

	inbound := sdk.InboundMessage{
		WorkspaceUUID: runtime.WorkspaceUUID,
		Platform:      Platform,
		AccountUUID:   accountUUID,
		ChannelUUID:   runtime.Channel.UUID,
		ChatType:      chatType,
		ChatID:        chatID,
		SenderID:      senderID,
		MessageID:     messageIDStr,
		Text:          text,
		MentionedMe:   mentionedMe,
		DedupeKey:     dedupeKey,
		Raw: map[string]any{
			"update_id":  update.UpdateID,
			"message_id": msg.MessageID,
			"chat_id":    msg.Chat.ID,
			"from_id":    senderID,
		},
	}

	// 5a. Ensure the chat session (identity includes account uuid → two bots
	// in the same group never share a session).
	sessionUUID, err := runtime.Gateway.EnsureChatSession(ctx, sdk.EnsureChatSessionRequest{
		WorkspaceUUID:       runtime.WorkspaceUUID,
		Platform:            Platform,
		AccountUUID:         accountUUID,
		ChatType:            chatType,
		ChatID:              chatID,
		SenderID:            senderID,
		AgentParticipantID:  runtime.Gateway.AgentParticipantID(),
		BridgeParticipantID: runtime.Gateway.BridgeParticipantID(Platform),
	})
	if err != nil {
		return nil, err
	}

	// 5b. Create the Beak message.
	messageUUID, err := runtime.Gateway.CreateMessage(ctx, sdk.CreateMessageRequest{
		WorkspaceUUID: runtime.WorkspaceUUID,
		SessionUUID:   sessionUUID,
		SenderID:      sdk.IMPersonParticipantID(Platform, chatType, chatID, senderID),
		Content:       text,
		DedupeKey:     dedupeKey,
		Metadata: map[string]any{
			"source":             Platform,
			"telegram_chat_type": chatType,
			"telegram_chat_id":   chatID,
			"inbound_message":    inbound,
		},
	})
	if err != nil {
		return nil, err
	}

	// 5c. Persist session mapping and dedupe marker.
	st.PeerSessions[stateKey] = sessionUUID
	st.InboundSeen[dedupeKey] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.SaveAccount(ctx, st); err != nil {
		return nil, err
	}

	return &EventResult{
		Type:        "message",
		SessionUUID: sessionUUID,
		MessageUUID: messageUUID,
		Inbound:     &inbound,
	}, nil
}

// jsonWebhookResponse builds a 200 JSON webhook response.
func jsonWebhookResponse(value any) (*sdk.WebhookResponse, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &sdk.WebhookResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"content-type": "application/json; charset=utf-8"},
		Body:       data,
	}, nil
}

// connectorStateStore adapts sdk.AccountStore to typed state.AccountState.
type connectorStateStore struct {
	mu           sync.Mutex
	accounts     map[string]*state.AccountState
	accountStore sdk.AccountStore
}

func newConnectorStateStore(accountStore sdk.AccountStore) *connectorStateStore {
	return &connectorStateStore{
		accounts:     make(map[string]*state.AccountState),
		accountStore: accountStore,
	}
}

func (s *connectorStateStore) seed(account sdk.ChannelAccount) {
	accountID := accountKey(account)
	if accountID == "" {
		return
	}
	// Registration only; LoadAccount is the single point that populates the
	// cache (rehydrating from the AccountStore when present), so seed must not
	// pre-insert an empty state that would shadow persisted state.
	_ = accountID
}

func (s *connectorStateStore) LoadAccount(ctx context.Context, accountID string) (*state.AccountState, error) {
	s.mu.Lock()
	if st, ok := s.accounts[accountID]; ok {
		s.mu.Unlock()
		return st, nil
	}
	accountStore := s.accountStore
	s.mu.Unlock()

	st := &state.AccountState{AccountID: accountID}
	if accountStore != nil {
		raw, err := accountStore.LoadChannelAccountState(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if data, err := json.Marshal(raw); err == nil {
				_ = json.Unmarshal(data, st)
			}
			st.AccountID = accountID
		}
	}
	st.EnsureMaps()

	s.mu.Lock()
	s.accounts[accountID] = st
	s.mu.Unlock()
	return st, nil
}

func (s *connectorStateStore) SaveAccount(ctx context.Context, account *state.AccountState) error {
	if err := state.TouchAccount(account); err != nil {
		return err
	}
	s.mu.Lock()
	s.accounts[account.AccountID] = account
	accountStore := s.accountStore
	s.mu.Unlock()
	if accountStore != nil {
		data, err := json.Marshal(account)
		if err != nil {
			return err
		}
		var persisted map[string]any
		if err := json.Unmarshal(data, &persisted); err != nil {
			return err
		}
		return accountStore.SaveChannelAccountState(ctx, account.AccountID, persisted)
	}
	return nil
}

func runtimeAccountCandidates(runtime sdk.Runtime) []sdk.ChannelAccount {
	seen := make(map[string]bool)
	var out []sdk.ChannelAccount
	add := func(account sdk.ChannelAccount) {
		key := accountKey(account)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, account)
	}
	add(runtime.Account)
	for _, account := range runtime.Accounts {
		add(account)
	}
	return out
}

func selectRuntimeAccount(runtime sdk.Runtime, accountUUID string) (sdk.ChannelAccount, error) {
	accountUUID = strings.TrimSpace(accountUUID)
	candidates := runtimeAccountCandidates(runtime)
	if accountUUID != "" {
		for _, account := range candidates {
			if accountMatches(account, accountUUID) {
				return account, nil
			}
		}
		return sdk.ChannelAccount{}, fmt.Errorf("%s account %s not found in runtime", Platform, accountUUID)
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) == 0 {
		return sdk.ChannelAccount{}, fmt.Errorf("%s outbound account is required", Platform)
	}
	return sdk.ChannelAccount{}, fmt.Errorf("%s outbound account is ambiguous; account_uuid is required", Platform)
}

func accountMatches(account sdk.ChannelAccount, accountID string) bool {
	return strings.TrimSpace(account.UUID) == accountID ||
		strings.TrimSpace(stringValue(account.Credential["account_id"])) == accountID ||
		strings.TrimSpace(stringValue(account.Credential["bot_id"])) == accountID
}

func accountKey(account sdk.ChannelAccount) string {
	return firstString(account.UUID, account.Credential["account_id"], account.Credential["bot_id"])
}

func credentialStrings(credential map[string]any) map[string]string {
	out := make(map[string]string, len(credential))
	for key, value := range credential {
		out[key] = stringValue(value)
	}
	return out
}

func cloneMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		if s := strings.TrimSpace(stringValue(value)); s != "" {
			return s
		}
	}
	return ""
}
