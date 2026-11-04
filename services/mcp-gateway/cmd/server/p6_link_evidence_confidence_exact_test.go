package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDecodeToolArgumentsPreservesExactConfidenceNumber(t *testing.T) {
	args, err := decodeToolArguments(json.RawMessage(`{"confidence":1.0000000000000001,"limit":7}`))
	if err != nil {
		t.Fatal(err)
	}
	confidence, ok := args["confidence"].(json.Number)
	if !ok || confidence.String() != "1.0000000000000001" {
		t.Fatalf("confidence = %#v (%T), want exact json.Number", args["confidence"], args["confidence"])
	}
	if limit, ok := args["limit"].(float64); !ok || limit != 7 {
		t.Fatalf("unrelated numeric argument = %#v (%T), want float64(7)", args["limit"], args["limit"])
	}
}

func TestP6LinkEvidenceGatewayRejectsExactOutOfRangeAndInvalidTypes(t *testing.T) {
	s := &gatewayServer{}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	for _, value := range []any{
		json.Number("1.0000000000000001"),
		json.Number("-0.0000000000000001"),
		json.Number("1.01"),
		json.Number("-0.01"),
		"0.5",
		nil,
		true,
		[]any{json.Number("0.5")},
		math.NaN(),
		math.Inf(1),
	} {
		args := map[string]any{
			"fact_id":                "4cbe32c6-891c-4155-a9ec-6aa39e66704a",
			"code_symbol_version_id": "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
			"link_type":              "evidences",
			"confidence":             value,
		}
		if _, err := s.dispatchTool(req.Context(), req, identity, "scos_link_evidence", args); err == nil {
			t.Fatalf("invalid confidence %#v (%T) was accepted", value, value)
		}
	}
}

func TestP6LinkEvidenceGatewayForwardsExactDecimalAndOmitsMissingConfidence(t *testing.T) {
	const fact = "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	const version = "5cbe32c6-891c-4155-a9ec-6aa39e66704a"
	var received []map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		received = append(received, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"6cbe32c6-891c-4155-a9ec-6aa39e66704a"}`))
	}))
	defer upstream.Close()

	s := &gatewayServer{memoryIntelligenceURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	base := map[string]any{
		"fact_id":                fact,
		"code_symbol_version_id": version,
		"link_type":              "evidences",
	}
	withConfidence := map[string]any{}
	for key, value := range base {
		withConfidence[key] = value
	}
	withConfidence["confidence"] = json.Number("0.2500")
	if _, err := s.dispatchTool(req.Context(), req, identity, "scos_link_evidence", withConfidence); err != nil {
		t.Fatalf("forward exact decimal: %v", err)
	}
	if _, err := s.dispatchTool(req.Context(), req, identity, "scos_link_evidence", base); err != nil {
		t.Fatalf("forward omitted confidence: %v", err)
	}
	if len(received) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(received))
	}
	if got := string(received[0]["confidence"]); got != "0.2500" {
		t.Fatalf("forwarded confidence token = %q, want original decimal 0.2500", got)
	}
	if _, exists := received[1]["confidence"]; exists {
		t.Fatalf("omitted confidence was forwarded: %s", received[1]["confidence"])
	}
}

func TestSCOSLinkEvidenceAdvertisesConfidenceBounds(t *testing.T) {
	for _, tool := range governedTools() {
		if tool["name"] != "scos_link_evidence" {
			continue
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("inputSchema has type %T, want map[string]any", tool["inputSchema"])
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("inputSchema.properties has type %T, want map[string]any", schema["properties"])
		}
		confidence, ok := properties["confidence"].(map[string]any)
		if !ok {
			t.Fatalf("confidence schema has type %T, want map[string]any", properties["confidence"])
		}
		if confidence["minimum"] != 0 || confidence["maximum"] != 1 {
			t.Fatalf("confidence bounds = [%v, %v], want [0, 1]", confidence["minimum"], confidence["maximum"])
		}
		return
	}
	t.Fatal("scos_link_evidence missing from governed tools")
}
