package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// These exercise real loopback HTTP requests, not only JSON decoder helpers.
// A valid prefix with a corrupt tail must not mutate extension or skill state.
func postIntegrityRequest(t *testing.T, handler http.Handler, path string, body []byte) (int, []byte) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	response, err := http.Post(server.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, result
}

func TestInstallRejectsTrailingJSONBeforeStateMutation(t *testing.T) {
	key := withSigningKey(t)
	manifest := sealManifest(t, validManifest("integrity.install"), key)
	prefix := mustJSON(t, manifest)
	cases := []struct {
		name, tail string
		want       int
	}{
		{"second object", `{"package_id":"shadow"}`, http.StatusBadRequest},
		{"second scalar", "true", http.StatusBadRequest},
		{"malformed tail", "garbage", http.StatusBadRequest},
		{"oversized whitespace", strings.Repeat(" ", 2<<20), http.StatusBadRequest},
		{"valid whitespace", " \n\t", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newExtServerAt(filepath.Join(t.TempDir(), "extension-state.json"))
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := auth.ContextWithClaims(r.Context(), auth.Claims{
					TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
				})
				srv.handleInstall(w, r.WithContext(ctx))
			})
			status, response := postIntegrityRequest(t, handler, "/v1/install", append(append([]byte{}, prefix...), tc.tail...))
			if status != tc.want {
				t.Fatalf("status=%d want=%d response=%q", status, tc.want, response)
			}
			srv.mu.RLock()
			extensions, receipts, hooks := len(srv.extensions), len(srv.receipts), len(srv.hooks)
			srv.mu.RUnlock()
			if tc.want == http.StatusBadRequest && (extensions != 0 || receipts != 0 || hooks != 0) {
				t.Fatalf("rejected install mutated state: extensions=%d receipts=%d hooks=%d", extensions, receipts, hooks)
			}
			if tc.want == http.StatusOK && (extensions != 1 || receipts != 1) {
				t.Fatalf("valid install missing: extensions=%d receipts=%d", extensions, receipts)
			}
		})
	}
}

func TestInstallDryRunRejectsTrailingJSONBeforeVerdict(t *testing.T) {
	key := withSigningKey(t)
	manifest := sealManifest(t, validManifest("integrity.dryrun"), key)
	prefix := mustJSON(t, manifest)
	cases := []struct {
		name, tail string
		want       int
	}{
		{"second document", `{"package_id":"shadow"}`, http.StatusBadRequest},
		{"malformed tail", "!", http.StatusBadRequest},
		{"oversized whitespace", strings.Repeat(" ", 2<<20), http.StatusBadRequest},
		{"valid whitespace", " \n\t", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newExtServer()
			status, response := postIntegrityRequest(t, http.HandlerFunc(srv.handleInstallDryRun), "/v1/install/dryrun", append(append([]byte{}, prefix...), tc.tail...))
			if status != tc.want {
				t.Fatalf("status=%d want=%d response=%q", status, tc.want, response)
			}
			if tc.want == http.StatusOK {
				var verdict struct {
					Accepted bool `json:"would_be_accepted"`
				}
				if err := json.Unmarshal(response, &verdict); err != nil {
					t.Fatal(err)
				}
				if !verdict.Accepted {
					t.Fatalf("valid signed manifest reported rejected: %s", response)
				}
			}
		})
	}
}

func TestSkillCreationRejectsTrailingJSONBeforeStateMutation(t *testing.T) {
	prefix := []byte(`{"name":"memory-search","purpose":"test"}`)
	cases := []struct {
		name, tail string
		want       int
	}{
		{"second document", `{"name":"shadow"}`, http.StatusBadRequest},
		{"malformed tail", "invalid", http.StatusBadRequest},
		{"oversized whitespace", strings.Repeat(" ", 512<<10), http.StatusBadRequest},
		{"valid whitespace", " \n\t", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newExtServer()
			status, response := postIntegrityRequest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				srv.handleSkills(w, r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope}})))
			}), "/v1/skills", append(append([]byte{}, prefix...), tc.tail...))
			if status != tc.want {
				t.Fatalf("status=%d want=%d response=%q", status, tc.want, response)
			}
			srv.mu.RLock()
			count, nextID := len(srv.skills), srv.nextID
			srv.mu.RUnlock()
			if tc.want == http.StatusBadRequest && (count != 0 || nextID != 0) {
				t.Fatalf("rejected skill created state: skills=%d nextID=%d", count, nextID)
			}
			if tc.want == http.StatusOK && (count != 1 || nextID != 1) {
				t.Fatalf("valid skill not registered: skills=%d nextID=%d", count, nextID)
			}
		})
	}
}
