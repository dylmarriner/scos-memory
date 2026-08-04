package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
