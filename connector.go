package beaktelegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	platform "github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/internal/telegram"
	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/state"
)

const (
	ID                     = "beak-agent-telegram"
	Platform               = "telegram"
	maxWebhookRequestBytes = 4 << 20
	businessChatIDPrefix   = "business:"
	directChatIDPrefix     = "direct_messages:"
	directThreadIDPrefix   = "direct_messages:"
	credentialAttempts     = 2
	credentialRetryDelay   = 100 * time.Millisecond
	webhookSecretBytes     = 32
)

var ErrCredentialLogin = errors.New("telegram connector uses credential login; create channel account from CredentialSchema")

var webhookSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

var inboundAccountLocks = struct {
	sync.Mutex
	items map[string]*inboundAccountLock
}{items: make(map[string]*inboundAccountLock)}

type inboundAccountLock struct {
	mu   sync.Mutex
	refs int
}

var managedAccountStateKeys = []string{
	"account_id",
	"channel_link_session",
	"peer_sessions",
	"inbound_seen",
	"sent_beak_messages",
	"outbound_deliveries",
	"stream_cursors",
	"bot_id",
	"bot_username",
	"offset",
	sdk.RuntimeHealthKeyStreamConnectionState,
	sdk.RuntimeHealthKeyStreamConnectedAt,
	sdk.RuntimeHealthKeyStreamDisconnectedAt,
	sdk.RuntimeHealthKeyStreamLastActivityAt,
	sdk.RuntimeHealthKeyStreamLastPingAt,
	sdk.RuntimeHealthKeyStreamLastPongAt,
	sdk.RuntimeHealthKeyStreamLastEventAt,
	sdk.RuntimeHealthKeyStreamLastError,
	sdk.RuntimeHealthKeyStreamLastErrorAt,
	sdk.RuntimeHealthKeyStreamReconnectRequestedAt,
	sdk.RuntimeHealthKeyStreamReconnectError,
	sdk.RuntimeHealthKeyStreamReconnectErrorAt,
	sdk.RuntimeHealthKeyStreamSessionExpired,
	"updated_at",
}

type Connector struct{}

// WebhookConnector is the optional raw-body ingress extension implemented by
// Telegram connectors. It mirrors the extension exposed by the Lark and
// DingTalk SDKs while keeping the base sdk.Connector platform-neutral.
type WebhookConnector interface {
	sdk.Connector
	HandleWebhook(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, body []byte) (*EventResult, error)
}

// WebhookRequestConnector lets a Beak HTTP adapter delegate request size
// limits, secret-token verification, decoding, and structured HTTP errors to
// the SDK.
type WebhookRequestConnector interface {
	sdk.Connector
	HandleWebhookRequest(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, req *http.Request) (*sdk.WebhookResponse, error)
}

func NewConnector() sdk.Connector {
	return Connector{}
}

