package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (f testRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResp(body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestClientValidate(t *testing.T) {
	var sawPath string
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sawPath = req.URL.Path
		return jsonResp(`{"ok":true,"result":{"id":123456,"is_bot":true,"first_name":"Test Bot","username":"testbot"}}`)
	})}

	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	info, err := client.Validate(context.Background())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sawPath != "/bot123456:ABCdef/getMe" {
		t.Fatalf("expected /bot123456:ABCdef/getMe, saw %q", sawPath)
	}
	if info.AccountID != "123456" {
		t.Fatalf("AccountID=%q want 123456", info.AccountID)
	}
	if info.BotUserID != "123456" {
		t.Fatalf("BotUserID=%q want 123456", info.BotUserID)
	}
	if info.Username != "testbot" {
		t.Fatalf("Username=%q want testbot", info.Username)
	}
	if info.DisplayName != "@testbot" {
		t.Fatalf("DisplayName=%q want @testbot", info.DisplayName)
	}
}

func TestClientSendAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	var method string
	var payload string
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		method = req.URL.Path
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			return nil, err
		}
		file, _, err := req.FormFile("document")
		if err != nil {
			return nil, err
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}
		payload = string(body)
		return jsonResp(`{"ok":true,"result":{"message_id":991}}`)
	})}
	client := NewClient("https://telegram.test", map[string]string{"bot_token": "123:token"})
	client.HTTPClient = httpClient
	messageID, err := client.SendAttachment(context.Background(), TextRoute{ChatID: "-20", MessageThreadID: "8"}, sdk.MediaAttachment{
		ID: "asset-1", Kind: sdk.MediaKindFile, Source: sdk.MediaSourcePath, Path: path, FileName: "report.txt",
	})
	if err != nil {
		t.Fatalf("SendAttachment: %v", err)
	}
	if messageID != "991" || method != "/bot123:token/sendDocument" || payload != "report" {
		t.Fatalf("message_id=%q method=%q payload=%q", messageID, method, payload)
	}
}

func TestBlockedMediaHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "169.254.169.254", "10.0.0.1"} {
		if !blockedMediaHost(host) {
			t.Fatalf("host %q should be blocked", host)
		}
	}
}

func TestClientValidate_InvalidToken(t *testing.T) {
	httpClient := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "bad"})
	client.HTTPClient = httpClient

	if _, err := client.Validate(context.Background()); err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestClientValidate_RejectsInvalidBotIdentity(t *testing.T) {
	httpClient := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"ok":true,"result":{"id":0,"is_bot":false,"first_name":"Not a bot"}}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "bad"})
	client.HTTPClient = httpClient

	if _, err := client.Validate(context.Background()); err == nil {
		t.Fatal("expected invalid getMe identity to be rejected")
	}
}

func TestClientValidate_HTTPError(t *testing.T) {
	httpClient := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"Unauthorized"}`)),
		}, nil
	})}
	client := NewClient("", map[string]string{"bot_token": "bad"})
	client.HTTPClient = httpClient

	if _, err := client.Validate(context.Background()); err == nil {
		t.Fatal("expected error for HTTP 401")
	}
}

func TestClientSetWebhook(t *testing.T) {
	var sawPath string
	var payload map[string]any
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sawPath = req.URL.Path
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		return jsonResp(`{"ok":true,"result":true}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	err := client.SetWebhook(context.Background(), "https://beak.example/api/v1/channel-webhooks/telegram/acct-1", "generated_secret-1")
	if err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	if sawPath != "/bot123456:ABCdef/setWebhook" {
		t.Fatalf("path=%q", sawPath)
	}
	if payload["url"] != "https://beak.example/api/v1/channel-webhooks/telegram/acct-1" || payload["secret_token"] != "generated_secret-1" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestClientSetWebhookRejectsInvalidRuntimeConfiguration(t *testing.T) {
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	for _, tc := range []struct {
		name   string
		url    string
		secret string
	}{
		{name: "missing URL", secret: "secret"},
		{name: "relative URL", url: "/callback", secret: "secret"},
		{name: "insecure URL", url: "http://beak.example/callback", secret: "secret"},
		{name: "missing secret", url: "https://beak.example/callback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := client.SetWebhook(context.Background(), tc.url, tc.secret); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestClientSendText(t *testing.T) {
	var sawPath string
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sawPath = req.URL.Path
		return jsonResp(`{"ok":true,"result":{"message_id":42}}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	msgID, err := client.SendText(context.Background(), "-200", "", "hi", "", nil, false)
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if sawPath != "/bot123456:ABCdef/sendMessage" {
		t.Fatalf("expected /bot123456:ABCdef/sendMessage, saw %q", sawPath)
	}
	if msgID != "42" {
		t.Fatalf("msgID=%q want 42", msgID)
	}
}

func TestClientSendText_PlatformError(t *testing.T) {
	httpClient := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"ok":false,"description":"chat not found"}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	if _, err := client.SendText(context.Background(), "-200", "", "hi", "", nil, false); err == nil {
		t.Fatal("expected error when ok=false")
	}
}

func TestClientSendText_RejectsMissingMessageID(t *testing.T) {
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"ok":true,"result":{"message_id":0}}`)
	})}
	if _, err := client.SendText(context.Background(), "-200", "", "hi", "", nil, false); err == nil {
		t.Fatal("expected invalid message_id to be rejected")
	}
}

