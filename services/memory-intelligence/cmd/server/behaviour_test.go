package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/release"
)

func TestPeerProbeReadsReadinessBuildAndUptime(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/ready":
			w.WriteHeader(http.StatusOK)
		case "/health/version":
			_ = json.NewEncoder(w).Encode(release.Info{Service: "peer", SourceSHA: "0123456789abcdef0123", StartedAt: "2026-10-17T00:00:00Z", UptimeSeconds: 61})
		}
	}))
	defer peer.Close()
	s := &engineServer{httpClient: peer.Client()}
	entry := s.probePeer(context.Background(), "peer", peer.URL)
	if entry.Status != "healthy" || entry.Version != "0123456789ab" || entry.Uptime != "1m1s" || entry.Detail != "" {
		t.Fatalf("probe = %#v", entry)
	}

	// A build that does not report its start time has an unknown uptime,
	// not zero.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"service": "old", "source_sha": "abc"})
		}
	}))
	defer old.Close()
	if entry := s.probePeer(context.Background(), "old", old.URL); entry.Uptime != "unknown" || entry.Version != "abc" {
		t.Fatalf("old build probe = %#v", entry)
	}
}

func TestSystemMapCoversTheFleet(t *testing.T) {
	names := map[string]bool{}
	for _, p := range peerDefaults {
		names[p.name] = true
	}
	for _, want := range []string{"security-gate", "tool-governance", "observability", "backup-manager", "mcp-gateway", "agent-registry", "credential-vault"} {
		if !names[want] {
			t.Errorf("system map does not probe %s", want)
		}
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
