// Command smoke is a manual, real-credential integration tool for
// beak-agent-channel-telegram. It is NOT part of the automated test suite:
// every credential is read from an environment variable, never hard-coded, and
// every call goes over the network to the real Telegram API. Use it to prove,
// with a live account, that:
//
//	validate            — the real Telegram API accepts the credential and the bot identity resolves
//	serve [addr]        — a real inbound webhook delivery is verified and parsed into a Beak message
//	send <chat_id> text — a real outbound message is delivered to a chat
//
// Usage:
//
//	smoke validate
//	smoke serve [addr]        (default addr ":8080")
//	smoke send <chat_id> <text...>
//
// Required/optional environment variables are listed by `smoke` with no
// arguments, or see the credential_fields in platforms/telegram.yaml.
//
// Telegram operator steps:
//  1. Create a bot via @BotFather; copy its token -> BEAK_BOT_TOKEN.
//  2. Optionally choose a random secret string -> BEAK_WEBHOOK_SECRET.
//  3. Run `smoke validate` — proves the token via getMe.
//  4. Run `smoke serve :8080` and expose it publicly (e.g. `ngrok http 8080`).
//  5. Point Telegram at it:
//     curl "https://api.telegram.org/bot<token>/setWebhook?url=https://<public-host>/&secret_token=<secret>"
//  6. Message the bot; watch this tool's stderr log for the verified inbound update.
//  7. Run `smoke send <chat_id> hello` — proves outbound sendMessage.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	beak "github.com/TrueWatch/beak-agent-channel-telegram"
	"github.com/TrueWatch/beak-agent-channel-telegram/sdk"
)

// credentialFieldSpec mirrors one entry of platforms/telegram.yaml's
// credential_fields, used to read credentials from the environment and to
// print usage.
type credentialFieldSpec struct {
	Key      string
	Title    string
	Required bool
}

var credentialFields = []credentialFieldSpec{
	{Key: "bot_token", Title: "Bot Token", Required: true},
	{Key: "webhook_secret", Title: "Webhook Secret Token", Required: false},
}

// envVarName derives the environment variable name for a credential key, e.g.
// "bot_token" -> "BEAK_BOT_TOKEN".
func envVarName(key string) string {
	return "BEAK_" + strings.ToUpper(key)
}

// loadCredentialFromEnv reads every credential field from its BEAK_* env var.
func loadCredentialFromEnv() (map[string]any, error) {
	credential := map[string]any{}
	var missing []string
	for _, f := range credentialFields {
		v := strings.TrimSpace(os.Getenv(envVarName(f.Key)))
		if v == "" {
			if f.Required {
				missing = append(missing, envVarName(f.Key))
			}
			continue
		}
		credential[f.Key] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variable(s): %s", strings.Join(missing, ", "))
	}
	return credential, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "smoke - manual real-API integration tool for beak-agent-channel-telegram")
	fmt.Fprintln(os.Stderr, "(see the file header comment for full per-platform operator steps)")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  smoke validate")
	fmt.Fprintln(os.Stderr, "  smoke serve [addr]")
	fmt.Fprintln(os.Stderr, "  smoke send <chat_id> <text...>")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Environment variables:")
	for _, f := range credentialFields {
		req := "optional"
		if f.Required {
			req = "required"
		}
		fmt.Fprintf(os.Stderr, "  %-28s (%s) %s\n", envVarName(f.Key), req, f.Title)
	}
}

// logGateway is a Gateway that logs every call instead of talking to a real
// Beak backend. Its EnsureChatSession/CreateMessage calls are the observable
// proof that a real inbound webhook delivery was verified and parsed.
type logGateway struct {
	logger *log.Logger
	mu     sync.Mutex
	nextID int
}

func (g *logGateway) EnsureChannel(_ context.Context, req sdk.EnsureChannelRequest) (string, error) {
	g.logger.Printf("EnsureChannel platform=%s name=%s", req.Platform, req.Name)
	return "smoke-channel", nil
}