func TestClientSendText_ThreadMarkdownDegradesSafely(t *testing.T) {
	var payload map[string]any
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		return jsonResp(`{"ok":true,"result":{"message_id":42}}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	if _, err := client.SendText(context.Background(), "-200", "77", "**bold**", "markdown", nil, false); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if _, exists := payload["parse_mode"]; exists {
		t.Fatalf("unconverted CommonMark must not be labelled as Telegram HTML: %#v", payload)
	}
	if payload["message_thread_id"] != float64(77) {
		t.Fatalf("message_thread_id=%#v", payload["message_thread_id"])
	}
}

func TestClientSendText_DirectMessagesTopic(t *testing.T) {
	var payload map[string]any
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		return jsonResp(`{"ok":true,"result":{"message_id":42}}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	_, err := client.SendTextPartsWithRouting(context.Background(), TextRoute{
		ChatID: "-200", DirectMessagesTopicID: "88",
	}, "hello", "", nil, false)
	if err != nil {
		t.Fatalf("SendTextPartsWithRouting: %v", err)
	}
	if payload["direct_messages_topic_id"] != float64(88) {
		t.Fatalf("direct_messages_topic_id=%#v", payload["direct_messages_topic_id"])
	}
	if _, exists := payload["message_thread_id"]; exists {
		t.Fatalf("direct message topic must not use message_thread_id: %#v", payload)
	}
}

func TestClientSendText_UsernameMentionAndLongMessage(t *testing.T) {
	requests := 0
	var firstText string
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if requests == 1 {
			firstText, _ = payload["text"].(string)
		}
		return jsonResp(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, requests))
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	ids, err := client.SendTextParts(context.Background(), "-200", "", strings.Repeat("a", 5000), "", []sdk.MentionIdentity{{ID: "alice", IDType: "telegram_username"}}, false)
	if err != nil {
		t.Fatalf("SendTextParts: %v", err)
	}
	if requests != 2 || len(ids) != 2 || !strings.HasPrefix(firstText, "@alice\n") {
		t.Fatalf("requests=%d ids=%#v firstTextPrefix=%q", requests, ids, firstText[:min(len(firstText), 16)])
	}
}

func TestClientSendText_MentionAllRejected(t *testing.T) {
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	if _, err := client.SendText(context.Background(), "-200", "", "hello", "", nil, true); err == nil {
		t.Fatal("expected mention_all to be rejected explicitly")
	}
}

func TestClientTransportErrorRedactsBotToken(t *testing.T) {
	const token = "123456:super-secret-token"
	client := NewClient("", map[string]string{"bot_token": token})
	client.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial %s failed", req.URL.String())
	})}
	_, err := client.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("transport error leaked token: %v", err)
	}
}

func TestClientTransportTimeoutRemainsClassifiable(t *testing.T) {
	client := NewClient("", map[string]string{"bot_token": "123456:secret"})
	client.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	_, err := client.Validate(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout classification was lost: %v", err)
	}
}

func TestClientHTTPErrorBodyRedactsBotToken(t *testing.T) {
	const token = "123456:super-secret-token"
	client := NewClient("", map[string]string{"bot_token": token})
	client.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("proxy rejected " + token)),
		}, nil
	})}
	_, err := client.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("HTTP error leaked token: %v", err)
	}
}

func TestClientHTTPErrorJSONDescriptionRedactsBotToken(t *testing.T) {
	const token = "123456:super-secret-token"
	client := NewClient("", map[string]string{"bot_token": token})
	client.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"ok":false,"error_code":502,"description":"proxy rejected ` + token + `"}`,
			)),
		}, nil
	})}
	_, err := client.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("HTTP JSON error leaked token: %v", err)
	}
}

func TestClientOKFalseDescriptionRedactsBotToken(t *testing.T) {
	const token = "123456:super-secret-token"
	client := NewClient("", map[string]string{"bot_token": token})
	client.HTTPClient = &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"ok":false,"error_code":401,"description":"token ` + token + ` rejected"}`,
			)),
		}, nil
	})}
	_, err := client.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("ok=false error leaked token: %v", err)
	}
}

func TestIsRetryableErrorClassifiesTransientHTTPStatuses(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{status: http.StatusRequestTimeout, want: true},
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusBadGateway, want: true},
		{status: http.StatusUnauthorized, want: false},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			err := &APIError{Method: "getMe", HTTPStatusCode: tt.status}
			if got := IsRetryableError(err); got != tt.want {
				t.Fatalf("IsRetryableError(status=%d)=%v want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestVerifyWebhookSignature_Valid(t *testing.T) {
	if err := VerifyWebhookSignature("mysecret", "mysecret"); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
}

func TestVerifyWebhookSignature_Tampered(t *testing.T) {
	if err := VerifyWebhookSignature("mysecret", "wrongsecret"); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestVerifyWebhookSignature_EmptySecretRejected(t *testing.T) {
	if err := VerifyWebhookSignature("", "anything"); err == nil {
		t.Fatal("empty configured secret must be rejected")
	}
}
