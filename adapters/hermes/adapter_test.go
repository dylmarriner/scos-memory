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
		if r.URL.EscapedPath() != "/v1/sessions/sess%2Fhermes-1/messages" {
			t.Errorf("session ID not escaped into one path segment: %s", r.URL.EscapedPath())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer jwt" {
			t.Errorf("missing bearer token, got %q", got)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	bridge := &HermesBridge{EndpointURL: server.URL, BearerToken: "jwt", TenantID: "tenant", PrincipalID: "principal"}
	payload := &MemoryPayload{
		SessionID: "sess/hermes-1",
		Key:       "user_fact",
		Value:     "Prefers Go programming language",
		Timestamp: time.Now(),
	}

	if err := bridge.SyncTurn(context.Background(), payload); err != nil {
		t.Fatalf("unexpected error syncing Hermes turn: %v", err)
	}
}

func TestHermesBridgeRequiresIdentity(t *testing.T) {
	bridge := &HermesBridge{EndpointURL: "http://127.0.0.1:1"}
	err := bridge.SyncTurn(context.Background(), &MemoryPayload{SessionID: "s", Key: "k"})
	if err == nil {
		t.Fatal("expected an error without a bearer token or identity headers")
	}
}
