# Telegram Channel SDK

`github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram` connects
Telegram bot accounts to the Beak Channel Gateway. It implements the common
`sdk.Connector` interface and keeps Telegram-specific behavior inside the SDK.

Platform-specific logic includes credential validation, webhook verification,
update normalization, outbound delivery, topic threads, reply context, and
message reactions. Use `NewConnector()` to obtain a connector ready for Beak.

## Module

```
github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram
```

## Usage

```go
import (
    "fmt"

    beak "github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram"
)

func main() {
    connector := beak.NewConnector()
    fmt.Println(connector.Metadata().Label)
}
```

## Credential fields

These are the fields surfaced in the Beak console form (`CredentialSchema`):

| Key | Title | Secret | Required |
| --- | --- | --- | --- |
| `bot_token` | Bot Token | yes | yes |
| `webhook_secret` | Webhook Secret Token | yes | yes |

Backend-only values (base URL, callback URL, offsets, token cache) are never
exposed in the form.

## Event delivery

Mode: **webhook**. This SDK does not start `getUpdates` polling.

The Beak host owns the HTTP endpoint and forwards the raw request to
`HandleWebhookRequest`. Configure the same required `webhook_secret` when
calling Telegram `setWebhook`; Telegram then sends it in
`X-Telegram-Bot-Api-Secret-Token`. The SDK validates that header before parsing
the update. Do not enable `getUpdates` for the same bot while webhook delivery
is active.

Supported inbound update families:

- `message`, `channel_post`, and `business_message`
- text and media captions
- forum topic `message_thread_id`
- direct-message topics through `is_direct_messages` and `direct_messages_topic`
- embedded `reply_to_message` as the common `ReferencedMessage`

Telegram also delivers `edited_message`, `edited_channel_post`, and
`edited_business_message`. The SDK recognizes and explicitly ignores these
updates until the common Beak contract exposes a message-update operation.
Treating an edit as a new inbound message would incorrectly start another
agent turn.

Business updates preserve `business_connection_id` in an opaque common
`ChatID`, so Beak sessions cannot collide with a normal bot chat that happens
to use the same Telegram chat ID. `Send` reverses that target and includes the
connection ID in `sendMessage`. Outgoing business messages identified by
`sender_business_bot` are filtered as self echoes.

Direct-message topics are represented by an opaque common `ChatID` plus a
common `ThreadID`. `Send` decodes those values and sets
`direct_messages_topic_id`; the Beak adapter does not parse Telegram fields.

Telegram entity offsets are interpreted as UTF-16 code units. Bot mentions are
removed safely from the Beak-visible text, and a pure `@bot` event is still
delivered as a follow-up with `MentionedMe=true`.

## Outbound and acknowledgement

`Send` maps common text, thread, and username mention fields to `sendMessage`.
Messages longer than Telegram's 4096-character limit are split safely. Common
markdown currently degrades to plain text because CommonMark is not Telegram
HTML or MarkdownV2; the SDK never labels unconverted markdown as HTML.

Multipart sends require a stable common `MessageUUID` and
`sdk.Runtime.AccountStore`. The SDK checkpoints each acknowledged Telegram
part in `outbound_deliveries`, so an ordinary retry resumes with the first
unsent part instead of duplicating all earlier parts.

Telegram does not provide a portable `@all`, so `MentionAll` returns an explicit
SDK error. Username mentions are supported. `Acknowledge` supports the common
`reaction` mode through `setMessageReaction` and maps intents such as
`processing`, `thinking`, and `done` to platform emoji.

## Identity and dedupe

`ValidateCredential` calls `getMe`. The stable numeric bot id is the
`AccountKey` and `bot_identity.id`; the username is display and mention
metadata only. Inbound dedupe uses `update_id`, with a chat-scoped
`chat_id + message_id` fallback, because `message_id` is not globally unique.
Definitive authentication failures return `Valid=false`; temporary transport,
rate-limit, and server failures are retried briefly and then returned as Go
errors so the host does not mislabel a valid token as invalid.

## State

Account-scoped state lives in `state/state.go`. Common collections plus the
standard `stream_*` runtime-health fields are persisted through
`sdk.AccountStore`. Successful webhook receipt updates activity; only an event
created in Beak updates `stream_last_event_at`. Unknown host-owned state keys
are preserved when SDK state is saved. Unobserved health timestamps are omitted
instead of being serialized as year-one values.

## Verification

```bash
go test ./...
cd conformance && go test ./...
```

The workspace-level `beak-channel-sdk-conformance-tests` module also includes a
real Telegram adapter covering credential identity, UTF-16 mentions, pure
mentions, threads/replies, captions, reactions, and structured webhook errors.
