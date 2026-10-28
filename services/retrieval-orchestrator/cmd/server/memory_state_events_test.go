package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestP5MemoryEventReadbackRejectsMalformedSelectorsBeforeDatabase(t *testing.T) {
	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{}), auth.Config{Token: "test-token", Identity: "internal-service"})
	id := "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		name, method, query string
		status              int
	}{
		{"missing memory", "GET", "", 400},
		{"bad UUID", "GET", "?memory_id=other", 400},
		{"duplicate ID", "GET", "?memory_id=" + id + "&memory_id=" + id, 400},
		{"unexpected parameter", "GET", "?memory_id=" + id + "&as_of_system_time=2020", 400},
		{"zero limit", "GET", "?memory_id=" + id + "&limit=0", 400},
		{"oversized limit", "GET", "?memory_id=" + id + "&limit=101", 400},
		{"negative cursor", "GET", "?memory_id=" + id + "&after_id=-1", 400},
		{"invalid cursor", "GET", "?memory_id=" + id + "&after_id=wat", 400},
		{"malformed percent encoding", "GET", "?memory_id=" + id + "&limit=%zz", 400},
		{"overflow cursor", "GET", "?memory_id=" + id + "&after_id=9999999999999999999999999", 400},
		{"wrong method", "POST", "?memory_id=" + id, 405},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/memory-state-events"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("X-SCOS-Tenant-ID", id)
			req.Header.Set("X-SCOS-Principal-ID", id)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("HTTP %d want %d, body %s", rec.Code, tc.status, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "panic") {
				t.Fatalf("internal detail exposed: %s", rec.Body.String())
			}
		})
	}
	req := httptest.NewRequest("GET", fmt.Sprintf("/v1/memory-state-events?memory_id=%s", id), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HTTP %d", rec.Code)
	}
}

func decodeMemoryEvents(t *testing.T, rec *httptest.ResponseRecorder) memoryStateEventsResponse {
	t.Helper()
	var response memoryStateEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