func (g *logGateway) EnsureChannelLinkSession(_ context.Context, req sdk.EnsureChannelLinkSessionRequest) (string, error) {
	g.logger.Printf("EnsureChannelLinkSession account=%s", req.AccountUUID)
	return "smoke-link-" + req.AccountUUID, nil
}

func (g *logGateway) EnsureChatSession(_ context.Context, req sdk.EnsureChatSessionRequest) (string, error) {
	g.logger.Printf("EnsureChatSession chat_type=%s chat_id=%s sender=%s  <-- real inbound webhook reached the gateway", req.ChatType, req.ChatID, req.SenderID)
	return "smoke-session-" + req.ChatType + "-" + req.ChatID, nil
}

func (g *logGateway) CreateMessage(_ context.Context, req sdk.CreateMessageRequest) (string, error) {
	g.mu.Lock()
	g.nextID++
	id := fmt.Sprintf("smoke-message-%d", g.nextID)
	g.mu.Unlock()
	g.logger.Printf("CreateMessage session=%s sender=%s content=%q  <-- parsed inbound message", req.SessionUUID, req.SenderID, req.Content)
	if raw, ok := req.Metadata["inbound_message"]; ok {
		if data, err := json.MarshalIndent(raw, "", "  "); err == nil {
			g.logger.Printf("inbound_message:\n%s", data)
		}
	}
	return id, nil
}

func (g *logGateway) StreamSession(context.Context, sdk.StreamSessionRequest, func(sdk.StreamEvent) error) error {
	return nil
}

func (g *logGateway) AgentParticipantID() string { return "agent:smoke" }

func (g *logGateway) BridgeParticipantID(platform string) string {
	return sdk.BridgeParticipantID(platform)
}

var _ sdk.Gateway = (*logGateway)(nil)

// memAccountStore is an in-memory AccountStore. State does NOT persist across
// process restarts: for a platform whose outbound send depends on state
// captured from a prior inbound delivery (Teams' conversation reference),
// `serve` and `send` must observe the same state within one process lifetime.
type memAccountStore struct {
	mu     sync.Mutex
	states map[string]map[string]any
}

func newMemAccountStore() *memAccountStore {
	return &memAccountStore{states: make(map[string]map[string]any)}
}

func (s *memAccountStore) LoadChannelAccountState(_ context.Context, accountUUID string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[accountUUID], nil
}

func (s *memAccountStore) SaveChannelAccountState(_ context.Context, accountUUID string, state map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[accountUUID] = state
	return nil
}

var _ sdk.AccountStore = (*memAccountStore)(nil)

func printJSON(label string, v any) {
	if v == nil {
		return
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Printf("%s: <error: %v>\n", label, err)
		return
	}
	fmt.Printf("%s:\n%s\n", label, data)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	credential, err := loadCredentialFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(2)
	}

	var connector beak.Connector
	logger := log.New(os.Stderr, "[smoke] ", log.LstdFlags)
	gateway := &logGateway{logger: logger}
	store := newMemAccountStore()

	account := sdk.ChannelAccount{
		UUID:       "smoke-account",
		Platform:   beak.Platform,
		Credential: credential,
	}

	runtime := sdk.Runtime{
		WorkspaceUUID: "smoke-workspace",
		Channel:       sdk.Channel{UUID: "smoke-channel", Platform: beak.Platform},
		Account:       account,
		Accounts:      []sdk.ChannelAccount{account},
		Gateway:       gateway,
		AccountStore:  store,
		HTTPClient:    http.DefaultClient,
		Logger:        logger,
	}

	ctx := context.Background()

	switch os.Args[1] {
	case "validate":
		runValidate(ctx, connector, runtime, account)
	case "serve":
		addr := ":8080"
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		runServe(ctx, connector, runtime, account, addr)
	case "send":
		if len(os.Args) < 4 {
			usage()
			os.Exit(2)
		}
		runSend(ctx, connector, runtime, os.Args[2], strings.Join(os.Args[3:], " "))
	default:
		usage()
		os.Exit(2)
	}
}

