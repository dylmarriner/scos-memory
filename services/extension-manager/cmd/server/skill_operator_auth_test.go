package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Direct skills are shared extension-manager state, not tenant-scoped objects.
// A tenant user must not create or remove skills without extensions:admin.
func TestDirectSkillMutationsRequireOperatorScope(t *testing.T) {
	for _, tc := range []struct {
		name, method, target, body string
	}{
		{"create", http.MethodPost, "/v1/skills", `{"name":"unapproved","capabilities":["memory.write"]}`},
		{"delete", http.MethodDelete, "/v1/skills/skill-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "state.json")
			srv := newExtServerAt(storePath)
			seed := httptest.NewRecorder()
			srv.handleSkills(seed, operatorRequest(http.MethodPost, "/v1/skills", strings.NewReader(`{"name":"existing"}`)))
			if seed.Code != http.StatusOK {
				t.Fatalf("operator seed failed: status=%d body=%s", seed.Code, seed.Body.String())
			}
			for _, identity := range []string{"tenant", "anonymous"} {
				req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
				if identity == "tenant" {
					req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
						TenantID: "tenant-a", PrincipalID: "ordinary-user",
						Scopes: []string{"memory:read"},
					}))
				}
				res := httptest.NewRecorder()
				if tc.method == http.MethodPost {
					srv.handleSkills(res, req)
				} else {
					srv.handleSkillByID(res, req)
				}
				if res.Code != http.StatusForbidden {
					t.Fatalf("%s %s accepted unauthorized mutation: status=%d body=%s", tc.name, identity, res.Code, res.Body.String())
				}
				if len(srv.skills) != 1 || srv.skills["skill-1"] == nil || srv.nextID != 1 {
					t.Fatalf("%s %s changed shared skills: %#v nextID=%d", tc.name, identity, srv.skills, srv.nextID)
				}
				restored := newExtServerAt(storePath)
				if len(restored.skills) != 1 || restored.skills["skill-1"] == nil || restored.nextID != 1 {
					t.Fatalf("%s %s changed persisted skills: %#v nextID=%d", tc.name, identity, restored.skills, restored.nextID)
				}
			}
			res := httptest.NewRecorder()
			req := operatorRequest(tc.method, tc.target, strings.NewReader(tc.body))
			if tc.method == http.MethodPost {
				srv.handleSkills(res, req)
			} else {
				srv.handleSkillByID(res, req)
			}
			if res.Code != http.StatusOK {
				t.Fatalf("operator %s rejected: status=%d body=%s", tc.name, res.Code, res.Body.String())
			}
		})
	}
}