var _ sdk.Connector = Connector{}
var _ WebhookConnector = Connector{}
var _ WebhookRequestConnector = Connector{}

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
			LoginModes:          []string{sdk.LoginModeCredential},
			Text:                true,
			Media:               true,
			MediaKinds:          []string{sdk.MediaKindImage, sdk.MediaKindFile, sdk.MediaKindAudio, sdk.MediaKindVideo, sdk.MediaKindSticker},
			GroupChat:           true,
			DirectChat:          true,
			Stream:              false,
			Webhook:             true,
			WebhookRegistration: sdk.WebhookRegistrationAutomatic,
			BlockStreaming:      false,
			AckModes:            []string{"reaction"},
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
	secret := strings.TrimSpace(stringValue(credential["webhook_secret"]))
	if secret != "" && !webhookSecretPattern.MatchString(secret) {
		return &sdk.CredentialValidationResult{
			Valid:       false,
			AccountKey:  firstString(credential["account_id"], credential["bot_id"]),
			DisplayName: firstString(credential["display_name"], credential["account_id"]),
			Credential:  credential,
			State:       stateMap,
			Metadata:    map[string]any{"platform": Platform},
			Error:       "telegram webhook_secret must be 1-256 characters using only A-Z, a-z, 0-9, _ or -",
		}, nil
	}
	if secret != "" {
		credential["webhook_secret"] = secret
	}
	if botToken := strings.TrimSpace(stringValue(credential["bot_token"])); botToken != "" {
		credential["bot_token"] = botToken
	}

	client := platform.NewClient("", credentialStrings(credential))
	client.HTTPClient = req.Runtime.HTTPClient

	info, err := validateTelegramCredential(ctx, client)
	if err != nil {
		if platform.IsCredentialRejected(err) {
			return credentialValidationFailure(credential, stateMap, err), nil
		}
		return nil, fmt.Errorf("telegram credential validation failed: %w", err)
	}
	if secret == "" {
		secret, err = generateWebhookSecret()
		if err != nil {
			return nil, fmt.Errorf("generate telegram webhook secret: %w", err)
		}
		credential["webhook_secret"] = secret
	}

	// Persist bot identity to state so inbound ownership / self-echo checks
	// have it without re-calling getMe.
	credential["account_id"] = info.AccountID
	credential["bot_id"] = info.BotID
	stateMap["bot_id"] = info.BotID
	if strings.TrimSpace(info.Username) != "" {
		stateMap["bot_username"] = info.Username
	}
	// Standardized nested identity (in addition to the flat keys above, which
	// self-echo detection still reads) so generic conformance/host tooling can
	// find the bot's identity at a single well-known path.
	if id := firstString(info.BotID, info.BotUserID, info.AccountID); id != "" {
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

func generateWebhookSecret() (string, error) {
	raw := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validateTelegramCredential(ctx context.Context, client *platform.Client) (*platform.BotInfo, error) {
	var lastErr error
	for attempt := 0; attempt < credentialAttempts; attempt++ {
		info, err := client.Validate(ctx)
		if err == nil {
			return info, nil
		}
		lastErr = err
		if platform.IsCredentialRejected(err) || !platform.IsRetryableError(err) || attempt+1 == credentialAttempts {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(credentialRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func credentialValidationFailure(credential, stateMap map[string]any, err error) *sdk.CredentialValidationResult {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return &sdk.CredentialValidationResult{
		Valid:       false,
		AccountKey:  firstString(credential["account_id"], credential["bot_id"]),
		DisplayName: firstString(credential["display_name"], credential["account_id"]),
		Credential:  credential,
		State:       stateMap,
		Metadata:    map[string]any{"platform": Platform},
		Error:       message,
	}
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
	accounts := runtimeAccountCandidates(runtime)
	if len(accounts) == 0 {
		return fmt.Errorf("%s runtime account is required", Platform)
	}
	if len(accounts) != 1 {
		return fmt.Errorf("%s webhook runtime requires exactly one account", Platform)
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
	for _, account := range accounts {
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
	account := accounts[0]
	client := platform.NewClient("", credentialStrings(account.Credential))
	client.HTTPClient = runtime.HTTPClient
	webhookURL := ""
	if runtime.Webhook != nil {
		webhookURL = runtime.Webhook.URL
	}
	if err := client.SetWebhook(ctx, webhookURL, strings.TrimSpace(stringValue(account.Credential["webhook_secret"]))); err != nil {
		return fmt.Errorf("register telegram webhook: %w", err)
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
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return nil, fmt.Errorf("%s outbound text or attachments are required", Platform)
	}

	client := platform.NewClient("", credentialStrings(account.Credential))
	client.HTTPClient = runtime.HTTPClient
	chatID, businessConnectionID, directTopicID, err := telegramOutboundChatTarget(req.ChatID, req.Raw)
	if err != nil {
		return nil, err
	}
	messageThreadID, threadDirectTopicID, err := telegramOutboundThreadTarget(req.ThreadID, req.Raw)
	if err != nil {
		return nil, err
	}
	if directTopicID != "" && threadDirectTopicID != "" && directTopicID != threadDirectTopicID {
		return nil, fmt.Errorf("telegram direct messages topic mismatch between chat_id and thread_id")
	}
	if directTopicID == "" {
		directTopicID = threadDirectTopicID
	}
	var parts []string
	if strings.TrimSpace(req.Text) != "" {
		parts, err = platform.PrepareTextParts(req.Text, req.Format, req.Mentions, req.MentionAll)
		if err != nil {
			return nil, err
		}
	}
	route := platform.TextRoute{
		ChatID:                chatID,
		MessageThreadID:       messageThreadID,
		DirectMessagesTopicID: directTopicID,
		BusinessConnectionID:  businessConnectionID,
	}
	if len(parts) > 1 && strings.TrimSpace(req.MessageUUID) == "" {
		return nil, fmt.Errorf("telegram multipart outbound requires message_uuid for retry-safe delivery")
	}
	if len(parts) > 1 && runtime.AccountStore == nil {
		return nil, fmt.Errorf("telegram multipart outbound requires sdk.Runtime.AccountStore for retry-safe delivery")
	}

	messageIDs := make([]string, 0, len(parts)+len(req.Attachments))
	resumed := false
	if len(parts) > 0 {
		textIDs, textResumed, sendErr := sendTelegramParts(ctx, runtime, account, req.MessageUUID, route, parts, client)
		if sendErr != nil {
			return nil, sendErr
		}
		messageIDs = append(messageIDs, textIDs...)
		resumed = textResumed
	}
	attachmentResults := make([]sdk.AttachmentSendResult, 0, len(req.Attachments))
	var attachmentErrors []string
	for _, attachment := range req.Attachments {
		result := sdk.AttachmentSendResult{AttachmentID: attachment.ID, Kind: attachment.Kind}
		if err := sdk.ValidateMediaAttachment(attachment); err != nil {
			result.Status = sdk.AttachmentStatusFailed
			result.Error = err.Error()
			attachmentResults = append(attachmentResults, result)
			attachmentErrors = append(attachmentErrors, err.Error())
			continue
		}
		messageID, sendErr := client.SendAttachment(ctx, route, attachment)
		if sendErr != nil {
			result.Status = sdk.AttachmentStatusFailed
			result.Error = sendErr.Error()
			attachmentResults = append(attachmentResults, result)
			attachmentErrors = append(attachmentErrors, sendErr.Error())
			continue
		}
		result.Status = sdk.AttachmentStatusSent
		result.MessageID = messageID
		attachmentResults = append(attachmentResults, result)
		messageIDs = append(messageIDs, messageID)
	}
	if len(messageIDs) == 0 {
		if len(attachmentErrors) > 0 {
			return nil, fmt.Errorf("%s outbound attachments failed: %s", Platform, strings.Join(attachmentErrors, "; "))
		}
		return nil, fmt.Errorf("%s outbound produced no messages", Platform)
	}
	result := &sdk.SendResult{
		Platform:          Platform,
		AccountUUID:       accountUUID,
		MessageID:         messageIDs[len(messageIDs)-1],
		AttachmentResults: attachmentResults,
		Raw:               map[string]any{"message_ids": messageIDs},
	}
	if resumed {
		result.Raw["resumed"] = true
	}
	if len(attachmentErrors) > 0 {
		result.Raw["attachment_errors"] = attachmentErrors
	}
	return result, nil
}

func sendTelegramParts(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, messageUUID string, route platform.TextRoute, parts []string, client *platform.Client) ([]string, bool, error) {
	messageUUID = strings.TrimSpace(messageUUID)
	if messageUUID == "" || runtime.AccountStore == nil {
		messageIDs, err := client.SendPreparedTextParts(ctx, route, parts, 0, nil)
		return messageIDs, false, err
	}

	accountUUID := accountKey(account)
	unlock := lockInboundAccount(accountUUID)
	defer unlock()

	store := newConnectorStateStore(runtime.AccountStore)
	store.seed(account)
	st, err := store.LoadAccount(ctx, accountUUID)
	if err != nil {
		return nil, false, err
	}
	fingerprint, err := telegramOutboundFingerprint(route, parts)
	if err != nil {
		return nil, false, err
	}
	delivery := st.OutboundDeliveries[messageUUID]
	if delivery.Fingerprint != "" && delivery.Fingerprint != fingerprint {
		return nil, false, fmt.Errorf("telegram message_uuid %s was already used for a different outbound payload", messageUUID)
	}
	if delivery.Fingerprint == "" {
		delivery.Fingerprint = fingerprint
	}
	if delivery.Completed {
		if len(delivery.MessageIDs) == 0 {
			return nil, false, fmt.Errorf("telegram completed outbound %s has no message ids", messageUUID)
		}
		return append([]string(nil), delivery.MessageIDs...), true, nil
	}
	if len(delivery.MessageIDs) > len(parts) {
		return nil, false, fmt.Errorf("telegram outbound %s persisted %d parts for a %d-part payload", messageUUID, len(delivery.MessageIDs), len(parts))
	}
	startPart := len(delivery.MessageIDs)
	delivery.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	st.OutboundDeliveries[messageUUID] = delivery
	if err := store.SaveAccount(ctx, st); err != nil {
		return nil, startPart > 0, err
	}

	_, err = client.SendPreparedTextParts(ctx, route, parts, startPart, func(index int, messageID string) error {
		if index != len(delivery.MessageIDs) {
			return fmt.Errorf("unexpected part index %d after %d persisted parts", index, len(delivery.MessageIDs))
		}
		delivery.MessageIDs = append(delivery.MessageIDs, messageID)
		delivery.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		st.OutboundDeliveries[messageUUID] = delivery
		return store.SaveAccount(ctx, st)
	})
	if err != nil {
		return nil, startPart > 0, err
	}
	delivery.Completed = true
	delivery.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	st.OutboundDeliveries[messageUUID] = delivery
	st.SentBeakMessages[messageUUID] = delivery.UpdatedAt
	if err := store.SaveAccount(ctx, st); err != nil {
		return nil, startPart > 0, err
	}
	return append([]string(nil), delivery.MessageIDs...), startPart > 0, nil
}

func telegramOutboundFingerprint(route platform.TextRoute, parts []string) (string, error) {
	payload := struct {
		Route platform.TextRoute `json:"route"`
		Parts []string           `json:"parts"`
	}{Route: route, Parts: parts}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode telegram outbound fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:]), nil
}

func (Connector) Acknowledge(ctx context.Context, runtime sdk.Runtime, req sdk.OutboundAck) (*sdk.AckResult, error) {
	account, err := selectRuntimeAccount(runtime, req.AccountUUID)
	if err != nil {
		return nil, err
	}
	accountUUID := accountKey(account)
	mode := strings.ToLower(firstString(req.Mode, "reaction"))
	result := &sdk.AckResult{Platform: Platform, AccountUUID: accountUUID, Mode: "reaction"}
	if mode != "reaction" && mode != "auto" {
		result.Mode = mode
		result.Status = "unsupported"
		result.Raw = map[string]any{"reason": "unsupported_ack_mode"}
		return result, nil
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action != "" && action != "start" && action != "processing" {
		result.Status = "skipped"
		result.Raw = map[string]any{"reason": "reaction_action_not_applicable"}
		return result, nil
	}
	if strings.TrimSpace(req.ChatID) == "" || strings.TrimSpace(req.TargetMessageID) == "" {
		result.Status = "skipped"
		result.Raw = map[string]any{"reason": "missing_reaction_target"}
		return result, nil
	}
	emoji := telegramAckEmoji(req)
	chatID, _, _, err := telegramOutboundChatTarget(req.ChatID, req.Raw)
	if err != nil {
		return nil, err
	}
	client := platform.NewClient("", credentialStrings(account.Credential))
	client.HTTPClient = runtime.HTTPClient
	if err := client.SetMessageReaction(ctx, chatID, req.TargetMessageID, emoji); err != nil {
		return nil, err
	}
	result.Status = "sent"
	result.ReactionID = emoji
	result.Raw = map[string]any{"emoji": emoji, "message_id": req.TargetMessageID}
	return result, nil
}

func (Connector) Stop(ctx context.Context, account sdk.ChannelAccount) error {
	_ = ctx
	_ = account
	return nil
}

// HandleWebhookRequest implements WebhookRequestConnector. It reads the raw
// body, verifies the X-Telegram-Bot-Api-Secret-Token header, then processes the
// inbound Telegram Update.
func (c Connector) HandleWebhookRequest(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, req *http.Request) (*sdk.WebhookResponse, error) {
	if req == nil || req.Body == nil {
		return nil, webhookError(http.StatusBadRequest, "invalid_request_body", errors.New("telegram webhook request body is required"))
	}
	// Reject unauthenticated requests before reading attacker-controlled bodies.
	webhookSecret := stringValue(account.Credential["webhook_secret"])
	headerToken := req.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if err := platform.VerifyWebhookSignature(webhookSecret, headerToken); err != nil {
		return nil, webhookError(http.StatusForbidden, "channel_webhook_auth_failed", err)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxWebhookRequestBytes+1))
	if err != nil {
		return nil, webhookError(http.StatusBadRequest, "invalid_request_body", fmt.Errorf("%s read webhook body: %w", Platform, err))
	}
	if len(body) > maxWebhookRequestBytes {
		return nil, webhookError(http.StatusRequestEntityTooLarge, "request_body_too_large", fmt.Errorf("%s webhook body exceeds %d bytes", Platform, maxWebhookRequestBytes))
	}

	if _, err := c.HandleWebhook(ctx, runtime, account, body); err != nil {
		var statusErr interface{ HTTPStatusCode() int }
		if errors.As(err, &statusErr) {
			return nil, err
		}
		return nil, webhookError(http.StatusInternalServerError, "channel_webhook_failed", err)
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
		return nil, webhookError(http.StatusBadRequest, "invalid_request_body", fmt.Errorf("%s decode update: %w", Platform, err))
	}
	return c.processUpdate(ctx, runtime, account, &update)
}

// processUpdate implements the inbound standard flow: ownership / self-echo
// filter -> message normalization -> chat identity -> dedupe ->
// EnsureChatSession + CreateMessage + state persistence.
func (c Connector) processUpdate(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, update *platform.Update) (*EventResult, error) {
	if runtime.Gateway == nil {
		return nil, fmt.Errorf("%s event handling requires sdk.Runtime.Gateway", Platform)
	}
	accountUUID := accountKey(account)
	if accountUUID == "" {
		return nil, fmt.Errorf("%s account_uuid or account_id is required", Platform)
	}
	unlock := lockInboundAccount(accountUUID)
	defer unlock()

	store := newConnectorStateStore(runtime.AccountStore)
	store.seed(account)
	st, err := store.LoadAccount(ctx, accountUUID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if st.StreamConnectionState != sdk.RuntimeHealthStateConnected || st.StreamConnectedAt.IsZero() {
		st.StreamConnectedAt = now
	}
	st.StreamConnectionState = sdk.RuntimeHealthStateConnected
	st.StreamLastActivityAt = now
	st.StreamSessionExpired = false
	st.StreamLastError = ""
	st.StreamLastErrorAt = time.Time{}
	mediaClient := platform.NewClient("", credentialStrings(account.Credential))
	mediaClient.HTTPClient = runtime.HTTPClient

	ignored := func(eventType, reason, sessionUUID string) (*EventResult, error) {
		if err := store.SaveAccount(ctx, st); err != nil {
			return nil, err
		}
		return &EventResult{Type: eventType, Ignored: true, Reason: reason, SessionUUID: sessionUUID}, nil
	}
	failed := func(err error) (*EventResult, error) {
		st.StreamLastError = err.Error()
		st.StreamLastErrorAt = time.Now().UTC()
		if saveErr := store.SaveAccount(ctx, st); saveErr != nil {
			return nil, errors.Join(err, saveErr)
		}
		return nil, err
	}

	eventType, msg := telegramUpdateMessage(update)
	if msg == nil {
		return ignored("update", "unsupported_update_type", "")
	}
	if telegramEditedEvent(eventType) {
		// Beak does not yet expose a common message-update operation. Creating a
		// second message for an edit would incorrectly trigger another agent turn.
		return ignored(eventType, "edited_message_not_supported", "")
	}

	// Resolve bot ID from credential / state for self-echo detection.
	botIDStr := firstString(account.Credential["bot_id"], account.Credential["account_id"], st.BotID, account.State["bot_id"])
	var botID int64
	if botIDStr != "" {
		if parsed, err := strconv.ParseInt(botIDStr, 10, 64); err == nil {
			botID = parsed
		}
	}

	// Ignore only this connector's bot. Messages from other bots remain valid
	// Telegram events and must not be mistaken for self echoes.
	if botID != 0 && ((msg.From != nil && msg.From.ID == botID) || (msg.SenderBusinessBot != nil && msg.SenderBusinessBot.ID == botID)) {
		return ignored(eventType, "self_echo", "")
	}

	rawText, entities, messageType := telegramMessageContent(msg)
	if strings.TrimSpace(rawText) == "" && len(entities) == 0 && !telegramMessageHasMedia(msg) {
		return ignored(eventType, "unsupported_message_type", "")
	}

	// Chat identity normalization.
	chatType := sdk.ChatTypeGroup
	if msg.Chat.Type == "private" || msg.Chat.IsDirectMessages {
		chatType = sdk.ChatTypeDirect
	}
	platformChatID := strconv.FormatInt(msg.Chat.ID, 10)
	directTopicID := telegramDirectMessagesTopicID(msg)
	chatID := telegramInboundChatID(msg.BusinessConnectionID, platformChatID, directTopicID)
	senderID := telegramSenderID(msg)
	if senderID == "" {
		return ignored(eventType, "incomplete_message_identity", "")
	}

	messageIDStr := strconv.FormatInt(msg.MessageID, 10)
	if msg.MessageID == 0 || msg.Chat.ID == 0 {
		return ignored(eventType, "incomplete_message_identity", "")
	}
	dedupeKey := telegramDedupeKey(accountUUID, msg.BusinessConnectionID, update.UpdateID, msg.Chat.ID, msg.MessageID)
	stateKey := Platform + ":" + chatType + ":" + chatID
	if _, ok := st.InboundSeen[dedupeKey]; ok {
		return ignored(eventType, "duplicate", st.PeerSessions[stateKey])
	}

	botUsername := strings.TrimPrefix(firstString(st.BotUsername, account.State["bot_username"], account.Credential["bot_username"]), "@")
	mentions, mentionedMe, botMentionRanges := telegramMentionAnalysis(rawText, entities, botID, botUsername)
	text := strings.TrimSpace(stripUTF16Ranges(rawText, botMentionRanges))
	attachments, cleanupAttachments, attachmentErr := downloadTelegramAttachments(ctx, mediaClient, msg)
	if cleanupAttachments != nil {
		defer cleanupAttachments()
	}
	if text == "" && len(attachments) > 0 {
		text = telegramAttachmentPlaceholder(attachments)
	}
	if text == "" && attachmentErr != nil && telegramMessageHasMedia(msg) {
		text = telegramAttachmentFailurePlaceholder(msg)
	}
	if text == "" && !mentionedMe && len(attachments) == 0 {
		return ignored(eventType, "unsupported_message_type", "")
	}

	threadID := telegramThreadID(msg)
	chatDisplayName := telegramChatDisplayName(msg.Chat)
	senderDisplayName := telegramSenderDisplayName(msg)
	chatIdentityIDType := "telegram_chat_id"
	if msg.BusinessConnectionID != "" {
		chatIdentityIDType = "telegram_business_chat_id"
	} else if directTopicID != "" {
		chatIdentityIDType = "telegram_direct_messages_chat_id"
	}
	chatIdentity := sdk.ChatIdentity{
		ID:          chatID,
		IDType:      chatIdentityIDType,
		Type:        chatType,
		DisplayName: chatDisplayName,
	}
	referenced := telegramReferencedMessage(msg.ReplyToMessage, chatType, chatID)

	inbound := sdk.InboundMessage{
		WorkspaceUUID:     runtime.WorkspaceUUID,
		Platform:          Platform,
		AccountUUID:       accountUUID,
		ChannelUUID:       runtime.Channel.UUID,
		ChatType:          chatType,
		ChatID:            chatID,
		ThreadID:          threadID,
		ChatDisplayName:   chatDisplayName,
		ChatIdentity:      chatIdentity,
		SenderID:          senderID,
		SenderDisplayName: senderDisplayName,
		MessageID:         messageIDStr,
		Text:              text,
		Attachments:       attachments,
		ReferencedMessage: referenced,
		DedupeKey:         dedupeKey,
		Mentions:          mentions,
		MentionedMe:       mentionedMe,
		Raw: map[string]any{
			"business_connection_id":   msg.BusinessConnectionID,
			"direct_messages_topic_id": directTopicID,
			"update_id":                update.UpdateID,
			"update_type":              eventType,
			"message_id":               msg.MessageID,
			"message_thread_id":        msg.MessageThreadID,
			"message_type":             messageType,
			"chat_id":                  msg.Chat.ID,
			"telegram_chat_id":         platformChatID,
			"chat_display_name":        chatDisplayName,
			"chat_identity":            chatIdentity,
			"sender_id":                senderID,
			"sender_display_name":      senderDisplayName,
			"date":                     msg.Date,
			"mentions":                 mentions,
		},
	}
	if attachmentErr != nil {
		inbound.Raw["attachment_errors"] = []string{attachmentErr.Error()}
	}

	// Ensure the chat session (identity includes account uuid, so two bots
	// in the same group never share a session).
	sessionUUID, err := runtime.Gateway.EnsureChatSession(ctx, sdk.EnsureChatSessionRequest{
		WorkspaceUUID:       runtime.WorkspaceUUID,
		Platform:            Platform,
		AccountUUID:         accountUUID,
		ChatType:            chatType,
		ChatID:              chatID,
		ThreadID:            threadID,
		ChatDisplayName:     chatDisplayName,
		ChatIdentity:        chatIdentity,
		SenderID:            senderID,
		AgentParticipantID:  runtime.Gateway.AgentParticipantID(),
		BridgeParticipantID: runtime.Gateway.BridgeParticipantID(Platform),
		Metadata: map[string]any{
			"source":                   Platform,
			"platform":                 Platform,
			"account_uuid":             accountUUID,
			"business_connection_id":   msg.BusinessConnectionID,
			"direct_messages_topic_id": directTopicID,
			"telegram_chat_id":         platformChatID,
			"chat_display_name":        chatDisplayName,
			"chat_identity":            chatIdentity,
		},
	})
	if err != nil {
		return failed(err)
	}

	messageUUID, err := runtime.Gateway.CreateMessage(ctx, sdk.CreateMessageRequest{
		WorkspaceUUID: runtime.WorkspaceUUID,
		SessionUUID:   sessionUUID,
		SenderID:      sdk.IMPersonParticipantID(Platform, chatType, chatID, senderID),
		Content:       text,
		DedupeKey:     dedupeKey,
		Attachments:   inbound.Attachments,
		Metadata: map[string]any{
			"source":                            Platform,
			"platform":                          Platform,
			"account_uuid":                      accountUUID,
			"telegram_chat_type":                chatType,
			"telegram_chat_id":                  platformChatID,
			"telegram_common_chat_id":           chatID,
			"telegram_business_connection_id":   msg.BusinessConnectionID,
			"telegram_direct_messages_topic_id": directTopicID,
			"telegram_thread_id":                threadID,
			"telegram_message_id":               messageIDStr,
			"telegram_update_id":                update.UpdateID,
			"telegram_update_type":              eventType,
			"telegram_message_type":             messageType,
			"chat_display_name":                 chatDisplayName,
			"sender_display_name":               senderDisplayName,
			"inbound_message":                   inbound,
		},
	})
	if err != nil {
		return failed(err)
	}

	st.PeerSessions[stateKey] = sessionUUID
	st.InboundSeen[dedupeKey] = now.Format(time.RFC3339Nano)
	st.StreamLastEventAt = now
	if err := store.SaveAccount(ctx, st); err != nil {
		return nil, err
	}

	return &EventResult{
		Type:        eventType,
		SessionUUID: sessionUUID,
		MessageUUID: messageUUID,
		Inbound:     &inbound,
	}, nil
}

func telegramUpdateMessage(update *platform.Update) (string, *platform.Message) {
	if update == nil {
		return "update", nil
	}
	switch {
	case update.Message != nil:
		return "message", update.Message
	case update.EditedMessage != nil:
		return "edited_message", update.EditedMessage
	case update.ChannelPost != nil:
		return "channel_post", update.ChannelPost
	case update.EditedChannelPost != nil:
		return "edited_channel_post", update.EditedChannelPost
	case update.BusinessMessage != nil:
		return "business_message", update.BusinessMessage
	case update.EditedBusinessMessage != nil:
		return "edited_business_message", update.EditedBusinessMessage
	default:
		return "update", nil
	}
}

func telegramEditedEvent(eventType string) bool {
	switch eventType {
	case "edited_message", "edited_channel_post", "edited_business_message":
		return true
	default:
		return false
	}
}

func telegramMessageContent(message *platform.Message) (string, []platform.MessageEntity, string) {
	if message == nil {
		return "", nil, "unknown"
	}
	if message.Text != "" || len(message.Entities) > 0 {
		return message.Text, message.Entities, "text"
	}
	return message.Caption, message.CaptionEntities, telegramMessageType(message)
}

func telegramMessageHasMedia(message *platform.Message) bool {
	return message != nil && (len(message.Photo) > 0 || message.Document != nil || message.Video != nil || message.Audio != nil || message.Voice != nil || message.Animation != nil || message.Sticker != nil)
}

func downloadTelegramAttachments(ctx context.Context, client *platform.Client, message *platform.Message) ([]sdk.MediaAttachment, func(), error) {
	if !telegramMessageHasMedia(message) {
		return nil, nil, nil
	}
	attachments := make([]sdk.MediaAttachment, 0, 4)
	cleanups := make([]func(), 0, 4)
	cleanup := func() {
		for index := len(cleanups) - 1; index >= 0; index-- {
			if cleanups[index] != nil {
				cleanups[index]()
			}
		}
	}
	add := func(fileID, kind, name, contentType string) error {
		if strings.TrimSpace(fileID) == "" {
			return nil
		}
		path, remove, err := client.DownloadFile(ctx, fileID)
		if err != nil {
			return err
		}
		cleanups = append(cleanups, remove)
		attachments = append(attachments, sdk.MediaAttachment{ID: fileID, Kind: kind, Source: sdk.MediaSourcePath, Path: path, ContentType: contentType, FileName: name, PlatformResourceID: fileID})
		return nil
	}
	if len(message.Photo) > 0 {
		photo := message.Photo[len(message.Photo)-1]
		if err := add(photo.FileID, sdk.MediaKindImage, "photo.jpg", "image/jpeg"); err != nil {
			return attachments, cleanup, err
		}
	}
	if message.Document != nil {
		if err := add(message.Document.FileID, sdk.MediaKindFile, message.Document.FileName, message.Document.MimeType); err != nil {
			return attachments, cleanup, err
		}
	}
	if message.Video != nil {
		if err := add(message.Video.FileID, sdk.MediaKindVideo, message.Video.FileName, message.Video.MimeType); err != nil {
			return attachments, cleanup, err
		}
	}
	if message.Audio != nil {
		if err := add(message.Audio.FileID, sdk.MediaKindAudio, message.Audio.FileName, message.Audio.MimeType); err != nil {
			return attachments, cleanup, err
		}
	}
	if message.Voice != nil {
		if err := add(message.Voice.FileID, sdk.MediaKindAudio, "voice.ogg", message.Voice.MimeType); err != nil {
			return attachments, cleanup, err
		}
	}
	if message.Animation != nil {
		if err := add(message.Animation.FileID, sdk.MediaKindVideo, message.Animation.FileName, message.Animation.MimeType); err != nil {
			return attachments, cleanup, err
		}
	}
	if message.Sticker != nil {
		if err := add(message.Sticker.FileID, sdk.MediaKindSticker, message.Sticker.FileName, message.Sticker.MimeType); err != nil {
			return attachments, cleanup, err
		}
	}
	return attachments, cleanup, nil
}

func telegramAttachmentPlaceholder(attachments []sdk.MediaAttachment) string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		label := "附件"
		switch attachment.Kind {
		case sdk.MediaKindImage:
			label = "图片"
		case sdk.MediaKindFile:
			label = "文件"
		case sdk.MediaKindAudio:
			label = "音频"
		case sdk.MediaKindVideo:
			label = "视频"
		case sdk.MediaKindSticker:
			label = "贴纸"
		}
		if strings.TrimSpace(attachment.FileName) != "" && attachment.Kind == sdk.MediaKindFile {
			label += ": " + attachment.FileName
		}
		labels = append(labels, "["+label+"]")
	}
	return strings.Join(labels, " ")
}

func telegramAttachmentFailurePlaceholder(message *platform.Message) string {
	if message == nil {
		return "[附件下载失败]"
	}
	labels := make([]string, 0, 2)
	if len(message.Photo) > 0 {
		labels = append(labels, "[图片下载失败]")
	}
	if message.Document != nil {
		labels = append(labels, "[文件下载失败]")
	}
	if message.Audio != nil || message.Voice != nil {
		labels = append(labels, "[音频下载失败]")
	}
	if message.Video != nil || message.Animation != nil {
		labels = append(labels, "[视频下载失败]")
	}
	if message.Sticker != nil {
		labels = append(labels, "[贴纸下载失败]")
	}
	if len(labels) == 0 {
		return "[附件下载失败]"
	}
	return strings.Join(labels, " ")
}

func telegramMessageType(message *platform.Message) string {
	if message == nil {
		return "unknown"
	}
	switch {
	case len(message.Photo) > 0:
		return "photo"
	case message.Document != nil:
		return "document"
	case message.Video != nil:
		return "video"
	case message.Audio != nil:
		return "audio"
	case message.Voice != nil:
		return "voice"
	case message.Animation != nil:
		return "animation"
	case message.Sticker != nil:
		return "sticker"
	case message.Text != "":
		return "text"
	case message.Caption != "":
		return "caption"
	default:
		return "unknown"
	}
}

func telegramSenderID(message *platform.Message) string {
	if message == nil {
		return ""
	}
	if message.SenderChat != nil && message.SenderChat.ID != 0 {
		return strconv.FormatInt(message.SenderChat.ID, 10)
	}
	if message.From != nil && message.From.ID != 0 {
		return strconv.FormatInt(message.From.ID, 10)
	}
	if message.DirectMessagesTopic != nil && message.DirectMessagesTopic.User != nil && message.DirectMessagesTopic.User.ID != 0 {
		return strconv.FormatInt(message.DirectMessagesTopic.User.ID, 10)
	}
	if message.Chat.Type == "channel" && message.Chat.ID != 0 {
		return strconv.FormatInt(message.Chat.ID, 10)
	}
	return ""
}

func telegramChatDisplayName(chat platform.Chat) string {
	if name := strings.TrimSpace(strings.Join([]string{chat.FirstName, chat.LastName}, " ")); name != "" {
		return name
	}
	if title := strings.TrimSpace(chat.Title); title != "" {
		return title
	}
	if username := strings.TrimSpace(chat.Username); username != "" {
		return "@" + strings.TrimPrefix(username, "@")
	}
	if chat.ID != 0 {
		return strconv.FormatInt(chat.ID, 10)
	}
	return ""
}

func telegramUserDisplayName(user *platform.User) string {
	if user == nil {
		return ""
	}
	if name := strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " ")); name != "" {
		return name
	}
	if username := strings.TrimSpace(user.Username); username != "" {
		return "@" + strings.TrimPrefix(username, "@")
	}
	if user.ID != 0 {
		return strconv.FormatInt(user.ID, 10)
	}
	return ""
}

func telegramSenderDisplayName(message *platform.Message) string {
	if message == nil {
		return ""
	}
	if message.SenderChat != nil {
		return telegramChatDisplayName(*message.SenderChat)
	}
	if name := telegramUserDisplayName(message.From); name != "" {
		return name
	}
	if message.DirectMessagesTopic != nil {
		if name := telegramUserDisplayName(message.DirectMessagesTopic.User); name != "" {
			return name
		}
	}
	return telegramChatDisplayName(message.Chat)
}

func telegramThreadID(message *platform.Message) string {
	if directTopicID := telegramDirectMessagesTopicID(message); directTopicID != "" {
		return directThreadIDPrefix + directTopicID
	}
	if message == nil || message.MessageThreadID == 0 {
		return ""
	}
	return strconv.FormatInt(message.MessageThreadID, 10)
}

func telegramDirectMessagesTopicID(message *platform.Message) string {
	if message == nil || message.DirectMessagesTopic == nil || message.DirectMessagesTopic.TopicID <= 0 {
		return ""
	}
	return strconv.FormatInt(message.DirectMessagesTopic.TopicID, 10)
}

func telegramDedupeKey(accountUUID, businessConnectionID string, updateID, chatID, messageID int64) string {
	if updateID != 0 {
		return fmt.Sprintf("%s:update:%d", accountUUID, updateID)
	}
	if businessConnectionID != "" {
		return fmt.Sprintf("%s:business:%s:message:%d:%d", accountUUID, businessConnectionID, chatID, messageID)
	}
	return fmt.Sprintf("%s:message:%d:%d", accountUUID, chatID, messageID)
}

func telegramInboundChatID(businessConnectionID, chatID, directTopicID string) string {
	if businessConnectionID == "" {
		if directTopicID == "" {
			return chatID
		}
		return directChatIDPrefix + directTopicID + ":" + chatID
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(businessConnectionID))
	return businessChatIDPrefix + encoded + ":" + chatID
}

func telegramOutboundChatTarget(chatID string, raw map[string]any) (string, string, string, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return "", "", "", fmt.Errorf("%s outbound chat_id is required", Platform)
	}
	platformChatID := chatID
	businessConnectionID := ""
	directTopicID := ""
	if strings.HasPrefix(chatID, businessChatIDPrefix) {
		parts := strings.SplitN(strings.TrimPrefix(chatID, businessChatIDPrefix), ":", 2)
		if len(parts) != 2 || parts[0] == "" || strings.TrimSpace(parts[1]) == "" {
			return "", "", "", fmt.Errorf("%s business chat_id is invalid", Platform)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil || strings.TrimSpace(string(decoded)) == "" {
			return "", "", "", fmt.Errorf("%s business chat_id is invalid", Platform)
		}
		platformChatID = strings.TrimSpace(parts[1])
		businessConnectionID = string(decoded)
	} else if strings.HasPrefix(chatID, directChatIDPrefix) {
		parts := strings.SplitN(strings.TrimPrefix(chatID, directChatIDPrefix), ":", 2)
		if len(parts) != 2 || !positiveIntegerString(parts[0]) || strings.TrimSpace(parts[1]) == "" {
			return "", "", "", fmt.Errorf("%s direct messages chat_id is invalid", Platform)
		}
		directTopicID = parts[0]
		platformChatID = strings.TrimSpace(parts[1])
	}
	if rawChatID := strings.TrimSpace(stringValue(raw["telegram_chat_id"])); rawChatID != "" {
		platformChatID = rawChatID
	}
	if rawBusinessID := strings.TrimSpace(stringValue(raw["business_connection_id"])); rawBusinessID != "" {
		businessConnectionID = rawBusinessID
	}
	if rawDirectTopicID := strings.TrimSpace(firstString(raw["telegram_direct_messages_topic_id"], raw["direct_messages_topic_id"])); rawDirectTopicID != "" {
		if !positiveIntegerString(rawDirectTopicID) {
			return "", "", "", fmt.Errorf("%s direct_messages_topic_id is invalid", Platform)
		}
		directTopicID = rawDirectTopicID
	}
	return platformChatID, businessConnectionID, directTopicID, nil
}

func telegramOutboundThreadTarget(threadID string, raw map[string]any) (string, string, error) {
	threadID = strings.TrimSpace(threadID)
	messageThreadID := threadID
	directTopicID := ""
	if strings.HasPrefix(threadID, directThreadIDPrefix) {
		directTopicID = strings.TrimPrefix(threadID, directThreadIDPrefix)
		messageThreadID = ""
		if !positiveIntegerString(directTopicID) {
			return "", "", fmt.Errorf("%s direct messages thread_id is invalid", Platform)
		}
	}
	if rawDirectTopicID := strings.TrimSpace(firstString(raw["telegram_direct_messages_topic_id"], raw["direct_messages_topic_id"])); rawDirectTopicID != "" {
		if !positiveIntegerString(rawDirectTopicID) {
			return "", "", fmt.Errorf("%s direct_messages_topic_id is invalid", Platform)
		}
		directTopicID = rawDirectTopicID
		messageThreadID = ""
	}
	if messageThreadID != "" && !positiveIntegerString(messageThreadID) {
		return "", "", fmt.Errorf("%s message_thread_id is invalid", Platform)
	}
	return messageThreadID, directTopicID, nil
}

func positiveIntegerString(value string) bool {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return err == nil && parsed > 0
}

type utf16Range struct {
	start int
	end   int
}

func telegramMentionAnalysis(text string, entities []platform.MessageEntity, botID int64, botUsername string) ([]sdk.MentionIdentity, bool, []utf16Range) {
	botUsername = strings.TrimPrefix(strings.TrimSpace(botUsername), "@")
	mentions := make([]sdk.MentionIdentity, 0, len(entities))
	seen := make(map[string]struct{})
	botRanges := make([]utf16Range, 0, 1)
	mentionedMe := false
	appendMention := func(mention sdk.MentionIdentity) {
		key := mention.IDType + ":" + strings.ToLower(mention.ID)
		if mention.ID == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		mentions = append(mentions, mention)
	}

	for _, entity := range entities {
		visible, ok := sliceUTF16(text, entity.Offset, entity.Length)
		if !ok {
			continue
		}
		switch entity.Type {
		case "mention":
			username := strings.TrimPrefix(strings.TrimSpace(visible), "@")
			appendMention(sdk.MentionIdentity{ID: username, IDType: "telegram_username", DisplayName: visible})
			if botUsername != "" && strings.EqualFold(username, botUsername) {
				mentionedMe = true
				botRanges = append(botRanges, utf16Range{start: entity.Offset, end: entity.Offset + entity.Length})
			}
		case "text_mention":
			if entity.User == nil {
				continue
			}
			id := strconv.FormatInt(entity.User.ID, 10)
			appendMention(sdk.MentionIdentity{ID: id, IDType: "telegram_user_id", DisplayName: telegramUserDisplayName(entity.User)})
			if botID != 0 && entity.User.ID == botID {
				mentionedMe = true
				botRanges = append(botRanges, utf16Range{start: entity.Offset, end: entity.Offset + entity.Length})
			}
		case "bot_command":
			at := strings.LastIndex(visible, "@")
			if at < 0 || botUsername == "" || !strings.EqualFold(visible[at+1:], botUsername) {
				continue
			}
			mentionedMe = true
			appendMention(sdk.MentionIdentity{ID: botUsername, IDType: "telegram_username", DisplayName: "@" + botUsername})
			prefixUnits := len(utf16.Encode([]rune(visible[:at])))
			botRanges = append(botRanges, utf16Range{
				start: entity.Offset + prefixUnits,
				end:   entity.Offset + entity.Length,
			})
		}
	}
	return mentions, mentionedMe, botRanges
}

func sliceUTF16(value string, offset, length int) (string, bool) {
	units := utf16.Encode([]rune(value))
	if offset < 0 || length < 0 || offset > len(units) || offset+length > len(units) {
		return "", false
	}
	return string(utf16.Decode(units[offset : offset+length])), true
}

func stripUTF16Ranges(value string, ranges []utf16Range) string {
	if len(ranges) == 0 {
		return value
	}
	units := utf16.Encode([]rune(value))
	remove := make([]bool, len(units))
	for _, item := range ranges {
		start := item.start
		end := item.end
		if start < 0 {
			start = 0
		}
		if end > len(units) {
			end = len(units)
		}
		for i := start; i < end; i++ {
			remove[i] = true
		}
	}
	filtered := make([]uint16, 0, len(units))
	for i, unit := range units {
		if !remove[i] {
			filtered = append(filtered, unit)
		}
	}
	return string(utf16.Decode(filtered))
}

func telegramReferencedMessage(message *platform.Message, chatType, chatID string) *sdk.ReferencedMessage {
	if message == nil || message.MessageID == 0 {
		return nil
	}
	text, _, messageType := telegramMessageContent(message)
	reference := &sdk.ReferencedMessage{
		Platform:          Platform,
		MessageID:         strconv.FormatInt(message.MessageID, 10),
		ChatType:          chatType,
		ChatID:            chatID,
		ThreadID:          telegramThreadID(message),
		SenderID:          telegramSenderID(message),
		SenderDisplayName: telegramSenderDisplayName(message),
		MessageType:       messageType,
		Text:              strings.TrimSpace(text),
		Raw: map[string]any{
			"message_id":               message.MessageID,
			"message_thread_id":        message.MessageThreadID,
			"direct_messages_topic_id": telegramDirectMessagesTopicID(message),
			"date":                     message.Date,
		},
	}
	if message.Date > 0 {
		reference.CreatedAt = time.Unix(message.Date, 0).UTC().Format(time.RFC3339)
	}
	return reference
}

func telegramAckEmoji(req sdk.OutboundAck) string {
	value := firstString(req.Emoji, req.Intent)
	key := strings.ToLower(value)
	switch key {
	case "eyes", "eye", "processing", "process":
		return "👀"
	case "thinking", "think":
		return "🤔"
	case "typing", "keyboard", "⌨️", "⌨":
		return "👨‍💻"
	case "done", "complete", "completed", "check", "success", "✅":
		return "👍"
	case "wait", "waiting", "hourglass", "⏳":
		return "🙏"
	case "":
		return "👀"
	default:
		return value
	}
}

func webhookError(status int, code string, err error) error {
	return &sdk.WebhookError{StatusCode: status, Code: code, Err: err}
}

func lockInboundAccount(accountUUID string) func() {
	inboundAccountLocks.Lock()
	entry := inboundAccountLocks.items[accountUUID]
	if entry == nil {
		entry = &inboundAccountLock{}
		inboundAccountLocks.items[accountUUID] = entry
	}
	entry.refs++
	inboundAccountLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		inboundAccountLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(inboundAccountLocks.items, accountUUID)
		}
		inboundAccountLocks.Unlock()
	}
}

// connectorStateStore adapts sdk.AccountStore to typed state.AccountState.
type connectorStateStore struct {
	mu           sync.Mutex
	accounts     map[string]*state.AccountState
	rawStates    map[string]map[string]any
	seedStates   map[string]map[string]any
	accountStore sdk.AccountStore
}

func newConnectorStateStore(accountStore sdk.AccountStore) *connectorStateStore {
	return &connectorStateStore{
		accounts:     make(map[string]*state.AccountState),
		rawStates:    make(map[string]map[string]any),
		seedStates:   make(map[string]map[string]any),
		accountStore: accountStore,
	}
}

func (s *connectorStateStore) seed(account sdk.ChannelAccount) {
	accountID := accountKey(account)
	if accountID == "" {
		return
	}
	s.mu.Lock()
	s.seedStates[accountID] = cloneMap(account.State)
	s.mu.Unlock()
}

func (s *connectorStateStore) LoadAccount(ctx context.Context, accountID string) (*state.AccountState, error) {
	s.mu.Lock()
	if st, ok := s.accounts[accountID]; ok {
		s.mu.Unlock()
		return st, nil
	}
	accountStore := s.accountStore
	s.mu.Unlock()

	s.mu.Lock()
	raw := cloneMap(s.seedStates[accountID])
	s.mu.Unlock()
	if accountStore != nil {
		persisted, err := accountStore.LoadChannelAccountState(ctx, accountID)
		if err != nil {
			return nil, err
		}
		for key, value := range persisted {
			raw[key] = value
		}
	}
	st := &state.AccountState{AccountID: accountID}
	if len(raw) > 0 {
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("marshal %s account state: %w", Platform, err)
		}
		if err := json.Unmarshal(data, st); err != nil {
			return nil, fmt.Errorf("decode %s account state: %w", Platform, err)
		}
		st.AccountID = accountID
	}
	st.EnsureMaps()

	s.mu.Lock()
	s.accounts[accountID] = st
	s.rawStates[accountID] = cloneMap(raw)
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
		typed, err := state.MarshalAccount(account)
		if err != nil {
			return err
		}
		s.mu.Lock()
		persisted := cloneMap(s.rawStates[account.AccountID])
		for _, key := range managedAccountStateKeys {
			delete(persisted, key)
		}
		for key, value := range typed {
			persisted[key] = value
		}
		s.rawStates[account.AccountID] = cloneMap(persisted)
		s.mu.Unlock()
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
