package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

// DefaultBaseURL is the Telegram API base. Override via NewClient for private
// deployments or tests; do not surface it in CredentialSchema.
const DefaultBaseURL = "https://api.telegram.org"

const defaultRequestTimeout = 15 * time.Second

const (
	maxTelegramMessageRunes = 4096
	maxAPIResponseBytes     = 4 << 20
)

var ErrCredentialRejected = errors.New("telegram credential rejected")

// APIError preserves Telegram's machine-readable error classification without
// exposing the bot token embedded in the request URL.
type APIError struct {
	Method         string
	HTTPStatusCode int
	ErrorCode      int
	Description    string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	description := strings.TrimSpace(e.Description)
	if description == "" {
		description = "request rejected"
	}
	if e.ErrorCode != 0 {
		return fmt.Sprintf("telegram %s failed: error_code=%d description=%s", e.Method, e.ErrorCode, description)
	}
	if e.HTTPStatusCode != 0 {
		return fmt.Sprintf("telegram %s failed: status=%d description=%s", e.Method, e.HTTPStatusCode, description)
	}
	return fmt.Sprintf("telegram %s failed: %s", e.Method, description)
}

// TextRoute contains Telegram-native routing restored from common Beak fields.
type TextRoute struct {
	ChatID                string
	MessageThreadID       string
	DirectMessagesTopicID string
	BusinessConnectionID  string
}

// Client is the Telegram HTTP client. Credentials are kept in a map so the
// scaffold stays platform-agnostic.
//
// Credential fields supplied by CredentialSchema:
//   - bot_token: Bot Token
//   - webhook_secret: Webhook Secret Token
type Client struct {
	BaseURL        string
	Credential     map[string]string
	RequestTimeout time.Duration
	HTTPClient     *http.Client
}

func NewClient(baseURL string, credential map[string]string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	if credential == nil {
		credential = make(map[string]string)
	}
	return &Client{
		BaseURL:        baseURL,
		Credential:     credential,
		RequestTimeout: defaultRequestTimeout,
		HTTPClient:     http.DefaultClient,
	}
}

// botURL returns the full URL for a Telegram Bot API method.
// Telegram uses path /bot<token>/<Method>, not a Bearer header.
func (c *Client) botURL(method string, query map[string]string) string {
	token := strings.TrimSpace(c.Credential["bot_token"])
	base := strings.TrimRight(c.BaseURL, "/")
	values := url.Values{}
	for key, value := range query {
		if value != "" {
			values.Set(key, value)
		}
	}
	out := base + "/bot" + token + "/" + strings.TrimLeft(method, "/")
	if encoded := values.Encode(); encoded != "" {
		out += "?" + encoded
	}
	return out
}

// Validate calls GET /bot{token}/getMe and returns normalized bot identity.
// HTTP and parsing failures are returned as Go errors; an invalid token
// (Telegram returns ok=false) is also returned as a Go error and mapped to
// Valid=false by the connector.
func (c *Client) Validate(ctx context.Context) (*BotInfo, error) {
	token := strings.TrimSpace(c.Credential["bot_token"])
	if token == "" {
		return nil, fmt.Errorf("%w: bot_token is required", ErrCredentialRejected)
	}

	var resp getMeResponse
	if err := c.doJSONBot(ctx, http.MethodGet, "getMe", nil, nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, c.apiError("getMe", resp.ErrorCode, resp.Description)
	}
	if resp.Result.ID <= 0 || !resp.Result.IsBot {
		return nil, fmt.Errorf("%w: getMe returned an invalid bot identity", ErrCredentialRejected)
	}
	botID := strconv.FormatInt(resp.Result.ID, 10)
	displayName := strings.TrimSpace(resp.Result.FirstName)
	if username := strings.TrimSpace(resp.Result.Username); username != "" {
		displayName = "@" + username
	}
	if displayName == "" {
		displayName = "Telegram bot " + botID
	}
	return &BotInfo{
		AccountID:   botID,
		BotID:       botID,
		BotUserID:   botID,
		Username:    strings.TrimSpace(resp.Result.Username),
		DisplayName: displayName,
		BotName:     resp.Result.FirstName,
	}, nil
}

