package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewCompatServer(t *testing.T) {
	srv := newCompatServer()
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
	if len(srv.adapters) != 10 {
		t.Fatalf("expected 10 default adapters, got %d", len(srv.adapters))
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	// A server with no DATABASE_URL/postgres dependency configured must
	// report itself not-ready, not a hardcoded pass.
	srv := newCompatServer()
	if err := srv.readinessCheck(); err == nil {
		t.Fatal("expected readiness check to fail without a configured postgres dependency")
	}
}

func TestDefaultAdapters(t *testing.T) {
	srv := newCompatServer()

	native, ok := srv.adapters["native_sdk"]
	if !ok {
		t.Fatal("expected native_sdk adapter")
	}
	if native.Tier != "A" {
		t.Fatalf("expected Tier A for native_sdk, got %s", native.Tier)
	}

	watcher, ok := srv.adapters["session_watcher"]
	if !ok {
		t.Fatal("expected session_watcher adapter")
	}
	if watcher.Tier != "D" {
		t.Fatalf("expected Tier D for session_watcher, got %s", watcher.Tier)
	}
}

// TestAgentAdapterContractCoverage enforces governance ss23.4: every adapter
// must classify each required operation as supported or explicitly
// unsupported, with no gaps and no double-classification.
func TestAgentAdapterContractCoverage(t *testing.T) {
	srv := newCompatServer()
	for boundary, a := range srv.adapters {
		seen := make(map[string]string, len(requiredAdapterOperations))
		for _, op := range a.Supported {
			seen[op] = "supported"
		}
		for _, op := range a.Unsupported {
			if prev, ok := seen[op]; ok {
				t.Fatalf("%s: operation %q classified as both %s and unsupported", boundary, op, prev)
			}
			seen[op] = "unsupported"
		}
		for _, op := range requiredAdapterOperations {
			if _, ok := seen[op]; !ok {
				t.Errorf("%s: operation %q neither supported nor explicitly unsupported", boundary, op)
			}
		}
	}
}

// TestSupportedIntegrationBoundariesComplete enforces governance ss23.2's
// full boundary table is registered, not just a subset.
func TestSupportedIntegrationBoundariesComplete(t *testing.T) {
	srv := newCompatServer()
	required := []string{
		"native_sdk", "http_sidecar", "mcp_server", "model_api_proxy",
		"cli_wrapper", "provider_plugin", "lifecycle_hooks", "event_bridge",
		"session_watcher", "a2a_adapter",
	}
	for _, boundary := range required {
		if _, ok := srv.adapters[boundary]; !ok {
			t.Errorf("missing required integration boundary %q", boundary)
		}
	}
}

// TestTestedComesOnlyFromConformance guards ss23.7: a boundary is tested
// only when the in-process conformance run passed every check for it, and a
// boundary the gateway does not implement is unsupported, never tested.
func TestTestedComesOnlyFromConformance(t *testing.T) {
	srv := newCompatServer()
	for boundary, a := range srv.adapters {
		if _, implemented := boundaryProfiles[boundary]; !implemented {
			if a.Support != supportUnsupported || a.Tested || a.Conformance != nil {
				t.Errorf("%s has no implementation but reports support=%s tested=%v", boundary, a.Support, a.Tested)
			}
			continue
		}
		if a.Conformance == nil || len(a.Conformance.Checks) == 0 {
			t.Fatalf("%s has no conformance run", boundary)
		}
		for _, check := range a.Conformance.Checks {
			if !check.Passed {
				t.Errorf("%s: conformance check %q failed: %s", boundary, check.Name, check.Detail)
			}
		}
		if a.Tested != a.Conformance.Passed {
			t.Errorf("%s: tested=%v but conformance passed=%v", boundary, a.Tested, a.Conformance.Passed)
		}
	}
}

// TestFailingConformanceMarksBoundaryUntested proves the flag follows the
// run: a profile whose checks fail leaves its boundary untested.
func TestFailingConformanceMarksBoundaryUntested(t *testing.T) {
	broken := boundaryProfile{eventFormats: []string{"not_a_format"}, contextFormats: []string{"yaml"}}
	eventFixtures["not_a_format"] = []eventFixture{{name: "message", operation: "capture_ingress", native: `{}`}}
	defer delete(eventFixtures, "not_a_format")
	result := runBoundaryConformance(broken, time.Now())
	if result.Passed || len(result.Checks) == 0 {
		t.Fatalf("expected a failing run, got %+v", result)
	}
	for _, c := range result.Checks {
		if c.Passed || c.Detail == "" {
			t.Fatalf("expected every check to fail with detail, got %+v", c)
		}
	}
	srv := newCompatServer()
	srv.applyConformance(map[string]*AdapterConformance{"native_sdk": result})
	if srv.adapters["native_sdk"].Tested {
		t.Fatal("a failing conformance run must leave the boundary untested")
	}
	if srv.adapters["http_sidecar"].Tested {
		t.Fatal("a boundary with no conformance result must not be tested")
	}
}

func TestSessionWatcherIsUnsupported(t *testing.T) {
	srv := newCompatServer()
	a := srv.adapters["session_watcher"]
	if a.Support != supportUnsupported || a.Tested || len(a.Supported) != 0 {
		t.Fatalf("session_watcher should be unsupported with no supported operations, got %+v", a)
	}
	if got := srv.adapterHealthStatus(context.Background(), "tenant", a); got != "unsupported" {
		t.Fatalf("session_watcher health = %s", got)
	}
	body := strings.NewReader(`{"agent_id":"watcher-1","boundary":"session_watcher"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/agents", body)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	w := httptest.NewRecorder()
	srv.handleAgents(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("registering against an unimplemented boundary = %d", w.Code)
	}
}

func TestConformanceEndpointReportsRun(t *testing.T) {
	srv := newCompatServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/compatibility/conformance?boundary=mcp_server", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	w := httptest.NewRecorder()
	srv.handleConformance(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("conformance = %d %s", w.Code, w.Body.String())
	}
	var out []struct {
		Boundary    string              `json:"boundary"`
		Tested      bool                `json:"tested"`
		Conformance *AdapterConformance `json:"conformance"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Boundary != "mcp_server" || !out[0].Tested || out[0].Conformance == nil {
		t.Fatalf("unexpected conformance report %+v", out)
	}
	ops := strings.Join(out[0].Conformance.VerifiedOperations, ",")
	if ops != "inject_context,observe_tool_call,observe_tool_result" {
		t.Fatalf("verified operations = %s", ops)
	}
}

func TestCompatibilityTiersCoverage(t *testing.T) {
	srv := newCompatServer()
	tiers := map[string]bool{"A": false, "B": false, "C": false, "D": false}
	for _, a := range srv.adapters {
		tiers[a.Tier] = true
	}
	for tier, found := range tiers {
		if !found {
			t.Errorf("missing adapter for Tier %s", tier)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-1",
		SessionID: "session-1",
		Facts:     []string{"Fact 1", "Fact 2"},
		TaskState: "In progress",
		Sources:   []string{"memory::123"},
	}
	rendered := renderMarkdown(&env)
	if !strings.Contains(rendered, "## Facts") {
		t.Fatal("markdown should contain Facts section")
	}
	if !strings.Contains(rendered, "Fact 1") {
		t.Fatal("markdown should contain Fact 1")
	}
	if !strings.Contains(rendered, "memory::123") {
		t.Fatal("markdown should contain source reference")
	}
}

func TestRenderText(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-1",
		SessionID: "session-1",
		Facts:     []string{"Important fact"},
	}
	rendered := renderText(&env)
	if !strings.Contains(rendered, "FACTS:") {
		t.Fatal("text should contain FACTS section")
	}
	if !strings.Contains(rendered, "Important fact") {
		t.Fatal("text should contain the fact")
	}
}

// TestRenderPreservesAllEnvelopeSections enforces governance ss23.5: the
// envelope must render into the target agent's supported form without
// dropping any of its declared sections.
func TestRenderPreservesAllEnvelopeSections(t *testing.T) {
	env := ContextEnvelope{
		AgentID:         "agent-1",
		SessionID:       "session-1",
		EpisodicHistory: []string{"turn-1 summary"},
		CodeRefs:        []string{"pkg/foo.go:42"},
		Artifacts:       []string{"artifact::report"},
		RestorationRefs: []string{"checkpoint::abc"},
	}
	md := renderMarkdown(&env)
	for _, want := range []string{"turn-1 summary", "pkg/foo.go:42", "artifact::report", "checkpoint::abc"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown rendering dropped section content %q", want)
		}
	}
	text := renderText(&env)
	if !strings.Contains(text, "turn-1 summary") {
		t.Error("text rendering dropped episodic history")
	}
}

func TestRenderJSON(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-1",
		SessionID: "session-1",
	}
	rendered := renderJSON(&env)
	if !strings.Contains(rendered, `"agent_id"`) {
		t.Fatal("JSON should contain agent_id")
	}
}

