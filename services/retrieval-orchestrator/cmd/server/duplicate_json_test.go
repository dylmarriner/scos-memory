package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Conflicting duplicate selectors must fail before identity resolution.
// encoding/json otherwise silently keeps the last field.
func TestP7RetrieveRejectsDuplicateJSONSelectors(t *testing.T) {
	cases := []string{
		`{"tenant_id":"first","tenant_id":"second","principal_id":"p","query":"q","as_of_system_time":"2021-07-01T00:00:00Z"}`,
		`{"tenant_id":"t","principal_id":"p","query":"q","query":"different","as_of_system_time":"2021-07-01T00:00:00Z"}`,
		`{"tenant_id":"t","principal_id":"p","query":"q","as_of_system_time":"2021-07-01T00:00:00Z","as_of_system_time":"2022-01-01T00:00:00Z"}`,
		`{"tenant_id":"t","principal_id":"p","query":"q","filters":{"x":"1","x":"2"},"as_of_system_time":"2021-07-01T00:00:00Z"}`,
	}
	for _, body := range cases {
		rec := httptest.NewRecorder()
		(&server{}).handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("duplicate JSON accepted: HTTP %d for %s", rec.Code, body)
		}
	}
}
