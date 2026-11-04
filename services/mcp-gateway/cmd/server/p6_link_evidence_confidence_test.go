package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestP6LinkEvidenceMCPConfidenceValidationAndZeroForwarding(t *testing.T) {
	const fact = "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	const version = "5cbe32c6-891c-4155-a9ec-6aa39e66704a"
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/evidence-links" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if payload["confidence"] != float64(0) {
			t.Errorf("explicit zero confidence was not forwarded: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"id\":\"6cbe32c6-891c-4155-a9ec-6aa39e66704a\",\"fact_id\":\"" + fact + "\",\"code_symbol_version_id\":\"" + version + "\"}"))
	}))
	defer upstream.Close()

	s := &gatewayServer{memoryIntelligenceURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}

	zeroArgs := map[string]any{"fact_id": fact, "code_symbol_version_id": version, "link_type": "evidences", "confidence": float64(0)}
	if _, err := s.dispatchTool(req.Context(), req, identity, "scos_link_evidence", zeroArgs); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("valid zero confidence reached upstream %d times, want once", calls)
	}

	for _, value := range []any{
		float64(-0.01),
		float64(1.01),
		"0.5",
		math.NaN(),
		math.Inf(1),
	} {
		args := map[string]any{
			"fact_id": fact, "code_symbol_version_id": version,
			"link_type": "evidences", "confidence": value,
		}
		if _, err := s.dispatchTool(req.Context(), req, identity, "scos_link_evidence", args); err == nil {
			t.Fatalf("invalid confidence %#v was forwarded", value)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid confidence values triggered %d extra upstream calls", calls-1)
	}
}
