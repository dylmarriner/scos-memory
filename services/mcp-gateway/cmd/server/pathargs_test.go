package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Every tool that interpolates an argument into an upstream URL path must
// declare it required, or a blank value silently hits the collection route.
func TestPathParameterToolsDeclareRequiredArgs(t *testing.T) {
	pathParams := map[string][]string{
		"scos_explain": {"memory_id"}, "scos_code_repo_get": {"repository_id"},
		"scos_correct": {"memory_id"}, "scos_forget": {"memory_id"},
		"scos_fact_feedback": {"memory_id"}, "scos_fact_history": {"memory_id"},
		"scos_agent_get": {"id"}, "scos_handoff_get": {"id"},
		"scos_control_decision_get": {"interaction_id"}, "scos_control_status_get": {"interaction_id"},
		"scos_backup_get": {"id"}, "scos_adapter_get": {"boundary"}, "scos_compat_agent_get": {"id"},
		"scos_exec_result_get": {"operation_id"}, "scos_exec_restore": {"handle"},
		"scos_extension_get": {"id"}, "scos_skill_get": {"id"}, "scos_hook_get": {"event"},
		"scos_mg_message_append": {"session_id"}, "scos_mg_chunk_append": {"turn_id"},
		"scos_mg_response_start": {"turn_id"}, "scos_mg_turn_complete": {"turn_id"},
		"scos_okf_document_get": {"id"}, "scos_okf_proposal_action": {"id", "action"},
		"scos_recovery_status_get": {"id"}, "scos_audit_get": {"id"}, "scos_secret_get": {"id"},
		"scos_tool_get": {"id"}, "scos_operation_get": {"id"},
		"scos_credential_get": {"id"}, "scos_credential_update": {"id"}, "scos_credential_delete": {"id"},
		"scos_credential_acl_grant": {"id"}, "scos_credential_acl_revoke": {"id", "principal_id"},
		"scos_trace_get": {"id"},
	}
	required := requiredToolArgs()
	for tool, params := range pathParams {
		if !governedToolNames()[tool] {
			t.Errorf("%s is not a governed tool", tool)
			continue
		}
		for _, p := range params {
			if !slices.Contains(required[tool], p) {
				t.Errorf("%s: path parameter %q is not declared required (have %v)", tool, p, required[tool])
			}
		}
	}
}

func TestMissingRequiredArgsRejectsBlank(t *testing.T) {
	if got := missingRequiredArgs("scos_forget", map[string]any{"memory_id": "  "}); !slices.Equal(got, []string{"memory_id"}) {
		t.Fatalf("blank memory_id not rejected: %v", got)
	}
	if got := missingRequiredArgs("scos_forget", map[string]any{"memory_id": "m1"}); len(got) != 0 {
		t.Fatalf("valid args rejected: %v", got)
	}
}

func TestPathArgEscapesTraversal(t *testing.T) {
	if got := pathArg(map[string]any{"id": "../admin/x"}, "id"); got != "..%2Fadmin%2Fx" {
		t.Fatalf("pathArg did not escape: %q", got)
	}
}

func TestLocalModeTokenIsVerifiableClaims(t *testing.T) {
	id := requestIdentity{TenantID: localModeTenantID, PrincipalID: localModePrincipalID}
	token, err := localModeToken(id, "issuer-secret")
	if err != nil {
		t.Fatal(err)
	}
	var got auth.Claims
	h := auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.CanonicalClaims(r.Context())
	}), auth.Config{IssuerSecret: "issuer-secret"})
	req := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.TenantID != localModeTenantID || got.PrincipalID != localModePrincipalID || !auth.HasScope(got, "memory:write") {
		t.Fatalf("downstream did not accept the local-mode token: %+v", got)
	}
	if time.Until(got.ExpiresAt) > 2*time.Minute {
		t.Fatalf("local-mode token must be short-lived, expires %v", got.ExpiresAt)
	}
}
