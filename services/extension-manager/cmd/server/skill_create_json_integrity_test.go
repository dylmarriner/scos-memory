package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Operator-owned skills must not be created from ambiguous JSON: the Go
// decoder otherwise silently applies the last spelling of a field.
func TestDirectSkillCreateRejectsAmbiguousJSONBeforePersistence(t *testing.T) {
	cases := []struct{ name, body string }{
		{"duplicate name", `{"name":"approved","name":"shadow"}`},
		{"case folded name", `{"name":"approved","Name":"shadow"}`},
		{"escaped name", `{"name":"approved","\u006eame":"shadow"}`},
		{"duplicate capabilities", `{"name":"approved","capabilities":["memory.read"],"capabilities":["root.write"]}`},
		{"case folded permissions", `{"name":"approved","permissions":["memory.read"],"Permissions":["root.write"]}`},
		{"duplicate status", `{"name":"approved","status":"disabled","status":"active"}`},
		{"duplicate token budget", `{"name":"approved","token_budget":100,"token_budget":999999}`},
		{"duplicate required tools", `{"name":"approved","required_tools":["safe"],"required_tools":["unsafe"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "extension-state.json")
			srv := newExtServerAt(storePath)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := auth.ContextWithClaims(r.Context(), auth.Claims{
					TenantID: "operator-tenant", PrincipalID: "operator",
					Scopes: []string{extensionsAdminScope},
				})
				srv.handleSkills(w, r.WithContext(ctx))
			})
			status, response := postIntegrityRequest(t, handler, "/v1/skills", []byte(tc.body))
			if status != http.StatusBadRequest {
				t.Fatalf("ambiguous skill accepted: status=%d response=%q", status, response)
			}
			srv.mu.RLock()
			count, nextID := len(srv.skills), srv.nextID
			srv.mu.RUnlock()
			if count != 0 || nextID != 0 {
				t.Fatalf("rejected create changed live skills: count=%d nextID=%d", count, nextID)
			}
			restored := newExtServerAt(storePath)
			if len(restored.skills) != 0 || restored.nextID != 0 {
				t.Fatalf("rejected create changed persisted skills: %+v nextID=%d", restored.skills, restored.nextID)
			}
		})
	}
}

func TestDirectSkillCreateValidDocumentStillPersists(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "extension-state.json")
	srv := newExtServerAt(storePath)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.ContextWithClaims(r.Context(), auth.Claims{
			TenantID: "operator-tenant", PrincipalID: "operator",
			Scopes: []string{extensionsAdminScope},
		})
		srv.handleSkills(w, r.WithContext(ctx))
	})
	status, response := postIntegrityRequest(t, handler, "/v1/skills",
		[]byte("  "+`{"name":"approved","capabilities":["memory.read"],"status":"active"}`+" \n\t"))
	if status != http.StatusOK {
		t.Fatalf("valid skill rejected: status=%d response=%q", status, response)
	}
	restored := newExtServerAt(storePath)
	if restored.nextID != 1 || restored.skills["skill-1"] == nil ||
		restored.skills["skill-1"].Name != "approved" ||
		restored.skills["skill-1"].Status != "active" ||
		len(restored.skills["skill-1"].Capabilities) != 1 ||
		restored.skills["skill-1"].Capabilities[0] != "memory.read" ||
		!strings.Contains(string(response), `"approved"`) {
		t.Fatalf("valid skill not persisted: skills=%+v response=%q", restored.skills, response)
	}
}