// runValidate proves real API connectivity + authentication: it calls
// ValidateCredential, which hits the real Telegram API through
// runtime.HTTPClient, and prints the resolved bot identity.
func runValidate(ctx context.Context, connector beak.Connector, runtime sdk.Runtime, account sdk.ChannelAccount) {
	result, err := connector.ValidateCredential(ctx, sdk.CredentialValidationRequest{
		WorkspaceUUID: runtime.WorkspaceUUID,
		ChannelUUID:   runtime.Channel.UUID,
		Platform:      beak.Platform,
		Credential:    account.Credential,
		State:         account.State,
		Runtime:       runtime,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "validate: connector error:", err)
		os.Exit(1)
	}
	fmt.Printf("Valid:       %v\n", result.Valid)
	fmt.Printf("AccountKey:  %s\n", result.AccountKey)
	fmt.Printf("DisplayName: %s\n", result.DisplayName)
	if result.Error != "" {
		fmt.Printf("Error:       %s\n", result.Error)
	}
	// State/Metadata never carry raw secrets (see connector.go ValidateCredential);
	// the credential map itself is intentionally never printed.
	printJSON("State", result.State)
	printJSON("Metadata", result.Metadata)
	if !result.Valid {
		os.Exit(1)
	}
}

// runServe proves real inbound webhook handling: it starts an HTTP server that
// hands every request to the connector's HandleWebhookRequest, which verifies
// the request (signature/secret/JWT per platform) and, on success, drives
// logGateway.EnsureChatSession/CreateMessage — the printed evidence of a real,
// verified inbound delivery.
func runServe(ctx context.Context, connector beak.Connector, runtime sdk.Runtime, account sdk.ChannelAccount, addr string) {
	// Validate first so `serve` fails fast on bad credentials, and so the bot
	// identity fields (e.g. bot_id/team_id/bot_identity) that inbound self-echo
	// and ownership checks rely on are populated before any webhook arrives.
	result, err := connector.ValidateCredential(ctx, sdk.CredentialValidationRequest{
		WorkspaceUUID: runtime.WorkspaceUUID,
		ChannelUUID:   runtime.Channel.UUID,
		Platform:      beak.Platform,
		Credential:    account.Credential,
		State:         account.State,
		Runtime:       runtime,
	})
	if err != nil || !result.Valid {
		fmt.Fprintln(os.Stderr, "serve: credential validation failed; aborting before opening the webhook listener")
		if err != nil {
			fmt.Fprintln(os.Stderr, "  error:", err)
		} else {
			fmt.Fprintln(os.Stderr, "  error:", result.Error)
		}
		os.Exit(1)
	}
	account.Credential = result.Credential
	account.State = result.State
	account.DisplayName = result.DisplayName
	runtime.Account = account
	runtime.Accounts = []sdk.ChannelAccount{account}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp, err := connector.HandleWebhookRequest(r.Context(), runtime, account, r)
		if err != nil {
			logger := runtime.Logger
			logger.Println("webhook rejected:", err)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		statusCode := resp.StatusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		w.WriteHeader(statusCode)
		if len(resp.Body) > 0 {
			_, _ = w.Write(resp.Body)
		}
	})

	runtime.Logger.Printf("listening on %s for Telegram webhook deliveries (bot=%s)", addr, result.DisplayName)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// runSend proves real outbound delivery: it calls Send, which posts to the
// real Telegram API through runtime.HTTPClient.
func runSend(ctx context.Context, connector beak.Connector, runtime sdk.Runtime, chatID, text string) {
	result, err := connector.Send(ctx, runtime, sdk.OutboundMessage{
		WorkspaceUUID: runtime.WorkspaceUUID,
		Platform:      beak.Platform,
		AccountUUID:   runtime.Account.UUID,
		ChatType:      sdk.ChatTypeGroup,
		ChatID:        chatID,
		Text:          text,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "send: connector error:", err)
		os.Exit(1)
	}
	fmt.Printf("SendResult: platform=%s account=%s message_id=%s\n", result.Platform, result.AccountUUID, result.MessageID)
	printJSON("Raw", result.Raw)
}
