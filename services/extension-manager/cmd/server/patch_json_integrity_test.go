package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A PATCH must be one complete JSON document. An accepted prefix cannot
// silently change an operator-managed extension when its tail is invalid.
func TestExtensionPatchRejectsTrailingJSONWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"second object", `{"config":{"mode":"unsafe"}}{"status":"disabled"}`},
		{"second scalar", `{"config":{"mode":"unsafe"}}true`},
		{"malformed tail", `{"config":{"mode":"unsafe"}}garbage`},
		{"agent scope tail", `{"agent_ids":["other-agent"]}garbage`},
		{"over limit tail", `{"config":{"mode":"unsafe"}}` + strings.Repeat(" ", 256<<10)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newExtServer()
			srv.extensions["ext"] = &Extension{
				Manifest: ExtensionManifest{PackageID: "ext"},
				Status:   "enabled",
				Config:   map[string]any{"mode": "safe"},
			}
			w := httptest.NewRecorder()
			srv.handleExtensionByID(w, operatorRequest(http.MethodPatch,
				"/v1/extensions/ext", strings.NewReader(tc.body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid trailing JSON accepted: status=%d body=%q", w.Code, w.Body.String())
			}
			ext := srv.extensions["ext"]
			if ext.Status != "enabled" || ext.Config["mode"] != "safe" || len(ext.AgentScope) != 0 {
				t.Fatalf("rejected PATCH changed extension state: %+v", ext)
			}
		})
	}
}

func TestExtensionPatchAllowsTrailingWhitespace(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext"] = &Extension{
		Manifest: ExtensionManifest{PackageID: "ext"},
		Status:   "enabled",
		Config:   map[string]any{"mode": "safe"},
	}
	w := httptest.NewRecorder()
	srv.handleExtensionByID(w, operatorRequest(http.MethodPatch,
		"/v1/extensions/ext", strings.NewReader(`  {"config":{"mode":"updated"}} `+"\n\t")))
	if w.Code != http.StatusOK || srv.extensions["ext"].Config["mode"] != "updated" {
		t.Fatalf("valid PATCH failed: status=%d body=%q", w.Code, w.Body.String())
	}
}
