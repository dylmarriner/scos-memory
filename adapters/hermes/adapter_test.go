package hermesadapter

import (
	"context"
	"testing"
	"time"
)

func TestHermesBridge(t *testing.T) {
	bridge := &HermesBridge{EndpointURL: "http://127.0.0.1:8080"}
	payload := &MemoryPayload{
		SessionID: "sess-hermes-1",
		Key:       "user_fact",
		Value:     "Prefers Go programming language",
		Timestamp: time.Now(),
	}

	if err := bridge.SyncTurn(context.Background(), payload); err != nil {
		t.Fatalf("unexpected error syncing Hermes turn: %v", err)
	}
}
