package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestFuseResultsCombinesScores(t *testing.T) {
	exact := []RetrievalResult{
		{MemoryID: "a", Title: "Alpha", Provenance: "exact"},
		{MemoryID: "b", Title: "Beta", Provenance: "exact"},
	}
	fts := []RetrievalResult{
		{MemoryID: "b", Title: "Beta", Provenance: "fulltext"},
		{MemoryID: "c", Title: "Gamma", Provenance: "fulltext"},
	}

	fused := fuseResults(60.0, exact, fts)
	if len(fused) != 3 {
		t.Fatalf("expected 3 unique results, got %d", len(fused))
	}
	// 'b' should have the highest fused score (appears in both sets)
	if fused[0].MemoryID != "b" {
		t.Fatalf("expected 'b' as top result (appears in both sets), got %s", fused[0].MemoryID)
	}
	if fused[0].FusedScore <= 0 {
		t.Fatal("expected positive fused score for 'b'")
	}
}

func TestFuseResultsEmptyInputs(t *testing.T) {
	fused := fuseResults(60.0) // no result sets at all
	if len(fused) != 0 {
		t.Fatalf("expected 0 results, got %d", len(fused))
	}
}

func TestFuseResultsSingleSource(t *testing.T) {
	results := []RetrievalResult{
		{MemoryID: "a", Title: "A"},
		{MemoryID: "b", Title: "B"},
	}
	fused := fuseResults(60.0, results)
	if len(fused) != 2 {
		t.Fatalf("expected 2 results, got %d", len(fused))
	}
}

func TestDedupRemovesDuplicates(t *testing.T) {
	results := []RetrievalResult{
		{MemoryID: "a", Title: "A"},
		{MemoryID: "b", Title: "B"},
		{MemoryID: "a", Title: "A (dup)"},
	}
	deduped := deduplicateResults(results)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 after dedup, got %d", len(deduped))
	}
}

func TestDedupEmptyList(t *testing.T) {
	deduped := deduplicateResults(nil)
	if len(deduped) != 0 {
		t.Fatal("expected empty result")
	}
}

func TestTokenBudgetLimitsResults(t *testing.T) {
	results := []RetrievalResult{
		{MemoryID: "a", Content: strings.Repeat("x", 400)},  // ~100 tokens
		{MemoryID: "b", Content: strings.Repeat("y", 8000)}, // ~2000 tokens
		{MemoryID: "c", Content: "short"},                   // ~1 token
	}
	allocated := allocateByTokenBudget(results, 500) // budget for ~500 tokens
	if len(allocated) == 0 {
		t.Fatal("expected at least one result within budget")
	}
	// First result (100 tokens) should fit
	if allocated[0].MemoryID != "a" {
		t.Fatal("expected first result to be included")
	}
}

func TestAssembleSectionsByClass(t *testing.T) {
	results := []RetrievalResult{
		{MemoryID: "g1", MemoryClass: "governing", Content: "policy"},
		{MemoryID: "w1", MemoryClass: "working", Content: "active task"},
		{MemoryID: "e1", MemoryClass: "episodic", Content: "memory"},
		{MemoryID: "p1", MemoryClass: "procedural", Content: "how-to"},
		{MemoryID: "s1", MemoryClass: "semantic", Content: "fact"},
	}

	sections := assembleSections(results)
	if len(sections) == 0 {
		t.Fatal("expected at least one section")
	}

	// Verify each section has the right items
	sectionMap := make(map[string]int)
	for i, sec := range sections {
		sectionMap[sec.Heading] = i
	}

	if idx, ok := sectionMap["Governance"]; ok {
		if len(sections[idx].Items) != 1 || sections[idx].Items[0].MemoryID != "g1" {
			t.Fatal("governance section incorrect")
		}
	}
}

func TestHashQueryNormalisesInput(t *testing.T) {
	h1 := hashQuery("What database do we use?")
	h2 := hashQuery("What database do we use?  ")
	h3 := hashQuery("what DATABASE do We use?")
	if h1 != h2 {
		t.Fatal("hash should be case/whitespace independent")
	}
	if h1 != h3 {
		t.Fatal("hash should be case insensitive")
	}
	if h1 == "" {
		t.Fatal("hash should not be empty")
	}
}

func TestToTSQueryBuildsValidQuery(t *testing.T) {
	q := toTSQuery("What database does PostgreSQL use")
	if !strings.Contains(q, "database:*") {
		t.Fatal("expected database:* in tsquery")
	}
	if !strings.Contains(q, "postgresql:*") {
		t.Fatal("expected postgresql:* in tsquery")
	}
}

func TestToTSQueryMatchesAnyTermAndNeutralisesOperators(t *testing.T) {
	q := toTSQuery("Which database do we use? c++ a:b x|y (test) test")
	if q != "which:* | database:* | do:* | we:* | use:* | c:* | a:* | b:* | x:* | y:* | test:*" {
		t.Fatalf("tsquery = %q", q)
	}
}

func TestToTSQueryEmptyInput(t *testing.T) {
	q := toTSQuery("")
	if q != "" {
		t.Fatal("expected empty tsquery for empty input")
	}
}

