package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A backup or restore drill outlasts the short client; forwardLongPOST must
// still return the upstream result instead of abandoning the call.
func TestForwardLongPOSTOutlastsTheShortClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"backup-1","status":"completed"}`))
	}))
	defer upstream.Close()
	s := &gatewayServer{client: &http.Client{Timeout: 50 * time.Millisecond}, longClient: &http.Client{Timeout: 5 * time.Second}}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	if _, err := s.forwardPOST(context.Background(), r, upstream.URL, map[string]string{"type": "base_backup"}); err == nil {
		t.Fatal("the short client should give up on a slow upstream")
	}
	got, err := s.forwardLongPOST(context.Background(), r, upstream.URL, map[string]string{"type": "base_backup"})
	if err != nil {
		t.Fatalf("forwardLongPOST: %v", err)
	}
	if m, _ := got.(map[string]any); m["status"] != "completed" {
		t.Fatalf("unexpected result %#v", got)
	}
}
