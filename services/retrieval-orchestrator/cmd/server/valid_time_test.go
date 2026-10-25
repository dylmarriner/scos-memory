package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestP5TemporalRequestValidationFailsClosed(t *testing.T) {
	srv := &server{}
	for _, tc := range []struct {
		name     string
		payload  map[string]any
		expected int
	}{
		{"invalid timestamp", map[string]any{"as_of_valid_time": "yesterday"}, http.StatusBadRequest},
		{"timestamp missing timezone", map[string]any{"as_of_valid_time": "2021-07-01T00:00:00"}, http.StatusBadRequest},
		{"system time unavailable", map[string]any{"as_of_system_time": "2021-07-01T00:00:00Z"}, http.StatusNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.payload["tenant_id"] = "not-used"
			tc.payload["principal_id"] = "not-used"
			tc.payload["query"] = "temporal"
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(raw))
			recorder := httptest.NewRecorder()
			srv.handleRetrieve(recorder, req)
			if recorder.Code != tc.expected {
				t.Fatalf("status %d want %d: %s", recorder.Code, tc.expected, recorder.Body.String())
			}
		})
	}
}

func TestP5ValidTimeRecordedInRetrievalAudit(t *testing.T) {
	req := RetrieveRequest{AsOfValidTime: "2021-07-01T00:00:00Z"}
	evidence := appliedRestrictions(req)
	if evidence["as_of_valid_time"] != req.AsOfValidTime ||
		evidence["temporal_scope"] != "current_system_belief_valid_at" {
		t.Fatalf("valid time missing from audited retrieval filters: %+v", evidence)
	}
	if _, ok := appliedRestrictions(RetrieveRequest{})["as_of_valid_time"]; ok {
		t.Fatal("ordinary retrieval falsely records a historical selector")
	}
}
