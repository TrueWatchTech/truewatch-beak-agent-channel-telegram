package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	maxTrackedStateKeys = 4096
	trackedStateTTL     = 7 * 24 * time.Hour
)

type AccountState struct {
	AccountID                  string                           `json:"account_id"`
	ChannelLinkSession         string                           `json:"channel_link_session,omitempty"`
	PeerSessions               map[string]string                `json:"peer_sessions,omitempty"`
	InboundSeen                map[string]string                `json:"inbound_seen,omitempty"`
	SentBeakMessages           map[string]string                `json:"sent_beak_messages,omitempty"`
	OutboundDeliveries         map[string]OutboundDeliveryState `json:"outbound_deliveries,omitempty"`
	StreamCursors              map[string]string                `json:"stream_cursors,omitempty"`
	BotID                      any                              `json:"bot_id,omitempty"`
	BotUsername                string                           `json:"bot_username,omitempty"`
	Offset                     int64                            `json:"offset,omitempty"`
	StreamConnectionState      string                           `json:"stream_connection_state,omitempty"`
	StreamConnectedAt          time.Time                        `json:"stream_connected_at,omitempty"`
	StreamDisconnectedAt       time.Time                        `json:"stream_disconnected_at,omitempty"`
	StreamLastActivityAt       time.Time                        `json:"stream_last_activity_at,omitempty"`
	StreamLastPingAt           time.Time                        `json:"stream_last_ping_at,omitempty"`
	StreamLastPongAt           time.Time                        `json:"stream_last_pong_at,omitempty"`
	StreamLastEventAt          time.Time                        `json:"stream_last_event_at,omitempty"`
	StreamLastError            string                           `json:"stream_last_error,omitempty"`
	StreamLastErrorAt          time.Time                        `json:"stream_last_error_at,omitempty"`
	StreamReconnectRequestedAt time.Time                        `json:"stream_reconnect_requested_at,omitempty"`
	StreamReconnectError       string                           `json:"stream_reconnect_error,omitempty"`
	StreamReconnectErrorAt     time.Time                        `json:"stream_reconnect_error_at,omitempty"`
	StreamSessionExpired       bool                             `json:"stream_session_expired,omitempty"`
	UpdatedAt                  time.Time                        `json:"updated_at"`
}

type OutboundDeliveryState struct {
	Fingerprint string   `json:"fingerprint"`
	MessageIDs  []string `json:"message_ids,omitempty"`
	Completed   bool     `json:"completed,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

func (a *AccountState) EnsureMaps() {
	if a == nil {
		return
	}
	if a.PeerSessions == nil {
		a.PeerSessions = make(map[string]string)
	}
	if a.InboundSeen == nil {
		a.InboundSeen = make(map[string]string)
	}
	if a.SentBeakMessages == nil {
		a.SentBeakMessages = make(map[string]string)
	}
	if a.OutboundDeliveries == nil {
		a.OutboundDeliveries = make(map[string]OutboundDeliveryState)
	}
	if a.StreamCursors == nil {
		a.StreamCursors = make(map[string]string)
	}
}

func TouchAccount(account *AccountState) error {
	if account == nil {
		return fmt.Errorf("account state is nil")
	}
	if account.AccountID == "" {
		return fmt.Errorf("account_id is required")
	}
	account.EnsureMaps()
	now := time.Now().UTC()
	pruneTimestampMap(account.InboundSeen, now)
	pruneTimestampMap(account.SentBeakMessages, now)
	pruneOutboundDeliveries(account.OutboundDeliveries, now)
	account.UpdatedAt = now
	return nil
}

// MarshalAccount returns a persistence map without time.Time zero values.
// encoding/json's omitempty does not omit zero-valued structs, so relying on it
// would publish year-one health timestamps that look like real observations.
func MarshalAccount(account *AccountState) (map[string]any, error) {
	data, err := json.Marshal(account)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	zeroTimes := map[string]time.Time{
		"stream_connected_at":           account.StreamConnectedAt,
		"stream_disconnected_at":        account.StreamDisconnectedAt,
		"stream_last_activity_at":       account.StreamLastActivityAt,
		"stream_last_ping_at":           account.StreamLastPingAt,
		"stream_last_pong_at":           account.StreamLastPongAt,
		"stream_last_event_at":          account.StreamLastEventAt,
		"stream_last_error_at":          account.StreamLastErrorAt,
		"stream_reconnect_requested_at": account.StreamReconnectRequestedAt,
		"stream_reconnect_error_at":     account.StreamReconnectErrorAt,
	}
	for key, value := range zeroTimes {
		if value.IsZero() {
			delete(values, key)
		}
	}
	return values, nil
}

func pruneOutboundDeliveries(values map[string]OutboundDeliveryState, now time.Time) {
	for key, value := range values {
		updatedAt, err := time.Parse(time.RFC3339Nano, value.UpdatedAt)
		if err != nil || now.Sub(updatedAt) > trackedStateTTL {
			delete(values, key)
		}
	}
	if len(values) <= maxTrackedStateKeys {
		return
	}
	type item struct {
		key string
		at  time.Time
	}
	items := make([]item, 0, len(values))
	for key, value := range values {
		updatedAt, _ := time.Parse(time.RFC3339Nano, value.UpdatedAt)
		items = append(items, item{key: key, at: updatedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	for len(values) > maxTrackedStateKeys && len(items) > 0 {
		delete(values, items[0].key)
		items = items[1:]
	}
}

func pruneTimestampMap(values map[string]string, now time.Time) {
	for key, raw := range values {
		if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil && now.Sub(ts) > trackedStateTTL {
			delete(values, key)
		}
	}
	if len(values) <= maxTrackedStateKeys {
		return
	}
	type item struct {
		key string
		at  time.Time
	}
	items := make([]item, 0, len(values))
	for key, raw := range values {
		ts, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			ts = time.Time{}
		}
		items = append(items, item{key: key, at: ts})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].at.Before(items[j].at)
	})
	for len(values) > maxTrackedStateKeys && len(items) > 0 {
		delete(values, items[0].key)
		items = items[1:]
	}
}
