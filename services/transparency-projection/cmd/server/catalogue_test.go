package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSnapshotCarriesTheGatewayToolCatalogue covers the tool catalogue the
// dashboard groups into category tabs: it is mcp-gateway's own tools/list,
// asked for with a JSON-RPC POST, and a gateway that fails is reported as
// unavailable rather than shown as an empty catalogue.
func TestSnapshotCarriesTheGatewayToolCatalogue(t *testing.T) {
	var method string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mcp" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		method = req.Method
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"scos_recall","description":"Search memory","inputSchema":{"type":"object"}}]}}`))
	}))
	defer gateway.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	s := &projectionServer{upstream: upstream.URL, upstreams: map[string]string{"mcp_gateway": gateway.URL}, client: upstream.Client()}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	var snap struct {
		Views       map[string]json.RawMessage `json:"views"`
		Unavailable map[string]string          `json:"unavailable"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if method != "tools/list" {
		t.Fatalf("gateway was asked for %q, want tools/list", method)
	}
	if !strings.Contains(string(snap.Views["mcp_tools"]), `"scos_recall"`) {
		t.Fatalf("mcp_tools = %s, unavailable = %v", snap.Views["mcp_tools"], snap.Unavailable["mcp_tools"])
	}

	gateway.Close()
	res = httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	snap.Views, snap.Unavailable = nil, nil
	if err := json.Unmarshal(res.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Views["mcp_tools"]; ok || snap.Unavailable["mcp_tools"] == "" {
		t.Fatalf("a failed gateway must be reported unavailable, got views=%s unavailable=%q", snap.Views["mcp_tools"], snap.Unavailable["mcp_tools"])
	}
}
