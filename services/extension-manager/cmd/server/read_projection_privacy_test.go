package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Public discovery must not disclose operator-only configuration or
// hook destination credentials. The operator still needs the full record.
func TestExtensionDiscoveryRedactsOperatorSecrets(t *testing.T) {
	srv := newExtServerAt(t.TempDir() + "/state.json")
	const secret = "private-read-projection-sentinel"
	srv.extensions["private.ext"] = &Extension{
		Manifest: ExtensionManifest{
			PackageID: "private.ext", Name: "Discoverable",
			HookEndpoints: map[string]string{"memory.created": "https://hooks.invalid/" + secret},
		},
		Status:      "enabled",
		Config:      map[string]any{"api_token": secret, "nested": map[string]any{"secret": secret}},
		InstalledBy: "operator-tenant/private-principal",
	}
	srv.hooks["memory.created"] = []*Hook{{
		Event: "memory.created", ExtensionID: "private.ext",
		Endpoint: "https://hooks.invalid/" + secret, Type: "synchronous",
	}}
	cases := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{"list extensions", "/v1/extensions", srv.handleExtensions},
		{"get extension", "/v1/extensions/private.ext", srv.handleExtensionByID},
		{"get hooks", "/v1/hooks/memory.created", srv.handleHookByEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, operator := range []bool{false, true} {
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				claims := auth.Claims{TenantID: "tenant-a", PrincipalID: "user", Scopes: []string{"memory:read"}}
				if operator {
					claims.Scopes = []string{extensionsAdminScope}
				}
				req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
				w := httptest.NewRecorder()
				tc.handler(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				body := w.Body.String()
				if !strings.Contains(body, "private.ext") {
					t.Fatalf("discovery metadata missing: %s", body)
				}
				if operator && !strings.Contains(body, secret) {
					t.Fatalf("operator lost privileged detail: %s", body)
				}
				if !operator && (strings.Contains(body, secret) || strings.Contains(body, "private-principal")) {
					t.Fatalf("tenant read leaked operator detail: %s", body)
				}
				var decoded any
				if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	// Read projections must never scrub or mutate the canonical registration.
	if srv.extensions["private.ext"].Config["api_token"] != secret ||
		srv.extensions["private.ext"].Manifest.HookEndpoints["memory.created"] != "https://hooks.invalid/"+secret ||
		srv.hooks["memory.created"][0].Endpoint != "https://hooks.invalid/"+secret {
		t.Fatal("read projection mutated canonical extension or hook")
	}
}
