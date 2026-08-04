package hermesadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHermesBridge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/sess-hermes-1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	bridge := &HermesBridge{EndpointURL: server.URL, TenantID: "tenant", PrincipalID: "principal"}
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