// SetWebhook registers the host-owned HTTPS endpoint and secret token for
// inbound Telegram updates. Calling it repeatedly with the same values is
// idempotent and is expected whenever the account runtime starts.
func (c *Client) SetWebhook(ctx context.Context, webhookURL, secret string) error {
	if strings.TrimSpace(c.Credential["bot_token"]) == "" {
		return fmt.Errorf("telegram bot_token is required")
	}
	webhookURL = strings.TrimSpace(webhookURL)
	parsed, err := url.Parse(webhookURL)
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") || strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("telegram webhook URL must be an absolute HTTPS URL")
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("telegram webhook secret is required")
	}
	payload := map[string]any{
		"url":                  webhookURL,
		"secret_token":         secret,
		"drop_pending_updates": false,
		"allowed_updates": []string{
			"message",
			"edited_message",
			"channel_post",
			"edited_channel_post",
			"business_message",
			"edited_business_message",
		},
	}
	var resp setWebhookResponse
	if err := c.doJSONBot(ctx, http.MethodPost, "setWebhook", nil, payload, &resp); err != nil {
		return err
	}
	if !resp.OK || !resp.Result {
		return c.apiError("setWebhook", resp.ErrorCode, resp.Description)
	}
	return nil
}

// SendText sends one or more Telegram messages and returns the final message ID.
// Common markdown currently degrades to plain text: forwarding CommonMark as
// Telegram HTML/MarkdownV2 would be incorrect and can make the API reject valid
// Beak output. Long messages are split on rune boundaries at Telegram's limit.
func (c *Client) SendText(ctx context.Context, chatID, threadID, text, format string, mentions []sdk.MentionIdentity, mentionAll bool) (string, error) {
	ids, err := c.SendTextParts(ctx, chatID, threadID, text, format, mentions, mentionAll)
	if err != nil {
		return "", err
	}
	return ids[len(ids)-1], nil
}

// DownloadFile resolves a Telegram file_id and copies it to a private
// temporary file. The cleanup function must be called after the host imports
// the attachment.
func (c *Client) DownloadFile(ctx context.Context, fileID string) (string, func(), error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return "", nil, fmt.Errorf("telegram file_id is required")
	}
	var resp getFileResponse
	if err := c.doJSONBot(ctx, http.MethodPost, "getFile", nil, map[string]any{"file_id": fileID}, &resp); err != nil {
		return "", nil, err
	}
	if !resp.OK || strings.TrimSpace(resp.Result.FilePath) == "" {
		return "", nil, c.apiError("getFile", resp.ErrorCode, resp.Description)
	}
	token := strings.TrimSpace(c.Credential["bot_token"])
	if token == "" {
		return "", nil, fmt.Errorf("telegram bot_token is required")
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/file/bot" + token + "/" + strings.TrimLeft(resp.Result.FilePath, "/")
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, fmt.Errorf("telegram file request build failed: %w", err)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", nil, fmt.Errorf("telegram file download failed: status=%d", response.StatusCode)
	}
	file, err := os.CreateTemp("", "beak-telegram-inbound-*")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	written, err := io.Copy(file, io.LimitReader(response.Body, 32<<20+1))
	if err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, err
	}
	if written > 32<<20 {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("telegram file download exceeds 32 MiB limit")
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// SendAttachment uploads one local attachment with the Telegram-native send
// method selected from its common Kind.
func (c *Client) SendAttachment(ctx context.Context, route TextRoute, attachment sdk.MediaAttachment) (string, error) {
	if err := sdk.ValidateMediaAttachment(attachment); err != nil {
		return "", err
	}
	path, cleanup, err := c.materializeOutboundAttachment(ctx, attachment)
	if err != nil {
		return "", err
	}
	defer cleanup()
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	method, field, err := telegramMediaMethod(attachment.Kind)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", route.ChatID); err != nil {
		return "", err
	}
	if route.MessageThreadID != "" {
		_ = writer.WriteField("message_thread_id", route.MessageThreadID)
	}
	if route.DirectMessagesTopicID != "" {
		_ = writer.WriteField("direct_messages_topic_id", route.DirectMessagesTopicID)
	}
	if route.BusinessConnectionID != "" {
		_ = writer.WriteField("business_connection_id", route.BusinessConnectionID)
	}
	if strings.TrimSpace(attachment.AltText) != "" {
		_ = writer.WriteField("caption", attachment.AltText)
	}
	part, err := writer.CreateFormFile(field, filepath.Base(firstNonEmpty(attachment.FileName, path)))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.botURL(method, nil), &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBytes+1))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", c.apiError(method, response.StatusCode, sanitizeBotToken(string(data), c.Credential["bot_token"]))
	}
	var result sendMessageResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", c.apiError(method, result.ErrorCode, result.Description)
	}
	if result.Result.MessageID <= 0 {
		return "", fmt.Errorf("telegram %s returned an invalid message_id", method)
	}
	return strconv.FormatInt(result.Result.MessageID, 10), nil
}

