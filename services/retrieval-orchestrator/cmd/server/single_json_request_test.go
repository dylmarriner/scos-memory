package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// A second JSON value cannot be silently discarded after the authenticated
// tenant and temporal selectors have already been parsed.
func TestP5RetrieveRejectsTrailingJSONValues(t *testing.T) {
	base := `{"tenant_id":"tenant","principal_id":"principal","query":"temporal","as_of_system_time":"2021-07-01T00:00:00Z"}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"single document reaches explicit historical gate", base, http.StatusNotImplemented},
		{"trailing whitespace allowed", base + " \n\t", http.StatusNotImplemented},
		{"second object rejected", base + ` {"tenant_id":"other"}`, http.StatusBadRequest},
		{"second scalar rejected", base + ` null`, http.StatusBadRequest},
		{"malformed trailing bytes rejected", base + ` garbage`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			(&server{}).handleRetrieve(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Exercise the real authenticated mux so the single-document invariant is
// enforced on the service-token path as well as direct handler calls.
func TestP5AuthenticatedRetrieveRejectsTrailingJSON(t *testing.T) {
	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{}), auth.Config{
		Token: "shared-secret", Identity: "internal-service",
	})
	base := `{"tenant_id":"real-tenant","principal_id":"real-principal","query":"temporal","as_of_system_time":"2021-07-01T00:00:00Z"}`
	for _, tc := range []struct {
		body string
		want int
	}{
		{base, http.StatusNotImplemented},
		{base + ` {"tenant_id":"foreign-tenant"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer shared-secret")
		req.Header.Set("X-SCOS-Tenant-ID", "real-tenant")
		req.Header.Set("X-SCOS-Principal-ID", "real-principal")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
		}
	}
}
