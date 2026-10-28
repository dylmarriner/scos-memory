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
		{"invalid system time", map[string]any{"as_of_system_time": "yesterday"}, http.StatusBadRequest},
		{"system time missing timezone", map[string]any{"as_of_system_time": "2021-07-01T00:00:00"}, http.StatusBadRequest},
		{"invalid valid time with valid system time", map[string]any{"as_of_system_time": "2021-07-01T00:00:00Z", "as_of_valid_time": "yesterday"}, http.StatusBadRequest},
		{"system time offset hour 24", map[string]any{"as_of_system_time": "2021-07-01T00:00:00+24:00"}, http.StatusBadRequest},
		{"system time offset minute 60", map[string]any{"as_of_system_time": "2021-07-01T00:00:00+00:60"}, http.StatusBadRequest},
		{"valid time offset hour 24", map[string]any{"as_of_valid_time": "2021-07-01T00:00:00+24:00"}, http.StatusBadRequest},
		{"valid time offset minute 60", map[string]any{"as_of_valid_time": "2021-07-01T00:00:00+00:60"}, http.StatusBadRequest},
		{"invalid valid offset paired with system time", map[string]any{"as_of_system_time": "2021-07-01T00:00:00Z", "as_of_valid_time": "2021-07-01T00:00:00+24:00"}, http.StatusBadRequest},
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

func TestP5StrictRFC3339TimestampOffsetRanges(t *testing.T) {
	for _, value := range []string{
		"2021-07-01T00:00:00Z",
		"2021-07-01T00:00:00+13:00",
		"2021-07-01T00:00:00-00:30",
		"2021-07-01T00:00:00+23:59",
		"2021-07-01T00:00:00.123456789-12:45",
	} {
		if _, err := parseStrictRFC3339Timestamp(value); err != nil {
			t.Errorf("valid timestamp %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"2021-07-01T00:00:00+24:00",
		"2021-07-01T00:00:00+00:60",
		"2021-07-01T00:00:00-24:00",
		"2021-07-01T00:00:00-00:60",
		"2021-07-01T00:00:00",
		"yesterday",
	} {
		if _, err := parseStrictRFC3339Timestamp(value); err == nil {
			t.Errorf("malformed timestamp %q accepted", value)
		}
	}
}