func (c *Client) materializeOutboundAttachment(ctx context.Context, attachment sdk.MediaAttachment) (string, func(), error) {
	if path := strings.TrimSpace(attachment.Path); path != "" {
		return path, func() {}, nil
	}
	if strings.TrimSpace(attachment.URL) == "" {
		return "", nil, fmt.Errorf("telegram outbound attachment requires a local path or HTTP URL")
	}
	parsed, err := url.Parse(strings.TrimSpace(attachment.URL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", nil, fmt.Errorf("telegram attachment URL must be absolute HTTP or HTTPS")
	}
	if blockedMediaHost(parsed.Hostname()) {
		return "", nil, fmt.Errorf("telegram attachment URL host is not allowed")
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", nil, err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", nil, fmt.Errorf("telegram attachment URL download failed: status=%d", response.StatusCode)
	}
	file, err := os.CreateTemp("", "beak-telegram-outbound-*")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	written, err := io.Copy(file, io.LimitReader(response.Body, 32<<20+1))
	if err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, err
	}
	if written > 32<<20 {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("telegram attachment URL exceeds 32 MiB limit")
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func blockedMediaHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	return false
}

func telegramMediaMethod(kind string) (method, field string, err error) {
	switch strings.TrimSpace(kind) {
	case sdk.MediaKindImage:
		return "sendPhoto", "photo", nil
	case sdk.MediaKindFile:
		return "sendDocument", "document", nil
	case sdk.MediaKindAudio:
		return "sendAudio", "audio", nil
	case sdk.MediaKindVideo:
		return "sendVideo", "video", nil
	case sdk.MediaKindSticker:
		return "sendSticker", "sticker", nil
	default:
		return "", "", fmt.Errorf("telegram unsupported attachment kind %q", kind)
	}
}

func (c *Client) requestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return defaultRequestTimeout
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// SendTextParts is SendText with all platform message IDs exposed for the
// connector's SendResult.Raw diagnostic field.
func (c *Client) SendTextParts(ctx context.Context, chatID, threadID, text, format string, mentions []sdk.MentionIdentity, mentionAll bool) ([]string, error) {
	return c.SendTextPartsWithBusiness(ctx, chatID, threadID, "", text, format, mentions, mentionAll)
}

// SendTextPartsWithBusiness is SendTextParts with Telegram Business routing.
// businessConnectionID must come from the inbound Message and must never be
// inferred from a display name or user id.
func (c *Client) SendTextPartsWithBusiness(ctx context.Context, chatID, threadID, businessConnectionID, text, format string, mentions []sdk.MentionIdentity, mentionAll bool) ([]string, error) {
	return c.SendTextPartsWithRouting(ctx, TextRoute{
		ChatID:               chatID,
		MessageThreadID:      threadID,
		BusinessConnectionID: businessConnectionID,
	}, text, format, mentions, mentionAll)
}

// SendTextPartsWithRouting sends all prepared parts for a Telegram-native
// route. Callers that need retry-safe multipart delivery should use
// PrepareTextParts and SendPreparedTextParts with a persisted startPart.
func (c *Client) SendTextPartsWithRouting(ctx context.Context, route TextRoute, text, format string, mentions []sdk.MentionIdentity, mentionAll bool) ([]string, error) {
	parts, err := PrepareTextParts(text, format, mentions, mentionAll)
	if err != nil {
		return nil, err
	}
	return c.SendPreparedTextParts(ctx, route, parts, 0, nil)
}

// PrepareTextParts performs common mention handling and Telegram-size
// splitting once, so a retry can resume from a stable part index.
func PrepareTextParts(text, format string, mentions []sdk.MentionIdentity, mentionAll bool) ([]string, error) {
	if mentionAll {
		return nil, fmt.Errorf("telegram does not support mention_all")
	}
	text = prependUsernameMentions(text, mentions)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("telegram text is required")
	}
	// Telegram's parse modes are not CommonMark. Degrade the common markdown
	// format to text until an exact renderer is selected; never label unconverted
	// CommonMark as HTML.
	_ = format
	return splitMessageText(text, maxTelegramMessageRunes), nil
}

