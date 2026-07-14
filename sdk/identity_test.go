package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIdentityHelpers(t *testing.T) {
	if got := ChatSourceID("telegram", "account-1", "group", "chat-1"); got != "telegram:account-1:group:chat-1" {
		t.Fatalf("source id=%q", got)
	}
	participant := IMPersonParticipantID("telegram", "direct", "chat-1", "user-1")
	if participant != "im:telegram:direct:chat-1:user:user-1" {
		t.Fatalf("participant=%q", participant)
	}
	if !strings.Contains(participant, "telegram") {
		t.Fatalf("participant missing platform key: %q", participant)
	}
	bridge := BridgeParticipantID("telegram")
	if bridge != "bridge:telegram" {
		t.Fatalf("bridge=%q", bridge)
	}
	if !strings.Contains(bridge, "telegram") {
		t.Fatalf("bridge missing platform key: %q", bridge)
	}
}

func TestOutboundMessageCommonFormatContract(t *testing.T) {
	data, err := json.Marshal(OutboundMessage{
		Text:   "# title\n- item",
		Format: "markdown",
		Title:  "title",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["text"] != "# title\n- item" || decoded["format"] != "markdown" || decoded["title"] != "title" {
		t.Fatalf("common outbound json=%+v", decoded)
	}
}
