package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallServiceDoesNotExposeUntrustedUpstreamErrorBody(t *testing.T) {
	const secret = "postgres://db-admin:secret-password@private-db:5432/tenant-data"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal database failure: "+secret, http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)

	plane := &controlPlane{}
	_, err := plane.callService(context.Background(), "extension-manager", "/v1/hooks/dispatch",
		map[string]string{"event": "on_before_tool"}, RouteRequest{InteractionID: "isolation-test"})
	if err == nil {
		t.Fatal("HTTP 503 must fail")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "secret-password") ||
		strings.Contains(err.Error(), "private-db") || strings.Contains(err.Error(), "tenant-data") {
		t.Fatalf("untrusted upstream response escaped into authoritative error: %s", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("upstream HTTP status lost: %s", err)
	}
}

func TestCallServiceDoesNotExposeCredentialsOnTransportFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := strings.Replace(upstream.URL, "http://", "http://svc-user:svc-password@", 1)
	upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", endpoint)

	plane := &controlPlane{}
	_, err := plane.callService(context.Background(), "extension-manager", "/v1/hooks/dispatch",
		map[string]string{"event": "on_before_tool"}, RouteRequest{InteractionID: "transport-test"})
	if err == nil {
		t.Fatal("closed upstream must fail")
	}
	if strings.Contains(err.Error(), "svc-user") || strings.Contains(err.Error(), "svc-password") {
		t.Fatalf("upstream URL credentials escaped into authoritative error: %s", err)
	}
}
