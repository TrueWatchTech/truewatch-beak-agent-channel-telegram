package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TrueWatch/beak-agent-channel-telegram/sdk"
)

// DefaultBaseURL is the Telegram API base. Override via NewClient for private
// deployments or tests; do not surface it in CredentialSchema.
const DefaultBaseURL = "https://api.telegram.org"

const defaultRequestTimeout = 15 * time.Second

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
		return nil, fmt.Errorf("telegram bot_token is required")
	}

	var resp getMeResponse
	if err := c.doJSONBot(ctx, http.MethodGet, "getMe", nil, nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getMe: invalid token")
	}
	botID := strconv.FormatInt(resp.Result.ID, 10)
	return &BotInfo{
		AccountID:   botID,
		BotID:       botID,
		BotUserID:   resp.Result.Username,
		DisplayName: "@" + resp.Result.Username,
		BotName:     resp.Result.FirstName,
	}, nil
}

// SendText sends a message via POST /bot{token}/sendMessage and returns the
// Telegram message_id as a string. Format=markdown sends HTML parse_mode.
func (c *Client) SendText(ctx context.Context, chatID, text, format string, mentions []sdk.MentionIdentity, mentionAll bool) (string, error) {
	token := strings.TrimSpace(c.Credential["bot_token"])
	if token == "" {
		return "", fmt.Errorf("telegram bot_token is required")
	}
	if strings.TrimSpace(chatID) == "" {
		return "", fmt.Errorf("telegram chat_id is required")
	}
	_ = mentions
	_ = mentionAll

	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if strings.EqualFold(format, "markdown") {
		payload["parse_mode"] = "HTML"
	}

	var resp sendMessageResponse
	if err := c.doJSONBot(ctx, http.MethodPost, "sendMessage", nil, payload, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("telegram sendMessage failed")
	}
	return strconv.FormatInt(resp.Result.MessageID, 10), nil
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
		return err
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
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("telegram %s failed: status=%d body=%s", botMethod, resp.StatusCode, string(data))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram %s failed: status=%d body=%s", botMethod, resp.StatusCode, string(data))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// url builds a non-bot URL (kept for completeness; Telegram methods use botURL).
func (c *Client) url(path string, query map[string]string) string {
	base := strings.TrimRight(c.BaseURL, "/")
	values := url.Values{}
	for key, value := range query {
		if value != "" {
			values.Set(key, value)
		}
	}
	out := base + "/" + strings.TrimLeft(path, "/")
	if encoded := values.Encode(); encoded != "" {
		out += "?" + encoded
	}
	return out
}