// SendPreparedTextParts sends parts beginning at startPart. onPartSent is
// invoked after each remote success so the connector can persist progress
// before another part is attempted. Partial message IDs are returned on error.
func (c *Client) SendPreparedTextParts(ctx context.Context, route TextRoute, parts []string, startPart int, onPartSent func(index int, messageID string) error) ([]string, error) {
	token := strings.TrimSpace(c.Credential["bot_token"])
	if token == "" {
		return nil, fmt.Errorf("telegram bot_token is required")
	}
	if strings.TrimSpace(route.ChatID) == "" {
		return nil, fmt.Errorf("telegram chat_id is required")
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("telegram text parts are required")
	}
	if startPart < 0 || startPart > len(parts) {
		return nil, fmt.Errorf("telegram start part %d is outside 0..%d", startPart, len(parts))
	}
	messageThreadID, err := parsePositiveID("message_thread_id", route.MessageThreadID)
	if err != nil {
		return nil, err
	}
	directMessagesTopicID, err := parsePositiveID("direct_messages_topic_id", route.DirectMessagesTopicID)
	if err != nil {
		return nil, err
	}
	if messageThreadID != 0 && directMessagesTopicID != 0 {
		return nil, fmt.Errorf("telegram message_thread_id and direct_messages_topic_id are mutually exclusive")
	}

	ids := make([]string, 0, len(parts)-startPart)
	for index := startPart; index < len(parts); index++ {
		payload := map[string]any{
			"chat_id": route.ChatID,
			"text":    parts[index],
		}
		if route.BusinessConnectionID != "" {
			payload["business_connection_id"] = route.BusinessConnectionID
		}
		if messageThreadID != 0 {
			payload["message_thread_id"] = messageThreadID
		}
		if directMessagesTopicID != 0 {
			payload["direct_messages_topic_id"] = directMessagesTopicID
		}

		var resp sendMessageResponse
		if err := c.doJSONBot(ctx, http.MethodPost, "sendMessage", nil, payload, &resp); err != nil {
			return ids, err
		}
		if !resp.OK {
			return ids, c.apiError("sendMessage", resp.ErrorCode, resp.Description)
		}
		if resp.Result.MessageID <= 0 {
			return ids, fmt.Errorf("telegram sendMessage returned an invalid message_id")
		}
		messageID := strconv.FormatInt(resp.Result.MessageID, 10)
		ids = append(ids, messageID)
		if onPartSent != nil {
			if err := onPartSent(index, messageID); err != nil {
				return ids, fmt.Errorf("persist telegram outbound part %d: %w", index, err)
			}
		}
	}
	return ids, nil
}

func parsePositiveID(name, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("telegram %s must be a positive integer", name)
	}
	return parsed, nil
}

// SetMessageReaction applies a single standard emoji reaction to a Telegram
// message. Telegram bots support one non-paid reaction per message.
func (c *Client) SetMessageReaction(ctx context.Context, chatID, messageID, emoji string) error {
	if strings.TrimSpace(c.Credential["bot_token"]) == "" {
		return fmt.Errorf("telegram bot_token is required")
	}
	if strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("telegram chat_id is required")
	}
	parsedMessageID, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil || parsedMessageID <= 0 {
		return fmt.Errorf("telegram message_id must be a positive integer")
	}
	if strings.TrimSpace(emoji) == "" {
		return fmt.Errorf("telegram reaction emoji is required")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": parsedMessageID,
		"reaction": []map[string]string{{
			"type":  "emoji",
			"emoji": emoji,
		}},
	}
	var resp setMessageReactionResponse
	if err := c.doJSONBot(ctx, http.MethodPost, "setMessageReaction", nil, payload, &resp); err != nil {
		return err
	}
	if !resp.OK || !resp.Result {
		return c.apiError("setMessageReaction", resp.ErrorCode, resp.Description)
	}
	return nil
}

