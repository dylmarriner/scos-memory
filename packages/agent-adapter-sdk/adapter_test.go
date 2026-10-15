package agentadaptersdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cli, err := NewClient(AdapterConfig{AgentID: "agent-1", Boundary: BoundaryHTTP, GatewayURL: server.URL + "/", BearerToken: "tok-1"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cli
}

func TestFetchEnvelopeReadsStoredEnvelope(t *testing.T) {
	cli := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/envelope" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("session_id"); got != "sess 1" {
			t.Errorf("session_id = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"tenant_id":"t","agent_id":"agent-1","session_id":"sess 1","facts":["f1"],"code_references":["a.go:1"],"format":"json"}`))
	})
	env, err := cli.FetchEnvelope(context.Background(), "sess 1")
	if err != nil {
		t.Fatalf("FetchEnvelope: %v", err)
	}
	if len(env.Facts) != 1 || env.Facts[0] != "f1" || env.CodeReferences[0] != "a.go:1" || env.Format != "json" {
		t.Fatalf("unexpected envelope %+v", env)
	}
}

func TestFetchEnvelopeNotFound(t *testing.T) {
	cli := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "envelope not found", http.StatusNotFound)
	})
	if _, err := cli.FetchEnvelope(context.Background(), "s"); !errors.Is(err, ErrEnvelopeNotFound) {
		t.Fatalf("err = %v, want ErrEnvelopeNotFound", err)
	}
}

func TestFetchEnvelopeSurfacesRefusal(t *testing.T) {
	cli := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "authenticated token has no tenant and principal claims", http.StatusUnauthorized)
	})
	_, err := cli.FetchEnvelope(context.Background(), "s")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "no tenant") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchEnvelopeRejectsOtherAgentOrSession(t *testing.T) {
	for name, body := range map[string]string{
		"agent":   `{"agent_id":"agent-2","session_id":"s","format":"json"}`,
		"session": `{"agent_id":"agent-1","session_id":"other","format":"json"}`,
		"garbage": `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			cli := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
			if env, err := cli.FetchEnvelope(context.Background(), "s"); err == nil {
				t.Fatalf("accepted %+v", env)
			}
		})
	}
}

func TestNewClientRequiresConfig(t *testing.T) {
	for _, cfg := range []AdapterConfig{
		{GatewayURL: "http://x", BearerToken: "t"},
		{AgentID: "a", BearerToken: "t"},
		{AgentID: "a", GatewayURL: "http://x"},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("NewClient(%+v) succeeded", cfg)
		}
	}
}
