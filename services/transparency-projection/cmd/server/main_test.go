package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudAPIProxiesSessionAndResponse(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" || r.URL.RawQuery != "view=full" {
			t.Errorf("request URL = %s, want /api/me?view=full", r.URL.RequestURI())
		}
		if got := r.Header.Get("Cookie"); got != "scos_session=session-token" {
			t.Errorf("Cookie = %q, want session cookie", got)
		}
		w.Header().Set("Set-Cookie", "scos_session=refreshed; Path=/; HttpOnly")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"user-1"}`)
	}))
	defer cloud.Close()

	s := &projectionServer{cloudControlPlaneURL: cloud.URL, client: cloud.Client()}
	req := httptest.NewRequest(http.MethodGet, "/api/me?view=full", nil)
	req.Header.Set("Cookie", "scos_session=session-token")
	res := httptest.NewRecorder()
	s.cloudAPI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Set-Cookie"); got == "" {
		t.Fatal("expected proxied Set-Cookie header")
	}
	if got := res.Body.String(); got != `{"id":"user-1"}` {
		t.Fatalf("body = %q, want cloud response", got)
	}
}

func TestCloudAPIFailsClosedWhenUnconfigured(t *testing.T) {
	s := &projectionServer{}
	res := httptest.NewRecorder()
	s.cloudAPI(res, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.Code)
	}
}

func TestSnapshotUsesAuthoritativeUpstreamViews(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"projection","entries":[]}`))
	}))
	defer upstream.Close()

	s := &projectionServer{upstream: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil)
	res := httptest.NewRecorder()
	s.snapshot(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON response, got %q", got)
	}
}

func TestSnapshotDoesNotExposeFabricatedSystemTelemetry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/dashboard/system" {
			_, _ = w.Write([]byte(`{"type":"system_map","entries":[{"service":"memory-gateway","status":"unknown","version":"unknown","uptime":"unavailable"}],"total_count":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"projection","entries":[]}`))
	}))
	defer upstream.Close()

	s := &projectionServer{upstream: upstream.URL, client: upstream.Client()}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	var snapshot map[string]any
	if err := json.NewDecoder(res.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	payload, _ := json.Marshal(snapshot["views"])
	if strings.Contains(string(payload), `"status":"healthy"`) || strings.Contains(string(payload), `"uptime":"12m"`) {
		t.Fatalf("snapshot exposed fabricated system telemetry: %s", payload)
	}
}

func TestReadyFailsWhenUpstreamUnavailable(t *testing.T) {
	s := &projectionServer{upstream: "http://127.0.0.1:1", client: &http.Client{}}
	res := httptest.NewRecorder()
	s.ready(res, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.Code)
	}
}

func TestSnapshotIncludesAgentAndToolOperationViews(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"projection","entries":[]}`))
	}))
	defer upstream.Close()
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"agent-1"}]`))
	}))
	defer agents.Close()
	tools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"op-1"}]`))
	}))
	defer tools.Close()

	s := &projectionServer{upstream: upstream.URL, upstreams: map[string]string{"agent_registry": agents.URL, "tool_governance": tools.URL}, client: upstream.Client()}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	var decoded struct {
		Views map[string]json.RawMessage `json:"views"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := decoded.Views["agents"]; !ok {
		t.Fatal("expected an agents view")
	}
	if _, ok := decoded.Views["tool_operations"]; !ok {
		t.Fatal("expected a tool_operations view")
	}
}

