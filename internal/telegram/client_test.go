package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
	if info.BotUserID != "testbot" {
		t.Fatalf("BotUserID=%q want testbot", info.BotUserID)
	}
	if info.DisplayName != "@testbot" {
		t.Fatalf("DisplayName=%q want @testbot", info.DisplayName)
	}
}

func TestClientValidate_InvalidToken(t *testing.T) {
	httpClient := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"ok":false}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "bad"})
	client.HTTPClient = httpClient

	if _, err := client.Validate(context.Background()); err == nil {
		t.Fatal("expected error for invalid token")
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

func TestClientSendText(t *testing.T) {
	var sawPath string
	httpClient := &http.Client{Transport: testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sawPath = req.URL.Path
		return jsonResp(`{"ok":true,"result":{"message_id":42}}`)
	})}
	client := NewClient("", map[string]string{"bot_token": "123456:ABCdef"})
	client.HTTPClient = httpClient

	msgID, err := client.SendText(context.Background(), "-200", "hi", "", nil, false)
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

	if _, err := client.SendText(context.Background(), "-200", "hi", "", nil, false); err == nil {
		t.Fatal("expected error when ok=false")
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

func TestVerifyWebhookSignature_EmptySecret_Passes(t *testing.T) {
	// When no secret is configured, any header value (including empty) passes.
	if err := VerifyWebhookSignature("", "anything"); err != nil {
		t.Fatalf("empty configured secret should always pass: %v", err)
	}
}
