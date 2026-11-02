package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// Permission telemetry describes signed manifest declarations, not runtime grants.
func TestExtensionPermissionMetricsOnlyAfterDurableSignedInstall(t *testing.T) {
	srv, key := canaryFixture(t)
	metrics := extensionMetricCollector(t)

	install := func(version string, permissions []string, corrupt bool) *httptest.ResponseRecorder {
		t.Helper()
		m := canaryManifest(t, key, version)
		m.Permissions = permissions
		m = sealManifest(t, m, key)
		if corrupt {
			m.Signature = "invalid"
		}
		res := httptest.NewRecorder()
		srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, m))))
		return res
	}

	if res := install("2.0.0", []string{"filesystem.read", "network.outbound"}, false); res.Code != http.StatusOK {
		t.Fatalf("signed install: %d %s", res.Code, res.Body.String())
	}
	points := filterExtensionMetrics(metrics(), telemetry.MetricExtensionPermissions)
	if len(points) != 1 || points[0].Value != 2 ||
		points[0].Labels["extension"] != "canary.ext" ||
		points[0].Labels["version"] != "2.0.0" ||
		points[0].Labels["scope"] != "declared" {
		t.Fatalf("expected declared permission count after durable install, got %+v", points)
	}
	if res := install("2.1.0", []string{"admin"}, true); res.Code != http.StatusForbidden {
		t.Fatalf("invalid signature accepted: %d %s", res.Code, res.Body.String())
	}
	srv.persistBeforeWrite = func(context.Context) error { return errors.New("injected persistence failure") }
	if res := install("3.0.0", []string{"filesystem.root"}, false); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed persistence accepted: %d %s", res.Code, res.Body.String())
	}
	if got := filterExtensionMetrics(metrics(), telemetry.MetricExtensionPermissions); len(got) != 1 {
		t.Fatalf("rejected or uncommitted permissions emitted telemetry: %+v", got)
	}
}