// doJSONBot is like doJSON but builds the URL using botURL (token in path).
func (c *Client) doJSONBot(ctx context.Context, method, botMethod string, query map[string]string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	timeout := c.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, c.botURL(botMethod, query), reader)
	if err != nil {
		return fmt.Errorf("telegram %s request build failed: %s", botMethod, sanitizeBotToken(err.Error(), c.Credential["bot_token"]))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "BeakAgentTelegram/0.1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return &redactedTransportError{
			message: sanitizeBotToken(fmt.Sprintf("telegram %s request failed: %v", botMethod, err), c.Credential["bot_token"]),
			cause:   transportCause(err),
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxAPIResponseBytes {
		return fmt.Errorf("telegram %s response exceeds %d bytes", botMethod, maxAPIResponseBytes)
	}
	bodyText := sanitizeBotToken(string(data), c.Credential["bot_token"])
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(data, &envelope)
		description := strings.TrimSpace(envelope.Description)
		if description == "" {
			description = bodyText
		}
		description = sanitizeBotToken(description, c.Credential["bot_token"])
		return &APIError{Method: botMethod, HTTPStatusCode: resp.StatusCode, ErrorCode: envelope.ErrorCode, Description: description}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

type redactedTransportError struct {
	message string
	cause   error
}

func (e *redactedTransportError) Error() string { return e.message }
func (e *redactedTransportError) Unwrap() error { return e.cause }

func transportCause(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

// IsCredentialRejected reports a definitive credential failure. Transient
// transport, rate-limit, and server errors intentionally return false so Beak
// can treat them as retryable runtime errors instead of invalid user input.
func IsCredentialRejected(err error) bool {
	if errors.Is(err, ErrCredentialRejected) {
		return true
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.ErrorCode
	if code == 0 {
		code = apiErr.HTTPStatusCode
	}
	return code == http.StatusBadRequest || code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
}

// IsRetryableError reports errors for which a bounded credential-validation
// retry is safe because getMe has no side effects.
func IsRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var transportErr *redactedTransportError
	if errors.As(err, &transportErr) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode
		if code == 0 {
			code = apiErr.HTTPStatusCode
		}
		return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func sanitizeBotToken(message, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return message
	}
	return strings.ReplaceAll(message, token, "<redacted>")
}

func (c *Client) apiError(method string, code int, description string) error {
	return &APIError{
		Method:      method,
		ErrorCode:   code,
		Description: sanitizeBotToken(description, c.Credential["bot_token"]),
	}
}

func prependUsernameMentions(text string, mentions []sdk.MentionIdentity) string {
	seen := make(map[string]struct{})
	prefixes := make([]string, 0, len(mentions))
	for _, mention := range mentions {
		id := strings.TrimSpace(mention.ID)
		idType := strings.ToLower(strings.TrimSpace(mention.IDType))
		if id == "" || (idType != "username" && idType != "telegram_username" && !strings.HasPrefix(id, "@")) {
			continue
		}
		username := "@" + strings.TrimPrefix(id, "@")
		key := strings.ToLower(username)
		if _, ok := seen[key]; ok || strings.Contains(strings.ToLower(text), key) {
			continue
		}
		seen[key] = struct{}{}
		prefixes = append(prefixes, username)
	}
	if len(prefixes) == 0 {
		return text
	}
	return strings.Join(prefixes, " ") + "\n" + text
}

func splitMessageText(value string, limit int) []string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return []string{value}
	}
	parts := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' || runes[i-1] == ' ' || runes[i-1] == '\t' {
				cut = i
				break
			}
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	return parts
}