func TestRenderEmptyEnvelope(t *testing.T) {
	env := ContextEnvelope{AgentID: "a", SessionID: "s"}
	rendered := renderText(&env)
	if !strings.Contains(rendered, "Context for Agent a") {
		t.Fatal("should render basic info")
	}
}

func TestJSONRoundTripEnvelope(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-test",
		SessionID: "session-1",
		Facts:     []string{"fact 1"},
		Format:    "json",
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ContextEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AgentID != "agent-test" {
		t.Fatal("roundtrip failed")
	}
}

func translateCall(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	srv := newCompatServer()
	r := httptest.NewRequest(http.MethodPost, "/v1/translate", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleTranslate(w, r)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestTranslateProducesRequestedTargetFormat(t *testing.T) {
	code, out := translateCall(t, `{"source_format":"openai","target_format":"anthropic","source_data":{"role":"assistant","content":"Looking.","tool_calls":[{"id":"c1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}}`)
	if code != http.StatusOK || out["target_format"] != "anthropic" || out["event_type"] != "tool_call" {
		t.Fatalf("translate = %d %+v", code, out)
	}
	msg := out["translated"].(map[string]any)
	blocks := msg["content"].([]any)
	if msg["role"] != "assistant" || len(blocks) != 2 {
		t.Fatalf("anthropic message = %+v", msg)
	}
	use := blocks[1].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "c1" || use["name"] != "search" || use["input"].(map[string]any)["q"] != "x" {
		t.Fatalf("tool_use block = %+v", use)
	}

	code, out = translateCall(t, `{"source_format":"anthropic","target_format":"mcp","source_data":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"done","is_error":true}]}}`)
	if code != http.StatusOK {
		t.Fatalf("anthropic->mcp = %d %+v", code, out)
	}
	frame := out["translated"].(map[string]any)
	result := frame["result"].(map[string]any)
	if frame["jsonrpc"] != "2.0" || frame["id"] != "c1" || result["isError"] != true {
		t.Fatalf("mcp frame = %+v", frame)
	}

	code, out = translateCall(t, `{"source_format":"mcp","source_data":{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"path":"a"}}}}`)
	common := out["translated"].(map[string]any)
	if code != http.StatusOK || out["target_format"] != "scos_common" || common["event_type"] != "tool_call" || common["role"] != "assistant" {
		t.Fatalf("mcp->scos_common = %d %+v", code, out)
	}
}

func TestTranslateRejectsUnsupportedFormats(t *testing.T) {
	for _, body := range []string{
		`{"source_format":"openai","target_format":"yaml","source_data":{"role":"user","content":"hi"}}`,
		`{"source_format":"custom","target_format":"scos_common","source_data":{"role":"user","content":"hi"}}`,
		`{"target_format":"openai","source_data":{"role":"user","content":"hi"}}`,
	} {
		if code, _ := translateCall(t, body); code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, code)
		}
	}
}

