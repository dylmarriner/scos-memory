package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewHermesServer(t *testing.T) {
	s := newHermesServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.startTime.IsZero() {
		t.Fatal("expected start time to be set")
	}
}

func TestProviderStatus(t *testing.T) {
	status := ProviderStatus{
		Name:        providerName,
		Version:     version,
		IsAvailable: true,
		Healthy:     true,
		Uptime:      "5s",
	}
	if status.Name != "scos-nexus" {
		t.Fatal("expected scos-nexus provider name")
	}
	if !status.IsAvailable {
		t.Fatal("expected available")
	}
}

func TestHandleStatusReflectsRealReadiness(t *testing.T) {
	srv := newHermesServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/provider/status", nil)
	rec := httptest.NewRecorder()
	srv.handleStatus(rec, req)

	var status ProviderStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if status.Healthy || status.IsAvailable {
		t.Fatalf("expected unhealthy status without a configured postgres dependency, got %+v", status)
	}
}

func TestInitializeRequiresIdentityProfileAndStore(t *testing.T) {
	srv := newHermesServer()
	call := func(body map[string]string, withIdentity bool) int {
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/v1/provider/initialize", bytes.NewReader(encoded))
		if withIdentity {
			r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
			r.Header.Set("X-SCOS-Principal-ID", "principal-1")
		}
		w := httptest.NewRecorder()
		srv.handleInit(w, r)
		return w.Code
	}
	profile := map[string]string{"profile_path": "/home/user/.hermes/profiles/default"}
	if code := call(map[string]string{}, true); code != http.StatusBadRequest {
		t.Fatalf("missing profile_path: %d", code)
	}
	if code := call(profile, false); code != http.StatusUnauthorized {
		t.Fatalf("missing identity: %d", code)
	}
	// Without a store there is nothing to bind the profile to; initialize
	// must not report success.
	if code := call(profile, true); code != http.StatusServiceUnavailable {
		t.Fatalf("initialize without a store: %d", code)
	}
}

func TestToolSchemas(t *testing.T) {
	srv := newHermesServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/provider/tool_schemas", nil)
	w := httptest.NewRecorder()
	srv.handleToolSchemas(w, r)
	var tools []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&tools); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		got[name] = true
		if _, ok := tool["input_schema"]; !ok {
			t.Errorf("tool %q has no input schema", name)
		}
	}
	for _, want := range []string{
		"scos_recall", "scos_explain", "scos_user_model", "scos_memory_graph",
		"scos_correct", "scos_forget", "scos_status",
	} {
		if !got[want] {
			t.Errorf("missing governed tool: %s", want)
		}
	}
	if len(tools) != 7 {
		t.Fatalf("expected the 7 governed tools, got %d", len(tools))
	}
}

func TestMemoryWriteRequestShape(t *testing.T) {
	req := MemoryWriteRequest{
		Key:         "user.name",
		Value:       "Alice",
		Scope:       "user",
		Source:      "user_message",
		NamespaceID: "00000000-0000-4000-8000-000000000000",
	}
	if req.Scope != "user" {
		t.Fatal("scope mismatch")
	}
	if req.Source != "user_message" {
		t.Fatal("source mismatch")
	}
}

func TestMirroredMemoryTransitionUsesPriorContentHash(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name       string
		req        MemoryWriteRequest
		wantAction string
		wantTarget string
		wantHash   string
	}{
		{
			name: "ordinary write",
			req:  MemoryWriteRequest{Key: "user.name", Source: "user_message"},
		},
		{
			name:       "replace",
			req:        MemoryWriteRequest{Key: "hermes:user:" + hash, Source: "hermes_memory:replace"},
			wantAction: "replace",
			wantTarget: "user",
			wantHash:   hash,
		},
		{
			name:       "remove",
			req:        MemoryWriteRequest{Key: "hermes:memory:" + hash, Source: "hermes_memory:remove"},
			wantAction: "remove",
			wantTarget: "memory",
			wantHash:   hash,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action, target, priorHash := mirroredMemoryTransition(tc.req)
			if action != tc.wantAction || target != tc.wantTarget || priorHash != tc.wantHash {
				t.Fatalf("mirroredMemoryTransition() = (%q,%q,%q), want (%q,%q,%q)", action, target, priorHash, tc.wantAction, tc.wantTarget, tc.wantHash)
			}
		})
	}
}

func TestClassifyMemoryScope(t *testing.T) {
	cases := map[string]struct {
		memoryType string
		authority  int
	}{
		"governing":  {"governing", 100},
		"procedural": {"procedural", 70},
		"episodic":   {"episodic", 40},
		"user":       {"semantic", 60},
		"":           {"working", 30},
		"unknown":    {"working", 30},
	}
	for scope, want := range cases {
		gotType, gotAuthority := classifyMemoryScope(scope)
		if gotType != want.memoryType || gotAuthority != want.authority {
			t.Fatalf("classifyMemoryScope(%q) = (%s,%d), want (%s,%d)", scope, gotType, gotAuthority, want.memoryType, want.authority)
		}
	}
}

func TestProviderStatusJSON(t *testing.T) {
	status := ProviderStatus{
		Name:        "scos-nexus",
		Version:     "0.1.0",
		IsAvailable: true,
		Healthy:     true,
		Uptime:      "10s",
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProviderStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "scos-nexus" {
		t.Fatal("roundtrip failed")
	}
}

func TestSystemPromptBlock(t *testing.T) {
	srv := newHermesServer()
	r := httptest.NewRequest(http.MethodPost, "/v1/provider/system_prompt", nil)
	w := httptest.NewRecorder()
	srv.handleSystemPrompt(w, r)
	var out map[string]string
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !contains(out["system_prompt_block"], "SCOS Nexus") {
		t.Fatal("prompt should mention SCOS Nexus")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestIsUUID(t *testing.T) {
	if !isUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Fatal("expected valid UUID to be recognised")
	}
	for _, bad := range []string{"", "session-1", "s1", "123e4567e89b12d3a456426614174000"} {
		if isUUID(bad) {
			t.Fatalf("expected %q to be rejected as a UUID", bad)
		}
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/provider/prefetch", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/provider/prefetch", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	identity, status, _ := resolveIdentity(r, "", "")
	if status != 0 {
		t.Fatalf("expected success, got status %d", status)
	}
	if identity.TenantID != "tenant-1" || identity.PrincipalID != "principal-1" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}
