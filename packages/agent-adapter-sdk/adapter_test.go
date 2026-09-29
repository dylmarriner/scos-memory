package agentadaptersdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/envelope":
			if r.URL.Query().Get("session_id") != "s 1" {
				http.Error(w, "envelope not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(ContextEnvelope{AgentID: "a1", SessionID: "s 1", Facts: []string{"prefers Go"}, Format: "native"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/envelope/render":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["target_format"] != "markdown" || req["agent_id"] != "a1" {
				t.Errorf("unexpected render request %v", req)
			}
			_ = json.NewEncoder(w).Encode(RenderedEnvelope{Format: "markdown", Content: "# Context Envelope"})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestEnvelopeClient(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c, err := NewClient(AdapterConfig{AgentID: "a1", GatewayURL: srv.URL + "/", BearerToken: "jwt"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := c.GetEnvelope(context.Background(), "s 1")
	if err != nil || env.SessionID != "s 1" || len(env.Facts) != 1 {
		t.Fatalf("GetEnvelope: env=%+v err=%v", env, err)
	}
	if _, err := c.GetEnvelope(context.Background(), "missing"); !errors.Is(err, ErrEnvelopeNotFound) {
		t.Fatalf("expected ErrEnvelopeNotFound, got %v", err)
	}
	out, err := c.RenderEnvelope(context.Background(), "s 1", "markdown")
	if err != nil || out.Content != "# Context Envelope" {
		t.Fatalf("RenderEnvelope: out=%+v err=%v", out, err)
	}
	if _, err := c.RenderEnvelope(context.Background(), "s 1", "yaml"); err == nil {
		t.Fatal("expected unsupported format error")
	}
}

func TestEnvelopeClientSurfacesAuthFailure(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c, _ := NewClient(AdapterConfig{AgentID: "a1", GatewayURL: srv.URL, BearerToken: "wrong"})
	if _, err := c.GetEnvelope(context.Background(), "s 1"); err == nil {
		t.Fatal("auth failure must surface as an error, not a fallback envelope")
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, cfg := range []AdapterConfig{
		{GatewayURL: "http://x", BearerToken: "t"},
		{AgentID: "a", BearerToken: "t"},
		{AgentID: "a", GatewayURL: "http://x"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("expected validation error for %+v", cfg)
		}
	}
}