func TestTranslateRefusesUnrepresentableEvents(t *testing.T) {
	cases := []string{
		`{"source_format":"openai","target_format":"mcp","source_data":{"role":"user","content":"hi"}}`,
		`{"source_format":"openai","target_format":"anthropic","source_data":{"role":"system","content":"be terse"}}`,
		`{"source_format":"anthropic","target_format":"openai","source_data":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"x","is_error":true}]}}`,
	}
	for _, body := range cases {
		if code, out := translateCall(t, body); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d %+v, want 422", body, code, out)
		}
	}
}

func TestTranslateRejectsMismatchedEventTypeAndMalformedSource(t *testing.T) {
	if code, _ := translateCall(t, `{"event_type":"tool_call","source_format":"openai","source_data":{"role":"user","content":"hi"}}`); code != http.StatusBadRequest {
		t.Fatalf("mismatched event_type = %d", code)
	}
	if code, _ := translateCall(t, `{"source_format":"openai","source_data":{"role":"wizard","content":"hi"}}`); code != http.StatusBadRequest {
		t.Fatalf("unknown role = %d", code)
	}
	if code, _ := translateCall(t, `{"source_format":"mcp","source_data":{"jsonrpc":"2.0","id":1,"method":"resources/list"}}`); code != http.StatusBadRequest {
		t.Fatalf("non-tools/call MCP frame = %d", code)
	}
}

