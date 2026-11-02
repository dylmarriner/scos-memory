package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Exercise the real HTTP routes with a publisher-signed manifest. Before
// strict decoding, encoding/json silently accepted the last of two fields;
// the signature still verified because it covered the last-wins manifest.
func TestInstallRoutesRejectAmbiguousSignedManifestJSON(t *testing.T) {
	key := withSigningKey(t)
	manifest := validManifest("signed-install")
	manifest.HooksSubscribed = []string{"turn.completed"}
	manifest.HookEndpoints = map[string]string{"turn.completed": "https://example.com/hook"}
	manifest.HookFallbackPolicies = map[string]string{"turn.completed": "continue"}
	manifest = sealManifest(t, manifest, key)
	clean := string(mustJSON(t, manifest))
	hookField := `"hook_endpoints":{"turn.completed":"https://example.com/hook"}`
	cases := []struct {
		name   string
		body   string
		reject bool
	}{
		{"clean signed manifest", clean, false},
		{"duplicate package identity", strings.Replace(clean, `"package_id":"signed-install"`, `"package_id":"shadow","package_id":"signed-install"`, 1), true},
		{"case folded package identity", strings.Replace(clean, `"package_id":"signed-install"`, `"PACKAGE_ID":"shadow","package_id":"signed-install"`, 1), true},
		{"escaped package identity", strings.Replace(clean, `"package_id":"signed-install"`, `"package_\u0069d":"shadow","package_id":"signed-install"`, 1), true},
		{"duplicate nested hook endpoint", strings.Replace(clean, hookField, `"hook_endpoints":{"turn.completed":"https://attacker.example/hook","turn.completed":"https://example.com/hook"}`, 1), true},
		{"duplicate nested fallback policy", strings.Replace(clean, `"hook_fallback_policies":{"turn.completed":"continue"}`, `"hook_fallback_policies":{"turn.completed":"fail_closed","turn.completed":"continue"}`, 1), true},
		{"trailing second document", clean + " {}", true},
	}
	for _, route := range []string{"/v1/install", "/v1/extensions/install", "/v1/install/dryrun"} {
		for _, tc := range cases {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				if tc.body == clean && tc.reject {
					t.Fatal("test mutation did not alter signed JSON")
				}
				srv := newExtServer()
				mux := http.NewServeMux()
				mux.HandleFunc("/v1/install", srv.handleInstall)
				mux.HandleFunc("/v1/extensions/install", srv.handleInstall)
				mux.HandleFunc("/v1/install/dryrun", srv.handleInstallDryRun)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					claims := auth.Claims{TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope}}
					mux.ServeHTTP(w, r.WithContext(auth.ContextWithClaims(r.Context(), claims)))
				}))
				defer server.Close()
				resp, err := server.Client().Post(server.URL+route, "application/json", bytes.NewBufferString(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				expected := http.StatusOK
				if tc.reject {
					expected = http.StatusBadRequest
				}
				if resp.StatusCode != expected {
					t.Fatalf("status %d, want %d for %s", resp.StatusCode, expected, tc.name)
				}
				if route == "/v1/install/dryrun" && !tc.reject {
					var result struct {
						WouldBeAccepted bool `json:"would_be_accepted"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
						t.Fatal(err)
					}
					if !result.WouldBeAccepted {
						t.Fatal("valid signed manifest rejected by dry-run")
					}
				}
				if tc.reject && (len(srv.extensions) != 0 || len(srv.hooks) != 0 || len(srv.receipts) != 0) {
					t.Fatalf("invalid manifest mutated state: extensions=%d hooks=%d receipts=%d", len(srv.extensions), len(srv.hooks), len(srv.receipts))
				}
			})
		}
	}
}
