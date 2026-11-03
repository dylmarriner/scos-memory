package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceparentIntegrity(t *testing.T) {
	const valid = "00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-01"
	cases := []struct{ input, trace, span string }{
		{valid, "4bf92f3577b34da6a3ce929d0e0e4730", "00f067aa0ba902b7"},
		{"00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", "", ""},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", "", ""},
		{"00-4bf92f3577b34da6a3ce929d0e0e4730-0000000000000000-01", "", ""},
		{"ff-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-01", "", ""},
		{"00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-gg", "", ""},
		{"00-4BF92F3577B34DA6A3CE929D0E0E4730-00f067aa0ba902b7-01", "", ""},
		{"00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-01-extra", "", ""},
		{"00-4bf92f3577b34da6a3ce929d0e0e4730x00f067aa0ba902b7-01", "", ""},
		{"00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7x01", "", ""},
	}
	for _, tc := range cases {
		got := TraceCorrelation(tc.input)
		if got.TraceID != tc.trace || got.SpanID != tc.span {
			t.Errorf("TraceCorrelation(%q) = %+v, want trace=%q span=%q", tc.input, got, tc.trace, tc.span)
		}
	}
}

// HTTP ingress telemetry must use the same validated trace/span as hook
// telemetry, while retaining its tenant context on malformed headers.
func TestRequestCorrelationPreservesTenantWithValidatedTrace(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-01")
	valid := requestCorrelation(req)
	if valid.Tenant != "tenant-a" || valid.TraceID != "4bf92f3577b34da6a3ce929d0e0e4730" || valid.SpanID != "00f067aa0ba902b7" {
		t.Fatalf("valid ingress correlation lost: %+v", valid)
	}
	req.Header.Set("traceparent", "00-00000000000000000000000000000000-00f067aa0ba902b7-01")
	invalid := requestCorrelation(req)
	if invalid.Tenant != "tenant-a" || invalid.TraceID != "" || invalid.SpanID != "" {
		t.Fatalf("invalid ingress trace accepted or tenant lost: %+v", invalid)
	}
}