func TestSnapshotIncludesGovernanceExpansionViews(t *testing.T) {
	respond := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	upstream := httptest.NewServer(respond(`{"type":"projection","entries":[]}`))
	defer upstream.Close()
	extensionManager := httptest.NewServer(respond(`[]`))
	defer extensionManager.Close()
	codeIntelligence := httptest.NewServer(respond(`{"status":"idle"}`))
	defer codeIntelligence.Close()
	controlPlane := httptest.NewServer(respond(`[]`))
	defer controlPlane.Close()
	recovery := httptest.NewServer(respond(`{"status":"idle"}`))
	defer recovery.Close()
	securityGate := httptest.NewServer(respond(`[]`))
	defer securityGate.Close()
	observability := httptest.NewServer(respond(`{"metrics":[]}`))
	defer observability.Close()

	s := &projectionServer{
		upstream: upstream.URL,
		upstreams: map[string]string{
			"extension_manager":        extensionManager.URL,
			"code_intelligence":        codeIntelligence.URL,
			"autonomous_control_plane": controlPlane.URL,
			"recovery_orchestrator":    recovery.URL,
			"security_gate":            securityGate.URL,
			"observability":            observability.URL,
		},
		client: upstream.Client(),
	}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	var decoded struct {
		Views map[string]json.RawMessage `json:"views"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, key := range []string{"plugins", "skills", "hooks", "code_intelligence", "routing", "recovery", "security", "token_flow", "costs", "service_flows"} {
		if _, ok := decoded.Views[key]; !ok {
			t.Errorf("expected a %q view", key)
		}
	}
}

func TestSnapshotToleratesPartialUpstreamUnavailability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"projection","entries":[]}`))
	}))
	defer upstream.Close()

	// agentRegistry and toolGovernance are left unset (unavailable).
	s := &projectionServer{upstream: upstream.URL, client: upstream.Client()}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 even with partial upstream unavailability, got %d: %s", res.Code, res.Body.String())
	}
	var decoded struct {
		Views       map[string]json.RawMessage `json:"views"`
		Unavailable map[string]string          `json:"unavailable"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Unavailable) == 0 {
		t.Fatal("expected agents/tool_operations to be reported unavailable, not silently dropped")
	}
	if _, ok := decoded.Views["system"]; !ok {
		t.Fatal("expected the working memory-intelligence views to still be present")
	}
}

func TestEventsResumesFromLastEventID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Last-Event-ID"); got != "41" {
			t.Errorf("Last-Event-ID = %q, want 41", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("id: 42\nevent: memory.updated\ndata: {\"sequence\":42}\n\n"))
	}))
	defer upstream.Close()

	s := &projectionServer{upstream: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/v1/transparency/stream", nil)
	req.Header.Set("Last-Event-ID", "41")
	res := httptest.NewRecorder()
	s.events(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "id: 42") {
		t.Fatalf("expected resumed event in response: %s", res.Body.String())
	}
}

func TestAccountsDisabledDeploymentAnswersPlainly(t *testing.T) {
	s := &projectionServer{accountsDisabled: true}
	res := httptest.NewRecorder()
	s.deploymentInfo(res, httptest.NewRequest(http.MethodGet, "/api/deployment", nil))
	if res.Code != http.StatusOK || strings.TrimSpace(res.Body.String()) != `{"accounts_enabled":false}` {
		t.Fatalf("deployment info = %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	s.cloudAPI(res, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if res.Code != http.StatusNotFound || !strings.Contains(res.Body.String(), "accounts are not enabled") {
		t.Fatalf("account API on an accounts-disabled deployment = %d %s", res.Code, res.Body.String())
	}

	enabled := &projectionServer{cloudControlPlaneURL: "https://cloud-control-plane:8079"}
	res = httptest.NewRecorder()
	enabled.deploymentInfo(res, httptest.NewRequest(http.MethodGet, "/api/deployment", nil))
	if strings.TrimSpace(res.Body.String()) != `{"accounts_enabled":true}` {
		t.Fatalf("deployment info with accounts = %s", res.Body.String())
	}
}

func TestAnEmptyListViewIsAnEmptyList(t *testing.T) {
	if got := string(normaliseListView("agents", json.RawMessage(" null\n"))); got != "[]" {
		t.Fatalf("agents null = %s, want []", got)
	}
	if got := string(normaliseListView("agents", json.RawMessage(`[{"id":"a"}]`))); got != `[{"id":"a"}]` {
		t.Fatalf("a populated list changed: %s", got)
	}
	// An object view keeps its null: only list views are normalised.
	if got := string(normaliseListView("user_model", json.RawMessage("null"))); got != "null" {
		t.Fatalf("object view null = %s", got)
	}
}
