package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Exercise the real HTTP transport: the complete bounded request, not just a
// valid JSON prefix, must be authenticated and parsed before a canary changes.
func canaryHTTP(t *testing.T, srv *extServer) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
			TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
		}))
		srv.handleExtensionByID(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func canaryPostHTTP(t *testing.T, server *httptest.Server, path string, body io.Reader) (int, string) {
	t.Helper()
	res, err := server.Client().Post(server.URL+path, "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(raw)
}

func TestCanaryStartRejectsTrailingJSONBeforeStateChange(t *testing.T) {
	for _, tc := range []struct{ name, suffix string }{
		{"second_object", `{"scope":{"organisation":true}}`},
		{"second_scalar", `true`},
		{"malformed_suffix", `garbage`},
		{"over_limit_whitespace", strings.Repeat(" ", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := canaryFixture(t)
			server := canaryHTTP(t, srv)
			payload := mustJSON(t, map[string]any{
				"manifest": canaryManifest(t, key, "2.0.0"),
				"scope":    CanaryScope{AgentIDs: []string{"agent-allowed"}},
			})
			status, response := canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary", strings.NewReader(string(payload)+tc.suffix))
			if status != http.StatusBadRequest {
				t.Fatalf("invalid suffix accepted: status=%d body=%s", status, response)
			}
			if len(srv.canaries) != 0 {
				t.Fatalf("invalid request changed active canary state: %+v", srv.canaries)
			}
			if restored := newExtServerAt(srv.storePath); len(restored.canaries) != 0 {
				t.Fatalf("rejected request persisted a canary: %+v", restored.canaries)
			}
			if got := resolve(t, srv, CanaryContext{AgentID: "agent-allowed"}); got.Channel != "stable" {
				t.Fatalf("rejected request activated candidate: %+v", got)
			}
		})
	}
}

func TestCanaryResolveRejectsTrailingJSON(t *testing.T) {
	for _, tc := range []struct{ name, suffix string }{
		{"second_object", `{"agent_id":"other"}`},
		{"second_scalar", `true`},
		{"malformed_suffix", `garbage`},
		{"over_limit_whitespace", strings.Repeat(" ", 64<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := canaryFixture(t)
			if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{AgentIDs: []string{"agent-allowed"}}); res.Code != http.StatusCreated {
				t.Fatalf("setup: %d %s", res.Code, res.Body.String())
			}
			server := canaryHTTP(t, srv)
			status, response := canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary/resolve", strings.NewReader(`{"agent_id":"agent-allowed"}`+tc.suffix))
			if status != http.StatusBadRequest {
				t.Fatalf("invalid resolve suffix accepted: status=%d body=%s", status, response)
			}
		})
	}
}

func TestCanaryJSONAllowsTrailingWhitespace(t *testing.T) {
	srv, key := canaryFixture(t)
	server := canaryHTTP(t, srv)
	payload := mustJSON(t, map[string]any{
		"manifest": canaryManifest(t, key, "2.0.0"),
		"scope":    CanaryScope{AgentIDs: []string{"agent-allowed"}},
	})
	status, body := canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary", bytes.NewReader(append(payload, []byte(" \n\t ")...)))
	if status != http.StatusCreated {
		t.Fatalf("valid canary rejected: %d %s", status, body)
	}
	status, body = canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary/resolve", strings.NewReader(`{"agent_id":"agent-allowed"}`+" \n\t "))
	if status != http.StatusOK {
		t.Fatalf("valid resolution rejected: %d %s", status, body)
	}
	var got VersionResolution
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Channel != "canary" {
		t.Fatalf("wrong resolution: %+v error=%v", got, err)
	}
}

func TestCanaryRejectsExtraPathSegments(t *testing.T) {
	srv, key := canaryFixture(t)
	server := canaryHTTP(t, srv)
	payload := mustJSON(t, map[string]any{
		"manifest": canaryManifest(t, key, "2.0.0"),
		"scope":    CanaryScope{Organisation: true},
	})
	status, body := canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary/unexpected", bytes.NewReader(payload))
	if status != http.StatusNotFound {
		t.Fatalf("unknown action accepted: %d %s", status, body)
	}
	status, body = canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary/resolve/extra", strings.NewReader(`{"agent_id":"x"}`))
	if status != http.StatusNotFound {
		t.Fatalf("extra path segment accepted: %d %s", status, body)
	}
}
