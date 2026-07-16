package state

import (
	"strconv"
	"testing"
	"time"
)

func TestTouchAccount_Validation(t *testing.T) {
	if err := TouchAccount(nil); err == nil {
		t.Fatal("expected error for nil account")
	}
	if err := TouchAccount(&AccountState{}); err == nil {
		t.Fatal("expected error for missing account_id")
	}

	acc := &AccountState{AccountID: "acc-1"}
	if err := TouchAccount(acc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.PeerSessions == nil || acc.InboundSeen == nil {
		t.Fatal("EnsureMaps did not initialize maps")
	}
	if acc.OutboundDeliveries == nil {
		t.Fatal("EnsureMaps did not initialize outbound deliveries")
	}
	if acc.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt was not set")
	}
}

func TestMarshalAccount_OmitsZeroHealthTimes(t *testing.T) {
	acc := &AccountState{AccountID: "acc-1"}
	if err := TouchAccount(acc); err != nil {
		t.Fatal(err)
	}
	values, err := MarshalAccount(acc)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"stream_connected_at", "stream_disconnected_at", "stream_last_activity_at",
		"stream_last_ping_at", "stream_last_pong_at", "stream_last_event_at",
		"stream_last_error_at", "stream_reconnect_requested_at", "stream_reconnect_error_at",
	} {
		if _, exists := values[key]; exists {
			t.Fatalf("zero time %s must be omitted: %#v", key, values)
		}
	}
	if _, exists := values["updated_at"]; !exists {
		t.Fatalf("updated_at must be persisted: %#v", values)
	}
}

func TestPruneTimestampMap_TTLExpiry(t *testing.T) {
	now := time.Now().UTC()
	values := map[string]string{
		"fresh": now.Format(time.RFC3339Nano),
		"stale": now.Add(-(trackedStateTTL + time.Hour)).Format(time.RFC3339Nano),
	}
	pruneTimestampMap(values, now)
	if _, ok := values["stale"]; ok {
		t.Error("stale entry beyond TTL was not pruned")
	}
	if _, ok := values["fresh"]; !ok {
		t.Error("fresh entry within TTL was pruned")
	}
}

func TestPruneTimestampMap_CapacityCap(t *testing.T) {
	now := time.Now().UTC()
	values := make(map[string]string)
	total := maxTrackedStateKeys + 10
	for i := 0; i < total; i++ {
		// All within TTL; staggered so the lowest indices are oldest.
		values["k"+strconv.Itoa(i)] = now.Add(-time.Duration(total-i) * time.Second).Format(time.RFC3339Nano)
	}
	pruneTimestampMap(values, now)
	if len(values) != maxTrackedStateKeys {
		t.Fatalf("expected capacity capped at %d, got %d", maxTrackedStateKeys, len(values))
	}
	// The oldest entry (k0) must have been evicted first.
	if _, ok := values["k0"]; ok {
		t.Error("oldest entry k0 was not evicted under capacity pressure")
	}
}