func TestRenderEnvelopeReportsProducedFormat(t *testing.T) {
	env := ContextEnvelope{AgentID: "a", SessionID: "s", Facts: []string{"f"}}
	for _, format := range renderFormats {
		produced, content, err := renderEnvelope(&env, format)
		if err != nil || produced != format || !strings.Contains(content, "f") {
			t.Fatalf("render %s = %s %q %v", format, produced, content, err)
		}
	}
	if _, _, err := renderEnvelope(&env, "yaml"); err == nil {
		t.Fatal("unsupported render format must be refused, not rendered as JSON")
	}
}

func TestRenderTextIncludesUserModel(t *testing.T) {
	env := ContextEnvelope{AgentID: "a", SessionID: "s", UserModel: "prefers terse answers"}
	if !strings.Contains(renderText(&env), "prefers terse answers") {
		t.Fatal("text rendering dropped the user model")
	}
}

func TestEnvelopeRejectsUnknownFormat(t *testing.T) {
	srv := newCompatServer()
	r := httptest.NewRequest(http.MethodPost, "/v1/envelope", strings.NewReader(`{"agent_id":"a","session_id":"s","format":"native"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	w := httptest.NewRecorder()
	srv.handleEnvelope(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown envelope format = %d", w.Code)
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/agents", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/agents", nil)
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

func TestAgentByIDMissingRequiresIdentity(t *testing.T) {
	srv := newCompatServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/agents/unknown", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal identity, got %d", w.Code)
	}
}

// An active agent is reported stale once it has not been seen within the
// health window; inactive is reported as stored.
func TestAgentStatusDerivesFromLastSeen(t *testing.T) {
	cases := []struct {
		stored string
		seen   sql.NullTime
		want   string
	}{
		{agentActive, sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}, agentActive},
		{agentActive, sql.NullTime{Time: time.Now().Add(-adapterHealthWindow - time.Minute), Valid: true}, agentStale},
		{agentActive, sql.NullTime{}, agentStale},
		{agentInactive, sql.NullTime{Time: time.Now(), Valid: true}, agentInactive},
	}
	for _, c := range cases {
		reg, err := scanAgentRegistration(agentInstanceRow{InstanceKey: "a", Boundary: "mcp", Status: c.stored, LastSeen: c.seen})
		if err != nil {
			t.Fatal(err)
		}
		if reg.Status != c.want {
			t.Errorf("stored %s seen %v: got %s, want %s", c.stored, c.seen, reg.Status, c.want)
		}
	}
}
