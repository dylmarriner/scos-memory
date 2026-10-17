package main

import (
	"context"
	"encoding/json"
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

func TestEachCallGetsItsOwnIdempotencyKeyUnlessTheCallerNamesOne(t *testing.T) {
	var keys []string
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	s := &gatewayServer{client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer t")
	call := func(args map[string]any) {
		if _, err := s.forwardPOST(context.Background(), req, upstream.URL+"/v1/sessions/s/messages", args); err != nil {
			t.Fatal(err)
		}
	}
	call(map[string]any{"content": "yes"})
	call(map[string]any{"content": "yes"})
	call(map[string]any{"content": "yes", "idempotency_key": "turn-7-ack"})
	call(map[string]any{"content": "yes", "idempotency_key": "turn-7-ack"})
	if keys[0] == keys[1] {
		t.Fatal("two identical calls shared a key, so the second would be dropped as a replay")
	}
	if keys[2] != keys[3] || keys[2] == keys[0] {
		t.Fatalf("a caller-named retry must reuse its key: %v", keys)
	}
	if _, ok := bodies[2]["idempotency_key"]; ok {
		t.Fatal("idempotency_key was forwarded to the service")
	}
}

func TestPathArgumentsAreOneSegment(t *testing.T) {
	if got := pathArg(map[string]any{"id": "x/acl/admin"}, "id"); got != "x%2Facl%2Fadmin" {
		t.Fatalf("pathArg = %q", got)
	}
}
