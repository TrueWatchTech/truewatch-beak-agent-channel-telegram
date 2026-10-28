package sdk

import (
	"strings"
	"testing"
)

func TestInteractionTokenBoundary(t *testing.T) {
	action := InteractionAction{RequestID: "approval_" + strings.Repeat("a", 32), Revision: 42, ActionID: "approve"}
	token, err := action.Token()
	if err != nil || len(token) > 64 {
		t.Fatalf("callback token exceeds Telegram boundary: %q %v", token, err)
	}
	parsed, err := ParseInteractionToken(token)
	if err != nil || parsed.RequestID != action.RequestID || parsed.Revision != 42 || parsed.ActionID != "approve" {
		t.Fatalf("callback identity changed: %#v %v", parsed, err)
	}
	for _, invalid := range []string{"", "a:0:approve:extra", "a:-1:approve", "a:0:execute", strings.Repeat("a", 65)} {
		if _, err := ParseInteractionToken(invalid); err == nil {
			t.Fatalf("accepted malformed action %q", invalid)
		}
	}
}

func TestTerminalInteractionHasNoActions(t *testing.T) {
	card := InteractionCard{RequestID: "approval_123", Kind: "tool_approval", State: "approved", Title: "Approval", Actions: []InteractionButton{{ID: "approve", Label: "Approve"}}}
	if err := card.Validate(); err == nil {
		t.Fatal("terminal card is actionable")
	}
	card.Actions = nil
	if err := card.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestInteractionRejectsMalformedInputContract(t *testing.T) {
	for _, inputs := range [][]InteractionInput{
		{{ID: "", Label: "Prompt"}},
		{{ID: "field", Label: "Prompt"}, {ID: "field", Label: "Duplicate"}},
		{{ID: "beak_action", Label: "Reserved action field"}},
		{{ID: "field", Label: "Prompt", Options: []InteractionButton{{ID: "choice", Label: "One"}, {ID: "choice", Label: "Duplicate"}}}},
	} {
		card := InteractionCard{RequestID: "req_1", Revision: 1, Kind: "input_request", State: "pending", Inputs: inputs}
		if card.Validate() == nil {
			t.Fatalf("accepted malformed inputs: %#v", inputs)
		}
	}
}
