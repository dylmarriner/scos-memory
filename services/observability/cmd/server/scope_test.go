package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func asClaims(r *http.Request, tenant string, scopes ...string) *http.Request {
	return r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p-" + tenant, Scopes: scopes}))
}

func TestClientIngestIsStampedCannotBackdateAndStaysOutOfRollups(t *testing.T) {
	s := newTestServer(t)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	rec := httptest.NewRecorder()
	s.handleMetricsIngest(rec, asClaims(httptest.NewRequest(http.MethodPost, "/v1/metrics/ingest", bytes.NewBufferString(`[{"name":"scos.http.server.request","value":1,"timestamp":"`+old+`","labels":{"service":"memory-gateway"}}]`)), "tenant-a"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only services may record past signals") {
		t.Fatalf("backdated client point = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleMetricsIngest(rec, asClaims(httptest.NewRequest(http.MethodPost, "/v1/metrics/ingest", bytes.NewBufferString(`[{"name":"scos.http.server.request","value":1,"tenant":"tenant-b","labels":{"service":"memory-gateway"}}]`)), "tenant-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("client ingest = %d %s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	got := s.metrics[len(s.metrics)-1]
	rollupsEmpty := s.rollups.empty()
	s.mu.RUnlock()
	if got.Tenant != "tenant-a" || got.Labels["source"] != "client" {
		t.Fatalf("client point = %+v", got)
	}
	if !rollupsEmpty {
		t.Fatalf("a client point reached the rollups")
	}
	// A service's point does feed them.
	rec = httptest.NewRecorder()
	s.handleMetricsIngest(rec, httptest.NewRequest(http.MethodPost, "/v1/metrics/ingest", bytes.NewBufferString(`[{"name":"scos.http.server.request","value":1,"labels":{"service":"memory-gateway"}}]`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("service ingest = %d", rec.Code)
	}
	s.mu.RLock()
	rollupsEmpty = s.rollups.empty()
	s.mu.RUnlock()
	if rollupsEmpty {
		t.Fatal("a service point did not reach the rollups")
	}
}

func TestSignalsAreListedPerTenantUnlessPlatformReader(t *testing.T) {
	s := newTestServer(t)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		rec := httptest.NewRecorder()
		s.handleMetricsIngest(rec, asClaims(httptest.NewRequest(http.MethodPost, "/v1/metrics/ingest", bytes.NewBufferString(`[{"name":"custom.metric","value":1}]`)), tenant))
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest %s = %d", tenant, rec.Code)
		}
	}
	list := func(r *http.Request) []MetricPoint {
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, r)
		var out []MetricPoint
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := list(asClaims(httptest.NewRequest(http.MethodGet, "/v1/metrics", nil), "tenant-a")); len(got) != 1 || got[0].Tenant != "tenant-a" {
		t.Fatalf("tenant-a sees %+v", got)
	}
	if got := list(asClaims(httptest.NewRequest(http.MethodGet, "/v1/metrics", nil), "tenant-c", observabilityReadScope)); len(got) != 2 {
		t.Fatalf("platform reader sees %d", len(got))
	}
}