func TestEstimateTokens(t *testing.T) {
	tokens := estimateTokens("hello world")
	if tokens <= 0 {
		t.Fatal("expected positive token count")
	}
}

func TestTruncate(t *testing.T) {
	short := "hello"
	if truncated := truncate(short, 10); truncated != short {
		t.Fatal("should not truncate short strings")
	}
	long := strings.Repeat("x", 100)
	if truncated := truncate(long, 10); len(truncated) > 14 {
		t.Fatalf("expected truncated string, got len %d", len(truncated))
	}
}

func TestJSONRoundTripRetrieveResponse(t *testing.T) {
	resp := RetrieveResponse{
		RetrievalRunID: "run-1",
		QueryHash:      "abc123",
		Sections: []ContextSection{
			{Heading: "Facts", Items: []RetrievalResult{
				{MemoryID: "m1", Title: "Fact 1", FusedScore: 0.95},
			}},
		},
		TotalTokens: 150,
		LatencyMs:   42,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	var decoded RetrieveResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.RetrievalRunID != "run-1" {
		t.Fatal("roundtrip failed")
	}
	if len(decoded.Sections) != 1 {
		t.Fatal("roundtrip failed: sections")
	}
}

func TestRRFFloat64(t *testing.T) {
	rrf := 1.0 / (float64(rrfK) + float64(0))
	if rrf != 1.0/60.0 {
		t.Fatalf("unexpected RRF score: %f", rrf)
	}
}

func TestEstimateSectionsTokens(t *testing.T) {
	got := estimateSectionsTokens([]ContextSection{{Items: []RetrievalResult{{Content: strings.Repeat("x", 40)}}}})
	if got != 10 {
		t.Fatalf("expected 10 tokens, got %d", got)
	}
}

func TestMinInt(t *testing.T) {
	if min(5, 10) != 5 {
		t.Fatal("min should return smaller value")
	}
	if min(10, 5) != 5 {
		t.Fatal("min should return smaller value")
	}
}

func TestToJSONB(t *testing.T) {
	data := toJSONB(map[string]string{"key": "val"})
	if !strings.Contains(string(data), "key") {
		t.Fatal("JSON encoding should contain key")
	}
}

func TestVectorLiteralBuildsPgvectorParameter(t *testing.T) {
	if got := vectorLiteral([]float64{0.125, -2, 3.5}); got != "[0.125,-2,3.5]" {
		t.Fatalf("unexpected vector literal %q", got)
	}
}

func TestDecodeResultJSONLoadsEvidenceAndConflicts(t *testing.T) {
	result := RetrievalResult{}
	decodeResultJSON(&result,
		[]byte(`[{"kind":"commit","reference_id":"abc"}]`),
		[]byte(`[{"relation_type":"conflict","target_memory_id":"m2"}]`))
	if len(result.Evidence) != 1 || len(result.Conflicts) != 1 {
		t.Fatalf("expected evidence and conflict payloads, got %d and %d", len(result.Evidence), len(result.Conflicts))
	}
}

func TestContainsAllNamespacesFailsClosedForUnauthorizedRequest(t *testing.T) {
	if containsAllNamespaces([]string{"public", "restricted"}, []string{"public"}) {
		t.Fatal("expected an unauthorized requested namespace to fail closed")
	}
}

func TestContainsAllNamespacesAcceptsAuthorizedRequest(t *testing.T) {
	if !containsAllNamespaces([]string{"public"}, []string{"public", "other"}) {
		t.Fatal("expected an authorized namespace request to pass")
	}
}

// TestRetrieveRejectsCrossTenantBodyOverride proves the fix for the tenant
// isolation gap in resolveIdentity: RetrieveRequest.TenantID/PrincipalID are
// ordinary JSON fields that reach this service as mcp-gateway's scos_retrieve
// tool-call arguments -- attacker-controlled, not authenticated. Before this
// fix, handleRetrieve trusted req.TenantID/req.PrincipalID directly, so any
// caller with a valid bearer token could search another tenant's memory by
// naming a different tenant_id in the request body. This drives the request
// through the actual mux (not a bare handler call) with real claims attached
// to the context, exactly as auth.AuthenticatedHandler would leave them, and
// asserts a body tenant/principal that disagrees with the authenticated
// claims is rejected before any database access.
func TestRetrieveRejectsCrossTenantBodyOverride(t *testing.T) {
	mux := newRetrievalMux(&server{})
	body := `{"tenant_id":"attacker-tenant","principal_id":"victim-principal","query":"secret"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
		TenantID: "real-tenant", PrincipalID: "real-principal",
	}))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected %d for a body tenant/principal that disagrees with authenticated claims, got %d: %s",
			http.StatusForbidden, recorder.Code, recorder.Body.String())
	}
}

// TestRetrieveIgnoresBodyIdentityWhenClaimsAgree confirms claims -- not the
// body -- are what select whose memory is searched: an empty body
// tenant/principal (the normal shape for a well-behaved caller) is accepted
// and resolved from claims, never left unauthenticated.
func TestResolveIdentityUsesClaimsIgnoringBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
		TenantID: "real-tenant", PrincipalID: "real-principal",
	}))
	tenantID, principalID, status, msg := resolveIdentity(req, "", "")
	if status != 0 {
		t.Fatalf("expected identity to resolve from claims, got status %d (%s)", status, msg)
	}
	if tenantID != "real-tenant" || principalID != "real-principal" {
		t.Fatalf("expected identity from claims, got tenant=%q principal=%q", tenantID, principalID)
	}
}

// TestRetrieveRequiresHeadersWhenAuthenticatedWithoutClaims exercises the
// static-shared-token path (no claims on the context): the caller is
// authenticated, but with no per-caller identity to trust, the handler must
// still refuse to fall back to the request body and instead require the
// X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers that mcp-gateway sets
// authoritatively.
func TestRetrieveRequiresHeadersWhenAuthenticatedWithoutClaims(t *testing.T) {
	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{}), auth.Config{Token: "shared-secret", Identity: "internal-service"})
	body := `{"tenant_id":"attacker-tenant","principal_id":"attacker-principal","query":"secret"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer shared-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d without X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers, got %d: %s",
			http.StatusUnauthorized, recorder.Code, recorder.Body.String())
	}
}

func TestRetrieveRequiresPrincipalContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"tenant_id":"tenant","query":"secret"}`))
	recorder := httptest.NewRecorder()
	(&server{}).handleRetrieve(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "principal_id is required") {
		t.Fatalf("expected principal context error, got %q", recorder.Body.String())
	}
}

func TestFusionRecordsTheLanesThatFoundEachMemory(t *testing.T) {
	a := RetrievalResult{MemoryID: "a", MemoryClass: "working"}
	b := RetrievalResult{MemoryID: "b", MemoryClass: "semantic"}
	fused := fuseResults(rrfK, []RetrievalResult{a}, []RetrievalResult{a, b}, nil, nil, nil, []RetrievalResult{b}, nil)
	got := map[string]string{}
	for _, r := range fused {
		got[r.MemoryID] = r.Provenance
	}
	if got["a"] != "exact+fulltext" || got["b"] != "fulltext+recent" {
		t.Fatalf("provenance = %v", got)
	}
	for _, sec := range assembleSections(fused) {
		for _, item := range sec.Items {
			if item.Provenance != got[item.MemoryID] {
				t.Fatalf("section assembly must keep lane provenance, %s became %q", item.MemoryID, item.Provenance)
			}
		}
	}
}

func TestFilterMemoryClassesKeepsOnlyRequestedClasses(t *testing.T) {
	in := []RetrievalResult{{MemoryID: "a", MemoryClass: "semantic"}, {MemoryID: "b", MemoryClass: "procedural"}}
	if out := filterMemoryClasses(in, []string{"Procedural"}); len(out) != 1 || out[0].MemoryID != "b" {
		t.Fatalf("filtered = %+v", out)
	}
	if out := filterMemoryClasses(in, nil); len(out) != 2 {
		t.Fatal("no classes requested must keep everything")
	}
}

func TestRetrieveRejectsUnsupportedFilters(t *testing.T) {
	s := &server{}
	body := `{"tenant_id":"00000000-0000-0000-0000-000000000001","principal_id":"00000000-0000-0000-0000-000000000001","query":"x","filters":{"repo":"y"}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(body))
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "00000000-0000-0000-0000-000000000001", PrincipalID: "00000000-0000-0000-0000-000000000001"}))
	w := httptest.NewRecorder()
	s.handleRetrieve(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "filters are not supported") {
		t.Fatalf("filters must be refused rather than silently ignored, got %d %s", w.Code, w.Body.String())
	}
}

func TestAssembleSectionsIsAnEmptyListWhenNothingMatched(t *testing.T) {
	if got := assembleSections(nil); got == nil || len(got) != 0 {
		t.Fatalf("sections = %#v, want an empty list", got)
	}
}

func TestFusionKeepsAStrongerLexicalMatchAboveAWeakerOneWithMoreAuthority(t *testing.T) {
	weak := RetrievalResult{MemoryID: "weak", RawLexicalScore: 0.025, Authority: 90, Confidence: 0.95}
	strong := RetrievalResult{MemoryID: "strong", RawLexicalScore: 0.076, Authority: 70, Confidence: 0.75}
	ranked := applyGovernanceWeighting(fuseResults(rrfK, nil, []RetrievalResult{weak, strong}))
	if ranked[0].MemoryID != "strong" {
		t.Fatalf("a memory matching three times as strongly must lead, got %s first", ranked[0].MemoryID)
	}
	equal := applyGovernanceWeighting(fuseResults(rrfK, nil, []RetrievalResult{
		{MemoryID: "low", RawLexicalScore: 0.05, Authority: 40, Confidence: 0.5},
		{MemoryID: "high", RawLexicalScore: 0.05, Authority: 90, Confidence: 0.95},
	}))
	if equal[0].MemoryID != "high" {
		t.Fatalf("equally relevant memories are ordered by governance, got %s first", equal[0].MemoryID)
	}
}
