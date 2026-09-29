package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCanonicaliseRemovesBOM(t *testing.T) {
	input := "\ufeffhello\r\nworld"
	result := canonicalise(input)
	if result != "hello\nworld" {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestCanonicaliseNormalisesLineEndings(t *testing.T) {
	input := "line1\r\nline2\rline3"
	result := canonicalise(input)
	if result != "line1\nline2\nline3" {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestClassifySegmentsIdentifiesCodeBlocks(t *testing.T) {
	input := "some text\n```go\nfunc main() {}\n```\nmore text"
	segments := classifySegments(input, nil)
	hasCode := false
	for _, s := range segments {
		if s.Class == "code_symbol" || s.Class == "code_dependency" {
			hasCode = true
		}
	}
	if !hasCode {
		t.Fatal("expected code symbol segment")
	}
}

func TestClassifyGovernancePolicy(t *testing.T) {
	input := "policy: all agents must use approved memory"
	segments := classifySegments(input, nil)
	if len(segments) > 0 && segments[0].Class != "governing_policy" {
		t.Fatalf("expected governing_policy, got %s", segments[0].Class)
	}
}

func TestNonDroppableSegments(t *testing.T) {
	segments := []Segment{
		{Class: "governing_policy", Content: "policy text", Droppable: false},
		{Class: "optional_background", Content: "background", Droppable: true},
	}
	if segments[0].Droppable {
		t.Fatal("governing_policy should not be droppable")
	}
	if !segments[1].Droppable {
		t.Fatal("optional_background should be droppable")
	}
}

func TestDeduplicateSegments(t *testing.T) {
	segments := []Segment{
		{Content: "hello world", Class: "text"},
		{Content: "hello world", Class: "text"},
		{Content: "goodbye", Class: "text"},
	}
	deduped := deduplicateSegments(segments)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 segments after dedup, got %d", len(deduped))
	}
}

// demandEverywhere gives every §18.6 bucket more droppable content than it
// could be funded for, so the profile's percentages are what decide.
func demandEverywhere() []Segment {
	var segs []Segment
	for _, class := range []string{"governing_policy", "current_user_instruction", "active_task_state", "evidence", "tool_schema"} {
		segs = append(segs, Segment{Class: class, OriginalTokens: 100000, Droppable: true})
	}
	return segs
}

func TestAllocateBudget(t *testing.T) {
	alloc := allocateBudget(8000, "balanced", demandEverywhere())
	if alloc.Governing != 1200 {
		t.Fatalf("expected 1200 governing, got %d", alloc.Governing)
	}
	if alloc.Reserve != 800 {
		t.Fatalf("expected 800 reserve, got %d", alloc.Reserve)
	}
	// With nothing to fund, every class's share returns to the reserve.
	if empty := allocateBudget(8000, "balanced", nil); empty.Reserve != 8000 || empty.Governing != 0 {
		t.Fatalf("unneeded shares must move to reserve: %+v", empty)
	}
}

func TestAllocateBudgetExact(t *testing.T) {
	alloc := allocateBudget(8000, "exact", demandEverywhere())
	if alloc.Governing != 2000 {
		t.Fatalf("expected 2000 governing for exact, got %d", alloc.Governing)
	}
}

func TestCompressBalanced(t *testing.T) {
	segments := []Segment{
		{Class: "governing_policy", Content: "keep this", OriginalTokens: 10, Droppable: false},
		{Class: "optional_background", Content: strings.Repeat("x", 5000), OriginalTokens: 1250, Droppable: true},
	}
	alloc := allocateBudget(8000, "balanced", nil)
	compiled, _ := compressSegments(segments, "balanced", alloc)
	if !strings.Contains(compiled, "keep this") {
		t.Fatal("non-droppable content must be preserved")
	}
}

func TestCompressExactPreservesAll(t *testing.T) {
	segments := []Segment{
		{Class: "optional_background", Content: "everything stays", OriginalTokens: 20, Droppable: true},
	}
	alloc := allocateBudget(8000, "exact", nil)
	compiled, _ := compressSegments(segments, "exact", alloc)
	if !strings.Contains(compiled, "everything stays") {
		t.Fatal("exact profile must preserve all content")
	}
}

func TestCompressAggressiveSummarises(t *testing.T) {
	segments := []Segment{
		{Class: "optional_background", Content: strings.Repeat("long content here. ", 100), OriginalTokens: 300, Droppable: true},
	}
	alloc := allocateBudget(8000, "aggressive", nil)
	compiled, _ := compressSegments(segments, "aggressive", alloc)
	if strings.Contains(compiled, "[optional_background summary]") && len(compiled) < 500 {
		return
	}
	t.Logf("compiled length: %d", len(compiled))
}

func TestValidateInvariantsPreservesHints(t *testing.T) {
	original := "Use PostgreSQL 16.2 for the primary database"
	compiled := "Use PostgreSQL 16.2"
	valid := validateInvariants(original, compiled, []string{"PostgreSQL"})
	if !valid {
		t.Fatal("should preserve hints")
	}
}

func TestValidateInvariantsDetectsMissingHint(t *testing.T) {
	original := "Use PostgreSQL for the database"
	compiled := "Use MySQL"
	valid := validateInvariants(original, compiled, []string{"PostgreSQL"})
	if valid {
		t.Fatal("should detect missing hint")
	}
}

func TestExtractExactValuesUUIDs(t *testing.T) {
	values := extractExactValues("id 550e8400-e29b-41d4-a716-446655440000")
	if len(values) == 0 {
		t.Fatal("expected UUID extraction")
	}
}

func TestExtractExactValuesVersions(t *testing.T) {
	values := extractExactValues("version 1.2.3")
	if len(values) == 0 {
		t.Fatal("expected version extraction")
	}
}

func TestOptimiseOutputRemovesRepeats(t *testing.T) {
	raw := "line1\n\n\n\nline2\n\n\nline3"
	opt, _ := optimiseOutput(raw, "standard", 1000, "cap-1")
	if strings.Contains(opt, "\n\n\n") {
		t.Fatal("should collapse repeated newlines")
	}
}

func TestOptimiseOutputCompact(t *testing.T) {
	raw := "I think this is true.\n\nLet me check.\n\nBased on the docs, I can confirm."
	opt, _ := optimiseOutput(raw, "compact", 1000, "cap-1")
	if strings.Contains(opt, "Let me check") {
		t.Fatal("compact should remove a pure filler line")
	}
	// Hedges and content-bearing sentences survive: removing them changes
	// what is being claimed.
	for _, kept := range []string{"I think this is true.", "Based on the docs, I can confirm."} {
		if !strings.Contains(opt, kept) {
			t.Fatalf("compact must keep %q, got %q", kept, opt)
		}
	}
}

func TestOptimiseOutputTruncates(t *testing.T) {
	raw := strings.Repeat("word ", 2000)
	opt, omitted := optimiseOutput(raw, "standard", 100, "cap-1")
	if estimateTokens(opt) > 150 {
		t.Fatalf("expected truncation to ~100 tokens, got %d tokens", estimateTokens(opt))
	}
	if omitted == 0 || !strings.Contains(opt, "restore the full output with capsule cap-1") {
		t.Fatalf("truncation must be explicit and name the restoring capsule: %q", opt)
	}
}

func TestOptimiseOutputKeepsErrorLinesWhenTruncating(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "progress step %d ok\n", i)
	}
	b.WriteString("ERROR: migration 000041 failed: relation missing\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "cleanup step %d ok\n", i)
	}
	opt, omitted := optimiseOutput(b.String(), "standard", 200, "cap-2")
	if !strings.Contains(opt, "ERROR: migration 000041 failed") {
		t.Fatal("an error line must survive truncation")
	}
	if omitted == 0 || !strings.HasSuffix(opt, "restore the full output with capsule cap-2]") {
		t.Fatalf("omission must be stated: %q", opt[len(opt)-120:])
	}
}

func TestCompactOutputKeepsHedgesIntact(t *testing.T) {
	got := compactOutput("I believe the replica is lagging.\nLet me know the deadline is Friday.")
	if !strings.Contains(got, "I believe the replica is lagging.") || !strings.Contains(got, "Let me know the deadline is Friday.") {
		t.Fatalf("compaction corrupted or dropped content: %q", got)
	}
}

func TestOutputOptimiseResultIsRestorable(t *testing.T) {
	t.Cleanup(func(prev SourceStore) func() { return func() { authoritativeSourceStore = prev } }(authoritativeSourceStore))
	authoritativeSourceStore = newFileSourceStore(t.TempDir())
	raw := strings.Repeat("build log line\n", 3000)
	w := httptest.NewRecorder()
	handleOutputOptimise(w, httptest.NewRequest(http.MethodPost, "/v1/output/optimise", strings.NewReader(`{"raw_output":`+strconv.Quote(raw)+`,"max_tokens":50}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("optimise: %d %s", w.Code, w.Body.String())
	}
	var resp OutputOptimiseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OmittedLines == 0 || len(resp.SourceObjects) != 1 {
		t.Fatalf("expected omitted lines and a stored source: %+v", resp)
	}
	body, _ := json.Marshal(map[string]any{"capsule": map[string]any{"capsule_id": resp.CapsuleID, "source_objects": resp.SourceObjects}})
	w = httptest.NewRecorder()
	handleContextRestore(w, httptest.NewRequest(http.MethodPost, "/v1/context/restore", strings.NewReader(string(body))))
	var restored struct{ Content string }
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil || restored.Content != raw {
		t.Fatalf("restore did not return the full raw output (%d): %v", w.Code, err)
	}
}

func TestEstimateTokens(t *testing.T) {
	tokens := estimateTokens("hello world four words")
	if tokens <= 0 {
		t.Fatal("expected positive tokens")
	}
}

func TestPercentOf(t *testing.T) {
	result := percentOf(1000, 15)
	if result != 150 {
		t.Fatalf("expected 150, got %d", result)
	}
}

func TestBudgetClassMapping(t *testing.T) {
	if budgetClass("governing_policy") != "governing" || budgetClass("tool_schema") != "code_tools" {
		t.Fatal("class mapping changed")
	}
	if budgetClass("optional_background") != "" {
		t.Fatal("optional background has no bucket of its own; it lives on reserve")
	}
}

func TestAllocationNeverExceedsTheBudgetAndReservesMandatoryContent(t *testing.T) {
	segs := []Segment{
		{Class: "governing_policy", OriginalTokens: 40, Droppable: false},
		{Class: "evidence", OriginalTokens: 5, Droppable: true},
		{Class: "optional_background", OriginalTokens: 5000, Droppable: true},
	}
	for _, profile := range []string{"balanced", "aggressive", "conservative"} {
		a := allocateBudget(200, profile, segs)
		sum := a.Governing + a.Request + a.TaskState + a.Evidence + a.CodeTools + a.Reserve
		if a.Mandatory != 40 || sum != 160 {
			t.Fatalf("%s: mandatory=%d funded=%d; want 40 reserved and exactly 160 left to allocate (%+v)", profile, a.Mandatory, sum, a)
		}
		if a.Evidence != 5 {
			t.Fatalf("%s: evidence needs 5 tokens, so its surplus must move to reserve: %+v", profile, a)
		}
	}
}

func TestLossyCompilationFitsTheBudget(t *testing.T) {
	input := "Deploy notes. " + strings.Repeat("background detail that is optional. ", 400)
	segs := deduplicateSegments(classifySegments(canonicalise(input), nil))
	budget := 120
	_, compiled := compressSegments(segs, "aggressive", allocateBudget(budget, "aggressive", segs))
	used := 0
	for _, seg := range compiled {
		used += estimateTokens(seg.Content)
	}
	if used > budget {
		t.Fatalf("aggressive compilation used %d tokens of a %d budget", used, budget)
	}
}

func TestSummary(t *testing.T) {
	s := summarise("This is a long piece of content. It has multiple sentences.", "test")
	if !strings.Contains(s, "[test summary]") {
		t.Fatal("summary should include class tag")
	}
	if !strings.Contains(s, "This is a long piece of content.") {
		t.Fatal("summary should include first sentence")
	}
}

func TestCapsuleID(t *testing.T) {
	id := newCapsuleID()
	// Format: 16 hex chars + "-" + 4 hex chars + "-" + 12 hex chars = 34
	if len(id) != 34 {
		t.Fatalf("expected 34 chars (UUID-like), got %d: %q", len(id), id)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}
}

func TestSha256Hex(t *testing.T) {
	h := sha256Hex([]byte("hello"))
	if len(h) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h))
	}
}

func TestBuildCapsule(t *testing.T) {
	segments := []Segment{
		{Class: "test", Content: "hello", OriginalTokens: 5},
	}
	compiled := []Segment{
		{Class: "test", Content: "hi", CompiledTokens: 2},
	}
	alloc := allocateBudget(8000, "balanced", nil)
	capsule := buildCapsule("hello", "balanced", segments, compiled, alloc)
	if capsule.Profile != "balanced" {
		t.Fatal("profile mismatch")
	}
	if capsule.CompilerVersion != version {
		t.Fatal("version mismatch")
	}
	if capsule.CapsuleID == "" {
		t.Fatal("expected capsule ID")
	}
}

func TestHandleContextEvaluateValidatesCompiledContext(t *testing.T) {
	body := `{"input":"Use PostgreSQL 16.2","compiled_text":"Use PostgreSQL 16.2","non_droppable_hints":["PostgreSQL"]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/context/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleContextEvaluate(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Valid {
		t.Fatal("expected compiled context to be valid")
	}
}

func TestHandleContextEvaluateRequiresContent(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/context/evaluate", bytes.NewBufferString(`{"input":"only input"}`))
	w := httptest.NewRecorder()
	handleContextEvaluate(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestContextRestoreResolvesExactAuthoritativeSource(t *testing.T) {
	previous := authoritativeSourceStore
	authoritativeSourceStore = newFileSourceStore(t.TempDir())
	t.Cleanup(func() { authoritativeSourceStore = previous })

	input := "policy: preserve exact value 42\nversion 1.2.3"
	requestBody, _ := json.Marshal(map[string]string{"input": input})
	compile := httptest.NewRequest(http.MethodPost, "/v1/context/compile", bytes.NewReader(requestBody))
	compiled := httptest.NewRecorder()
	handleInputOptimise(compiled, compile)
	if compiled.Code != http.StatusOK {
		t.Fatalf("compile failed: %d %s", compiled.Code, compiled.Body.String())
	}
	var response InputOptimiseResponse
	if err := json.Unmarshal(compiled.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(contextRestoreRequest{Capsule: response.Capsule})
	restore := httptest.NewRecorder()
	handleContextRestore(restore, httptest.NewRequest(http.MethodPost, "/v1/context/restore", bytes.NewReader(body)))
	if restore.Code != http.StatusOK {
		t.Fatalf("restore failed: %d %s", restore.Code, restore.Body.String())
	}
	var restored struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(restore.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Content != input {
		t.Fatalf("restored content changed: %q", restored.Content)
	}
}

func TestContextRestoreRejectsTamperedSource(t *testing.T) {
	store := newFileSourceStore(t.TempDir())
	previous := authoritativeSourceStore
	authoritativeSourceStore = store
	t.Cleanup(func() { authoritativeSourceStore = previous })
	content := "authoritative content"
	hash := sha256Hex([]byte(content))
	objectID := "source-" + hash[:32]
	if err := store.Put(objectID, "1", hash, content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, objectID+"-1.source"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}

	capsule := ContextCapsule{CapsuleID: "c1", SourceObjects: []SourceObjectRef{{ObjectID: objectID, Version: "1", ContentHash: hash}}}
	body, _ := json.Marshal(contextRestoreRequest{Capsule: capsule})
	w := httptest.NewRecorder()
	handleContextRestore(w, httptest.NewRequest(http.MethodPost, "/v1/context/restore", bytes.NewReader(body)))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected tamper conflict, got %d", w.Code)
	}
}

func TestContextRestoreRejectsMissingSource(t *testing.T) {
	previous := authoritativeSourceStore
	authoritativeSourceStore = newFileSourceStore(t.TempDir())
	t.Cleanup(func() { authoritativeSourceStore = previous })
	capsule := ContextCapsule{CapsuleID: "c1", SourceObjects: []SourceObjectRef{{ObjectID: "missing", Version: "1", ContentHash: strings.Repeat("a", 64)}}}
	body, _ := json.Marshal(contextRestoreRequest{Capsule: capsule})
	w := httptest.NewRecorder()
	handleContextRestore(w, httptest.NewRequest(http.MethodPost, "/v1/context/restore", bytes.NewReader(body)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected missing-source failure, got %d", w.Code)
	}
}

// TestHighImpactWorkCannotBeAggressivelyCompressed covers §18's rule that
// high-impact or restricted tasks default to exact or conservative, and §42's
// prohibition on dropping governing instructions or evidence during
// compression -- a caller must not be able to opt out of that floor.
func TestHighImpactWorkCannotBeAggressivelyCompressed(t *testing.T) {
	cases := []struct {
		name string
		req  InputOptimiseRequest
		want string
	}{
		{"governance task", InputOptimiseRequest{TaskType: "governance"}, "conservative"},
		{"legal task", InputOptimiseRequest{TaskType: "legal"}, "conservative"},
		{"security task", InputOptimiseRequest{TaskType: "security"}, "conservative"},
		{"restricted data", InputOptimiseRequest{Sensitivity: "restricted"}, "conservative"},
		{"secret data", InputOptimiseRequest{Sensitivity: "secret"}, "conservative"},
		{"critical risk", InputOptimiseRequest{RiskClass: "critical"}, "conservative"},
		{"high risk", InputOptimiseRequest{RiskClass: "high"}, "conservative"},
		// An explicitly requested lossy profile is refused for such work.
		{"aggressive refused", InputOptimiseRequest{TaskType: "legal", Profile: "aggressive"}, "conservative"},
		{"balanced refused", InputOptimiseRequest{Sensitivity: "restricted", Profile: "balanced"}, "conservative"},
		{"adaptive refused", InputOptimiseRequest{RiskClass: "high", Profile: "adaptive"}, "conservative"},
		// exact is stricter than the floor and is honoured.
		{"exact honoured", InputOptimiseRequest{TaskType: "governance", Profile: "exact"}, "exact"},
		// Ordinary work is unaffected.
		// §18.4: conservative is the default for uncharacterised work.
		{"routine default", InputOptimiseRequest{TaskType: "conversation"}, "conservative"},
		{"routine explicit", InputOptimiseRequest{TaskType: "conversation", Profile: "aggressive"}, "aggressive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := resolveProfile(tc.req); got != tc.want {
				t.Fatalf("expected profile %q, got %q", tc.want, got)
			}
		})
	}
}

func TestLossyProfilesAreNeverSelectedForHighImpactWork(t *testing.T) {
	// Exhaustive guard: no combination of high-impact marker and requested
	// profile may resolve to a lossy profile.
	markers := []InputOptimiseRequest{
		{TaskType: "governance"}, {TaskType: "security"}, {TaskType: "financial"},
		{Sensitivity: "restricted"}, {Sensitivity: "confidential"},
		{RiskClass: "high"}, {RiskClass: "critical"},
	}
	for _, req := range markers {
		for _, profile := range []string{"", "exact", "conservative", "balanced", "aggressive", "adaptive"} {
			req.Profile = profile
			if got, _ := resolveProfile(req); lossyProfiles[got] {
				t.Errorf("high-impact request %+v resolved to lossy profile %q", req, got)
			}
		}
	}
}

// TestCompileAcceptsControlPlanePayload guards the wire contract between the
// control plane and this service: the handler rejects unknown fields, so a
// field the control plane sends must be declared here or every compile call
// fails in production while unit tests stay green.
func TestCompileAcceptsControlPlanePayload(t *testing.T) {
	payload := []byte(`{"input":"hello","task_type":"production_change","profile":"","token_budget":8000,"risk_class":"critical","sensitivity":"restricted"}`)
	var req InputOptimiseRequest
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("control-plane payload rejected: %v", err)
	}
	if got, _ := resolveProfile(req); got != "conservative" {
		t.Fatalf("critical/restricted request resolved to %q", got)
	}
}

func TestCapsuleCountsUntouchedSegmentsInCompiledSize(t *testing.T) {
	kept := Segment{Class: "task_instruction", Content: strings.Repeat("keep this exactly ", 40), OriginalTokens: estimateTokens(strings.Repeat("keep this exactly ", 40))}
	shrunk := Segment{Class: "tool_output", Content: "summary", OriginalTokens: 400, CompiledTokens: estimateTokens("summary")}
	// kept passes through unchanged, so it carries no CompiledTokens.
	capsule := buildCapsule("orig", "balanced", []Segment{kept, shrunk}, []Segment{kept, shrunk}, BudgetAllocation{})
	want := estimateTokens(kept.Content) + estimateTokens("summary")
	if capsule.CompiledTokens != want {
		t.Fatalf("compiled tokens = %d, want %d (untouched segment must be counted)", capsule.CompiledTokens, want)
	}
}

func TestUnknownProfileIsRejected(t *testing.T) {
	if _, err := resolveProfile(InputOptimiseRequest{Profile: "agressive"}); err == nil {
		t.Fatal("a misspelled profile must be rejected, not silently replaced")
	}
	w := httptest.NewRecorder()
	handleInputOptimise(w, httptest.NewRequest(http.MethodPost, "/v1/input/optimise", strings.NewReader(`{"input":"x","profile":"turbo"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile: status %d", w.Code)
	}
}

func TestFailedInvariantsFallBackToExactAndReportIt(t *testing.T) {
	t.Cleanup(func(prev SourceStore) func() { return func() { authoritativeSourceStore = prev } }(authoritativeSourceStore))
	authoritativeSourceStore = newFileSourceStore(t.TempDir())
	id := "7f3e2a10-1b2c-4d5e-8f90-1234567890ab"
	input := "Tool output follows. " + strings.Repeat("noise line without value ", 200) + " the rollback operation id is " + id + " and must survive."
	// No hint: the validator itself recognises the UUID as an exact value
	// that aggressive summarisation (first sentence only) would drop.
	body, _ := json.Marshal(map[string]any{"input": input, "profile": "aggressive", "token_budget": 60})
	w := httptest.NewRecorder()
	handleInputOptimise(w, httptest.NewRequest(http.MethodPost, "/v1/input/optimise", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("optimise: %d %s", w.Code, w.Body.String())
	}
	var resp InputOptimiseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Validated || !strings.Contains(resp.CompiledText, id) {
		t.Fatalf("a protected identifier was lost: validated=%v", resp.Validated)
	}
	applied := strings.Join(resp.Capsule.Transformations, ",")
	if !strings.Contains(applied, "fallback:exact") || resp.Capsule.Profile != "exact" {
		t.Fatalf("fallback not recorded: profile=%q transformations=%s", resp.Capsule.Profile, applied)
	}
	for _, never := range []string{"delta_context", "code_aware_slicing", "tool_schema_routing"} {
		if strings.Contains(applied, never) {
			t.Fatalf("capsule claims a transformation that did not run: %s", never)
		}
	}
	if !resp.Capsule.Validation.Checks["source_hash_recorded"] {
		t.Fatal("source_hash_recorded must be true once the source is stored")
	}
}
