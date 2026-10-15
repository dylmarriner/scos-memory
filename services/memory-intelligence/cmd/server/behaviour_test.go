package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestPeerProbePresentsTheServiceCredentials(t *testing.T) {
	var gotBearer, gotControl string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/ready":
			w.WriteHeader(http.StatusOK)
		case "/v1/version":
			gotBearer = r.Header.Get("Authorization")
			gotControl = r.Header.Get("X-SCOS-Control-Plane-Token")
			if gotBearer != "Bearer svc-token" || gotControl != "cp-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(versionInfo{Status: "ok", Version: "9.9.9", UptimeSeconds: 61})
		}
	}))
	defer peer.Close()

	s := &engineServer{httpClient: peer.Client(), internalServiceToken: "svc-token", controlPlaneToken: "cp-token"}
	entry := s.probePeer(context.Background(), "peer", peer.URL)
	if entry.Status != "healthy" || entry.Version != "9.9.9" || entry.Uptime != "1m1s" || entry.Detail != "" {
		t.Fatalf("authenticated probe must read the peer's version: %#v (bearer %q, control %q)", entry, gotBearer, gotControl)
	}

	// Without credentials the peer refuses, and the map says so rather than
	// reporting an unexplained unknown version.
	bare := &engineServer{httpClient: peer.Client()}
	entry = bare.probePeer(context.Background(), "peer", peer.URL)
	if entry.Version != "unknown" || !strings.Contains(entry.Detail, "refused") || !strings.Contains(entry.Detail, "401") {
		t.Fatalf("a refused probe must be reported as refused: %#v", entry)
	}
}

func TestPeerProbeFallsBackToTheCallersVerifiedBearer(t *testing.T) {
	s := &engineServer{controlPlaneToken: "cp"}
	r := httptest.NewRequest(http.MethodGet, "/v1/dashboard/system", nil)
	r.Header.Set("Authorization", "Bearer caller-token")
	var headers http.Header
	handler := auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = s.peerProbeHeaders(r.Context())
	}), auth.Config{Token: "caller-token", ControlPlaneToken: "cp"})
	handler.ServeHTTP(httptest.NewRecorder(), r)
	if headers.Get("Authorization") != "Bearer caller-token" || headers.Get("X-SCOS-Control-Plane-Token") != "cp" {
		t.Fatalf("unexpected probe headers %v", headers)
	}
}

func TestRelationConfidence(t *testing.T) {
	for _, c := range []float64{0, 0.5, 1} {
		if !validRelationConfidence(c) {
			t.Errorf("%v must be valid", c)
		}
	}
	for _, c := range []float64{-0.01, 1.01, math.NaN(), math.Inf(1)} {
		if validRelationConfidence(c) {
			t.Errorf("%v must be invalid", c)
		}
	}
	if got := relationConfidenceFromFacts(&Fact{Confidence: 0.9}, &Fact{Confidence: 0.4}); got != 0.4 {
		t.Fatalf("a relation is as certain as its weaker fact, got %v", got)
	}
}

func TestRelationRequestConfidenceIsValidated(t *testing.T) {
	s := newEngineServer()
	for _, body := range []string{`{"source_memory_id":"a","target_memory_id":"b","relation_type":"derived_from","confidence":1.5}`,
		`{"source_memory_id":"a","target_memory_id":"b","relation_type":"derived_from","confidence":-1}`} {
		w := httptest.NewRecorder()
		s.handleRelations(w, httptest.NewRequest(http.MethodPost, "/v1/relations", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "confidence") {
			t.Fatalf("out-of-range confidence must be refused, got %d %s", w.Code, w.Body.String())
		}
	}
}

func TestHierarchyRefusesACallerSuppliedFactCount(t *testing.T) {
	s := newEngineServer()
	w := httptest.NewRecorder()
	s.handleHierarchy(w, httptest.NewRequest(http.MethodPost, "/v1/hierarchy", strings.NewReader(`{"name":"ops","fact_count":42}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "fact_count is computed") {
		t.Fatalf("fact_count must be refused, got %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleHierarchy(w, httptest.NewRequest(http.MethodPost, "/v1/hierarchy", strings.NewReader(`{"name":"ops","level":"basement"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown level must be refused, got %d", w.Code)
	}
}

func TestSubtreeFactsCountsDistinctFactsAndSurvivesCycles(t *testing.T) {
	children := map[string][]string{"palace": {"wing"}, "wing": {"drawer", "palace"}}
	direct := map[string][]string{"palace": {"f1"}, "wing": {"f2"}, "drawer": {"f2", "f3"}}
	if got := len(subtreeFacts("palace", children, direct)); got != 3 {
		t.Fatalf("palace holds 3 distinct facts, got %d", got)
	}
	if got := len(subtreeFacts("drawer", children, direct)); got != 2 {
		t.Fatalf("drawer holds 2 facts, got %d", got)
	}
}

func TestCodeLinksBelowThePromotionBarAreReportedDropped(t *testing.T) {
	s := &engineServer{okfAuthorityThresh: 90, okfConfidenceThresh: 0.85}
	fact := &Fact{ID: "f1", Authority: 70, Confidence: 0.75, CodeSymbolVersionIDs: []string{"c1", "c2"}}
	report := s.maybePromoteToOKF(fact, requestIdentity{TenantID: "t", PrincipalID: "p"})
	if len(report.Linked) != 0 || len(report.Dropped) != 2 {
		t.Fatalf("nothing is linked below the bar: %#v", report)
	}
	for i, d := range report.Dropped {
		if d.CodeSymbolVersionID != fact.CodeSymbolVersionIDs[i] || !strings.Contains(d.Reason, "below the OKF promotion threshold") {
			t.Fatalf("dropped link must name its id and reason: %#v", d)
		}
	}

	// Above the bar but with promotion unconfigured, links are still not
	// written and say why.
	fact.Authority, fact.Confidence = 95, 0.95
	report = s.maybePromoteToOKF(fact, requestIdentity{TenantID: "t", PrincipalID: "p"})
	if len(report.Linked) != 0 || len(report.Dropped) != 2 || !strings.Contains(report.Dropped[0].Reason, "not configured") {
		t.Fatalf("unconfigured promotion must report links dropped: %#v", report)
	}
}
