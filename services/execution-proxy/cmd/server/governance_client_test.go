package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingGovernance stands in for tool-governance's HTTP API and records
// what the proxy's client sends it.
type recordingGovernance struct {
	mu       sync.Mutex
	bodies   map[string][]map[string]any
	keys     map[string][]string
	prepared preparedResponse
	refuse   string
}

type preparedResponse struct {
	OperationID string `json:"operation_id"`
	Prepared    bool   `json:"prepared"`
	Authorized  bool   `json:"authorized"`
	RiskLevel   string `json:"risk_level"`
}

func (g *recordingGovernance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.bodies[r.URL.Path] = append(g.bodies[r.URL.Path], body)
	g.keys[r.URL.Path] = append(g.keys[r.URL.Path], r.Header.Get("Idempotency-Key"))
	g.mu.Unlock()
	if r.URL.Path == "/v1/operations/prepare" && g.refuse != "" {
		http.Error(w, g.refuse, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/operations/prepare" {
		_ = json.NewEncoder(w).Encode(g.prepared)
		return
	}
	_, _ = w.Write([]byte(`{}`))
}

func newRecordingGovernance() *recordingGovernance {
	return &recordingGovernance{
		bodies:   map[string][]map[string]any{},
		keys:     map[string][]string{},
		prepared: preparedResponse{OperationID: "5e5e5e5e-0000-4000-8000-0000000000aa", Prepared: true, Authorized: true, RiskLevel: "read"},
	}
}

// TestGovernanceClientSendsPlanRiskAndConfirmation guards the prepare
// contract: the built-in tool name, the plan, its risk and the caller's
// confirmation all reach tool-governance, which decides on them.
func TestGovernanceClientSendsPlanRiskAndConfirmation(t *testing.T) {
	gov := newRecordingGovernance()
	server := httptest.NewServer(gov)
	defer server.Close()
	client := toolGovernanceClient{baseURL: server.URL, client: server.Client()}

	req := ExecuteRequest{TenantID: "tenant", PrincipalID: "principal", OperationID: "5e5e5e5e-0000-4000-8000-0000000000bb", Command: "rm -rf build", Confirmed: true}
	plan := CommandPlan{OriginalRequest: "rm -rf build", ExecutedRequest: "rm -rf build", RiskLevel: "destructive", Mode: "forensic"}
	operation, err := client.AuthorizeAndPrepare(context.Background(), req, plan)
	if err != nil {
		t.Fatal(err)
	}
	if operation.TenantID != "tenant" || operation.PrincipalID != "principal" {
		t.Fatalf("prepared operation does not carry the caller identity: %+v", operation)
	}
	body := gov.bodies["/v1/operations/prepare"][0]
	if body["tool_name"] != executionProxyToolName || body["risk"] != "destructive" || body["confirmed"] != true {
		t.Fatalf("prepare body missing tool/risk/confirmation: %v", body)
	}
	sentPlan, _ := body["plan"].(map[string]any)
	if sentPlan["executed_request"] != "rm -rf build" || sentPlan["risk_level"] != "destructive" {
		t.Fatalf("prepare body missing plan: %v", body["plan"])
	}

	if err := client.Reconcile(context.Background(), operation, "succeeded", &ExecutionResult{OperationID: operation.OperationID}); err != nil {
		t.Fatal(err)
	}
	reconcile := gov.bodies["/v1/operations/reconcile"][0]
	if reconcile["tenant_id"] != "tenant" || reconcile["principal_id"] != "principal" || reconcile["operation_id"] != operation.OperationID {
		t.Fatalf("reconcile must act as the preparing identity: %v", reconcile)
	}
}

// TestGovernanceClientGivesEachUnkeyedRequestItsOwnOperation: two identical
// read-only commands without an operation_id are two operations, so the
// second must not replay the first's (already reconciled) operation.
func TestGovernanceClientGivesEachUnkeyedRequestItsOwnOperation(t *testing.T) {
	gov := newRecordingGovernance()
	server := httptest.NewServer(gov)
	defer server.Close()
	client := toolGovernanceClient{baseURL: server.URL, client: server.Client()}
	req := ExecuteRequest{TenantID: "tenant", PrincipalID: "principal", Command: "ls"}
	plan := CommandPlan{OriginalRequest: "ls", ExecutedRequest: "ls", RiskLevel: "read"}
	for i := 0; i < 2; i++ {
		if _, err := client.AuthorizeAndPrepare(context.Background(), req, plan); err != nil {
			t.Fatal(err)
		}
	}
	keys := gov.keys["/v1/operations/prepare"]
	if len(keys) != 2 || keys[0] == "" || keys[0] == keys[1] {
		t.Fatalf("expected distinct idempotency keys for unkeyed requests, got %v", keys)
	}

	keyed := req
	keyed.OperationID = "5e5e5e5e-0000-4000-8000-0000000000cc"
	for i := 0; i < 2; i++ {
		if _, err := client.AuthorizeAndPrepare(context.Background(), keyed, plan); err != nil {
			t.Fatal(err)
		}
	}
	keys = gov.keys["/v1/operations/prepare"]
	if keys[2] != keys[3] {
		t.Fatalf("expected a retried keyed request to reuse its idempotency key, got %v", keys[2:])
	}
}

func TestGovernanceClientReportsRefusalReason(t *testing.T) {
	gov := newRecordingGovernance()
	gov.refuse = `tool "execution-proxy" is cancelled, not authorised`
	server := httptest.NewServer(gov)
	defer server.Close()
	client := toolGovernanceClient{baseURL: server.URL, client: server.Client()}
	_, err := client.AuthorizeAndPrepare(context.Background(), ExecuteRequest{TenantID: "t", PrincipalID: "p", Command: "ls"}, CommandPlan{RiskLevel: "read"})
	if err == nil || !strings.Contains(err.Error(), "not authorised") {
		t.Fatalf("expected the governance refusal reason in the error, got %v", err)
	}
}
