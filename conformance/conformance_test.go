package telegramconformance

import (
	"testing"

	conformance "gitlab.jiagouyun.com/guance/beak-agent-channel-sdk/beak-channel-sdk-conformance"
)

func TestConformance(t *testing.T) {
	a := newAdapter()
	conformance.Run(t, conformance.Config{
		Platform:                 "telegram",
		MetadataProvider:         a,
		CredentialSchemaProvider: a,
		CredentialValidator:      a,
		InboundParser:            a,
		Acknowledger:             a,
		Sender:                   a,
		CredentialCases:          conformance.MustLoadJSON[[]conformance.CredentialValidationCase](t, "testdata/beak-conformance/credential_cases.json"),
		InboundCases:             conformance.MustLoadJSON[[]conformance.InboundCase](t, "testdata/beak-conformance/inbound_cases.json"),
		AckCases:                 conformance.MustLoadJSON[[]conformance.AckCase](t, "testdata/beak-conformance/ack_cases.json"),
		SendCases: []conformance.SendCase{{
			Name: "markdown outbound exposes common send result",
			Request: conformance.OutboundMessage{
				AccountUUID: "123456", ChatType: "group", ChatID: "-200", ThreadID: "77",
				MessageUUID: "message-send-telegram", Text: "**Telegram outbound**", Format: "markdown",
			},
			Expect: conformance.SendExpectation{MessageID: "301", RequiredRawKeys: []string{"message_ids"}},
		}},
	})
}
