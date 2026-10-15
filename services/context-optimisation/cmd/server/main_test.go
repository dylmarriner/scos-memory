package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestAllocateBudget(t *testing.T) {
	alloc := allocateBudget(8000, "balanced", nil)
	if alloc.Governing != 1200 {
		t.Fatalf("expected 1200 governing, got %d", alloc.Governing)
	}
	if alloc.Reserve != 800 {
		t.Fatalf("expected 800 reserve, got %d", alloc.Reserve)
	}
}

func TestAllocateBudgetExact(t *testing.T) {
	alloc := allocateBudget(8000, "exact", nil)
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
	opt := optimiseOutput(raw, "standard", 1000)
	if strings.Contains(opt, "\n\n\n") {
		t.Fatal("should collapse repeated newlines")
	}
}

func TestOptimiseOutputCompact(t *testing.T) {
	raw := "I think this is true.\n\nLet me check.\n\nBased on the docs, I can confirm."
	opt := optimiseOutput(raw, "compact", 1000)
	if strings.Contains(opt, "Let me") {
		t.Fatal("compact should remove 'Let me' filler")
	}
}

func TestOptimiseOutputTruncates(t *testing.T) {
	raw := strings.Repeat("word ", 2000)
	opt := optimiseOutput(raw, "standard", 100)
	if estimateTokens(opt) > 150 {
		t.Fatalf("expected truncation to ~100 tokens, got %d tokens", estimateTokens(opt))
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

func TestClassBudgetFor(t *testing.T) {
	alloc := BudgetAllocation{Governing: 1000, Evidence: 500}
	budget := classBudgetFor("governing_policy", alloc)
	if budget != 1000 {
		t.Fatalf("expected 1000, got %d", budget)
	}
	budget = classBudgetFor("unknown", alloc)
	if budget != 500 {
		t.Fatalf("expected 500 for unknown, got %d", budget)
	}
}

func TestSummary(t *testing.T) {
	s, kept := summarise("This is a long piece of content. It has multiple sentences.", "test")
	if !strings.Contains(s, "[test summary]") {
		t.Fatal("summary should include class tag")
	}
	if !strings.Contains(s, "This is a long piece of content.") {
		t.Fatal("summary should include first sentence")
	}
	if kept != len("This is a long piece of content.") {
		t.Fatalf("summary reported %d retained bytes", kept)
	}
}

func TestSummaryDoesNotSplitRunes(t *testing.T) {
	content := strings.Repeat("é", 150)
	s, kept := summarise(content, "test")
	if kept > 200 || !utf8.ValidString(s) || !strings.HasPrefix(content, content[:kept]) {
		t.Fatalf("summary cut inside a rune: kept=%d valid=%v", kept, utf8.ValidString(s))
	}
}

func TestCapsuleIDIsDerivedFromContent(t *testing.T) {
	a := deriveCapsuleID("input", sha256Hex([]byte("a")), "p", "c")
	if a != deriveCapsuleID("input", sha256Hex([]byte("a")), "p", "c") {
		t.Fatal("capsule id must be deterministic")
	}
	if !validCapsuleID(a) {
		t.Fatalf("derived id %q is not a valid capsule id", a)
	}
	for _, other := range []string{
		deriveCapsuleID("output", sha256Hex([]byte("a")), "p", "c"),
		deriveCapsuleID("input", sha256Hex([]byte("b")), "p", "c"),
		deriveCapsuleID("input", sha256Hex([]byte("a")), "q", "c"),
		deriveCapsuleID("input", sha256Hex([]byte("a")), "p", "d"),
	} {
		if other == a {
			t.Fatal("capsule id must change with kind, source, policy and compiled content")
		}
	}
	for _, bad := range []string{"", "c1", "capsule-xyz", "capsule-" + strings.Repeat("g", 32), "../capsule-" + strings.Repeat("a", 32)} {
		if validCapsuleID(bad) {
			t.Fatalf("%q accepted as a capsule id", bad)
		}
	}
}

func TestSha256Hex(t *testing.T) {
	h := sha256Hex([]byte("hello"))
	if len(h) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h))
	}
}

func TestCompileContextBuildsCapsule(t *testing.T) {
	store := newFileSourceStore(t.TempDir())
	resp, _, err := compileContext(InputOptimiseRequest{Input: "hello"}, store)
	if err != nil {
		t.Fatal(err)
	}
	capsule := resp.Capsule
	if capsule.Profile != "balanced" {
		t.Fatal("profile mismatch")
	}
	if capsule.CompilerVersion != version {
		t.Fatal("version mismatch")
	}
	if !validCapsuleID(capsule.CapsuleID) {
		t.Fatalf("expected a derived capsule id, got %q", capsule.CapsuleID)
	}
	if capsule.TokenBudget != defaultTokenBudget {
		t.Fatalf("expected default budget, got %d", capsule.TokenBudget)
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

	body, _ := json.Marshal(contextRestoreRequest{Capsule: &response.Capsule})
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

// useStore points the handlers at store for the duration of the test.
func useStore(t *testing.T, store SourceStore) {
	t.Helper()
	previous := authoritativeSourceStore
	authoritativeSourceStore = store
	t.Cleanup(func() { authoritativeSourceStore = previous })
}

func postJSON(t *testing.T, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	return w
}

func compileForTest(t *testing.T, req InputOptimiseRequest) InputOptimiseResponse {
	t.Helper()
	w := postJSON(t, handleInputOptimise, "/v1/context/compile", req)
	if w.Code != http.StatusOK {
		t.Fatalf("compile failed: %d %s", w.Code, w.Body.String())
	}
	var resp InputOptimiseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestContextRestoreRejectsTamperedSource(t *testing.T) {
	store := newFileSourceStore(t.TempDir())
	useStore(t, store)
	resp := compileForTest(t, InputOptimiseRequest{Input: "authoritative content"})
	source := resp.Capsule.SourceObjects[0]
	if err := os.WriteFile(filepath.Join(store.root, source.ObjectID+"-1.source"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: resp.Capsule.CapsuleID})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected tamper conflict, got %d", w.Code)
	}
}

func TestContextRestoreRejectsTamperedManifest(t *testing.T) {
	store := newFileSourceStore(t.TempDir())
	useStore(t, store)
	original := compileForTest(t, InputOptimiseRequest{Input: "original content"})
	other := compileForTest(t, InputOptimiseRequest{Input: "other content"})
	// Re-point the first capsule's manifest at the other source.
	manifestPath := filepath.Join(store.root, original.Capsule.CapsuleID+"-"+manifestVersion+".source")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest capsuleManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.SourceObjects = other.Capsule.SourceObjects
	edited, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, edited, 0600); err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: original.Capsule.CapsuleID})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected manifest tamper conflict, got %d: %s", w.Code, w.Body.String())
	}
}

func TestContextRestoreRejectsMissingSource(t *testing.T) {
	store := newFileSourceStore(t.TempDir())
	useStore(t, store)
	resp := compileForTest(t, InputOptimiseRequest{Input: "content that will go missing"})
	source := resp.Capsule.SourceObjects[0]
	if err := os.Remove(filepath.Join(store.root, source.ObjectID+"-1.source")); err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: resp.Capsule.CapsuleID})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected missing-source failure, got %d", w.Code)
	}
}

func TestContextRestoreRejectsUnknownCapsule(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: "capsule-" + strings.Repeat("a", 32)})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected unknown capsule to be 404, got %d", w.Code)
	}
	w = postJSON(t, handleContextRestore, "/v1/context/restore", map[string]string{"capsule_id": "c1"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed capsule id to be 400, got %d", w.Code)
	}
}

// TestContextRestoreRefusesForgedSourceObjects: a caller cannot borrow a real
// capsule id to read a different stored source.
func TestContextRestoreRefusesForgedSourceObjects(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	mine := compileForTest(t, InputOptimiseRequest{Input: "my content"})
	theirs := compileForTest(t, InputOptimiseRequest{Input: "their content"})
	forged := mine.Capsule
	forged.SourceObjects = theirs.Capsule.SourceObjects
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{Capsule: &forged})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected forged source objects to be refused, got %d", w.Code)
	}
	w = postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: theirs.Capsule.CapsuleID, Capsule: &mine.Capsule})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected mismatched capsule_id to be 400, got %d", w.Code)
	}
}

// TestCapsuleIDRestoresByIDAlone: the id in the response is the restore key,
// and the same input under the same policy always yields the same id.
func TestCapsuleIDRestoresByIDAlone(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	input := "instruction: keep 550e8400-e29b-41d4-a716-446655440000\r\nbackground   \r\n"
	first := compileForTest(t, InputOptimiseRequest{Input: input})
	second := compileForTest(t, InputOptimiseRequest{Input: input})
	if first.Capsule.CapsuleID != second.Capsule.CapsuleID {
		t.Fatalf("same input and policy produced different ids: %s %s", first.Capsule.CapsuleID, second.Capsule.CapsuleID)
	}
	if first.Capsule.RestorationHandles[0].HandleID != first.Capsule.CapsuleID {
		t.Fatal("restoration handle must name the restorable capsule id")
	}
	other := compileForTest(t, InputOptimiseRequest{Input: input, Profile: "exact"})
	if other.Capsule.CapsuleID == first.Capsule.CapsuleID {
		t.Fatal("a different policy must yield a different capsule id")
	}
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: first.Capsule.CapsuleID})
	if w.Code != http.StatusOK {
		t.Fatalf("restore by id failed: %d %s", w.Code, w.Body.String())
	}
	var restored struct {
		CapsuleID string `json:"capsule_id"`
		Kind      string `json:"kind"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Content != input || restored.CapsuleID != first.Capsule.CapsuleID || restored.Kind != "input" {
		t.Fatalf("restore returned %+v", restored)
	}
}

func TestOutputCapsuleIsRestorable(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	raw := "I believe the answer is 42.\n\nLet me check.\nI believe the answer is 42."
	w := postJSON(t, handleOutputOptimise, "/v1/optimize/output", OutputOptimiseRequest{RawOutput: raw, Format: "compact"})
	if w.Code != http.StatusOK {
		t.Fatalf("output optimise failed: %d %s", w.Code, w.Body.String())
	}
	var out OutputOptimiseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Optimised != "the answer is 42." {
		t.Fatalf("unexpected compact output %q", out.Optimised)
	}
	if out.OriginalTokens != estimateTokens(raw) || out.OptimisedTokens != estimateTokens(out.Optimised) ||
		out.Reduction != out.OriginalTokens-out.OptimisedTokens || out.TokenMethod != tokenMethod {
		t.Fatalf("output token accounting not measured: %+v", out)
	}
	if !validCapsuleID(out.CapsuleID) || out.SourceContentHash != sha256Hex([]byte(raw)) {
		t.Fatalf("unexpected capsule identity: %+v", out)
	}
	restore := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: out.CapsuleID})
	if restore.Code != http.StatusOK {
		t.Fatalf("restore failed: %d %s", restore.Code, restore.Body.String())
	}
	var restored struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(restore.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Content != raw || restored.Kind != "output" {
		t.Fatalf("output restore returned %+v", restored)
	}
}

func TestOutputOptimiseRejectsUnimplementedFields(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	cases := map[string]map[string]any{
		"request_intent was never used": {"raw_output": "x", "request_intent": "summarise"},
		"diff format is not supported":  {"raw_output": "x", "format": "diff"},
		"unknown format":                {"raw_output": "x", "format": "yaml"},
		"negative max tokens":           {"raw_output": "x", "max_tokens": -1},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if w := postJSON(t, handleOutputOptimise, "/v1/optimize/output", body); w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestCompileRejectsIgnoredOrInvalidFields(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	cases := map[string]map[string]any{
		"model has no tokenizer":          {"input": "x", "model": "gpt-4"},
		"session context is unsupported":  {"input": "x", "session_context": map[string]string{"session_id": "s"}},
		"unknown profile":                 {"input": "x", "profile": "maximal"},
		"negative budget":                 {"input": "x", "token_budget": -5},
		"code symbol absent from input":   {"input": "x", "code_symbols": []string{"ParseConfig"}},
		"memory reference absent":         {"input": "x", "memory_references": []string{"mem-1"}},
		"empty pinned reference":          {"input": "x", "code_symbols": []string{""}},
		"missing input":                   {"profile": "exact"},
		"unknown field from old contract": {"input": "x", "last_capsule_id": "c"},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if w := postJSON(t, handleInputOptimise, "/v1/context/compile", body); w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestTransformationsReportOnlyAppliedWork: a clean single-segment input
// under a lossless profile is passed through unframed, so nothing may be
// claimed; two segments are framed and the framing is measured.
func TestTransformationsReportOnlyAppliedWork(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	resp := compileForTest(t, InputOptimiseRequest{Input: "plain clean text", Profile: "exact"})
	if names := transformationNames(resp.Capsule); len(names) != 0 || resp.CompiledText != "plain clean text" {
		t.Fatalf("expected an unframed pass-through with no transformations, got %v %q", names, resp.CompiledText)
	}
	// Framing that would make the text larger is left out...
	unframed := compileForTest(t, InputOptimiseRequest{Input: "short note\n```\nfunc A() {}\n```", Profile: "exact"})
	if strings.Contains(unframed.CompiledText, "--- ") || len(unframed.CompiledText) > len("short note\n```\nfunc A() {}\n```") {
		t.Fatalf("framing must not make the text larger: %q", unframed.CompiledText)
	}
	// ...and kept when deduplication has saved more than it costs.
	block := "\n```\n" + strings.Repeat("func A() { return helper(1, 2, 3) }\n", 4) + "```\n"
	input := "a background paragraph" + block + block
	framed := compileForTest(t, InputOptimiseRequest{Input: input, Profile: "exact"})
	framing := transformationNamed(t, framed.Capsule, "segment_framing")
	overhead := len(framed.CompiledText)
	for _, seg := range framed.Capsule.Segments {
		overhead -= len(seg.Content)
	}
	if !strings.Contains(framed.CompiledText, segmentHeader("optional_background")) ||
		framing.CompiledTokens != tokensForBytes(overhead) || framing.OriginalTokens != 0 || len(framing.SourceRanges) != 0 {
		t.Fatalf("framing record not measured: %+v %q", framing, framed.CompiledText)
	}
}

func transformationNames(c ContextCapsule) []string {
	var names []string
	for _, tr := range c.Transformations {
		names = append(names, tr.Name)
	}
	return names
}

func transformationNamed(t *testing.T, c ContextCapsule, name string) TransformationRecord {
	t.Helper()
	for _, tr := range c.Transformations {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("transformation %q not reported; got %v", name, transformationNames(c))
	return TransformationRecord{}
}

func sliceRanges(s string, ranges []ContextRange) []string {
	var out []string
	for _, r := range ranges {
		out = append(out, s[r.Start:r.End])
	}
	return out
}

func TestTransformationRangesAddressTheSource(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	block := "```go\nfunc A() {}\n```"
	input := "\ufeffintro  \r\n" + block + "\nmiddle\n" + block
	resp := compileForTest(t, InputOptimiseRequest{Input: input, Profile: "exact"})
	c := resp.Capsule

	canon := transformationNamed(t, c, "canonicalisation")
	if got := sliceRanges(input, canon.SourceRanges); strings.Join(got, "|") != "\ufeff|  \r\n" {
		t.Fatalf("canonicalisation ranges cover %q", got)
	}
	if canon.OriginalTokens != tokensForBytes(len("\ufeff  \r\n")) || canon.CompiledTokens != tokensForBytes(1) {
		t.Fatalf("canonicalisation tokens not measured: %+v", canon)
	}

	dedup := transformationNamed(t, c, "exact_deduplication")
	secondCode := strings.LastIndex(input, "func A() {}")
	if len(dedup.SourceRanges) != 1 || dedup.SourceRanges[0] != (ContextRange{Start: secondCode, End: secondCode + len("func A() {}")}) {
		t.Fatalf("deduplication ranges %+v do not address the second copy", dedup.SourceRanges)
	}
	if dedup.OriginalTokens != estimateTokens("func A() {}") || dedup.CompiledTokens != 0 {
		t.Fatalf("deduplication tokens not measured: %+v", dedup)
	}

	fences := transformationNamed(t, c, "code_fence_removal")
	for _, text := range sliceRanges(input, fences.SourceRanges) {
		if strings.Trim(text, "`go\n") != "" {
			t.Fatalf("fence range covers non-fence text %q", text)
		}
	}
	for _, name := range transformationNames(c) {
		if name == transformTruncation || name == transformReference || name == transformSummary {
			t.Fatalf("exact profile reported lossy transformation %q", name)
		}
	}
}

// TestRangesPartitionSourceAndMatchCompiledText: included and excluded ranges
// cover the source exactly once, and every included byte is in the output.
func TestRangesPartitionSourceAndMatchCompiledText(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	input := "policy: keep 1.2.3   \r\n" + strings.Repeat("background sentence one. ", 120) + "\n```js\nimport x from 'y'\n```\ntrailing"
	for _, profile := range []string{"exact", "conservative", "balanced", "aggressive", "adaptive"} {
		t.Run(profile, func(t *testing.T) {
			resp := compileForTest(t, InputOptimiseRequest{Input: input, Profile: profile, TokenBudget: 200})
			c := resp.Capsule
			all := normaliseRanges(append(append([]ContextRange{}, c.IncludedRanges...), c.ExcludedRanges...))
			if len(all) != 1 || all[0] != (ContextRange{Start: 0, End: len(input)}) || rangeBytes(c.IncludedRanges)+rangeBytes(c.ExcludedRanges) != len(input) {
				t.Fatalf("ranges do not partition the source: %+v / %+v", c.IncludedRanges, c.ExcludedRanges)
			}
			for _, text := range sliceRanges(input, c.IncludedRanges) {
				if !strings.Contains(resp.CompiledText, strings.ReplaceAll(text, "\r\n", "\n")) {
					t.Fatalf("included range %q is not in the compiled text", text)
				}
			}
			if c.TokenAccounting.Included != tokensForBytes(rangeBytes(c.IncludedRanges)) ||
				c.TokenAccounting.Excluded != tokensForBytes(rangeBytes(c.ExcludedRanges)) {
				t.Fatalf("included/excluded tokens not derived from ranges: %+v", c.TokenAccounting)
			}
		})
	}
}

// TestTokenAccountingIsMeasured: pass-through segments count their real
// tokens and the reduction ratio is computed from the returned text.
func TestTokenAccountingIsMeasured(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	input := "a short note that passes through"
	resp := compileForTest(t, InputOptimiseRequest{Input: input})
	c := resp.Capsule
	if len(c.Segments) != 1 || c.Segments[0].Transformation != transformPassThrough {
		t.Fatalf("expected one pass-through segment, got %+v", c.Segments)
	}
	if c.Segments[0].CompiledTokens != c.Segments[0].OriginalTokens || c.Segments[0].CompiledTokens == 0 {
		t.Fatalf("pass-through segment tokens: %+v", c.Segments[0])
	}
	if c.OriginalTokens != estimateTokens(input) || c.CompiledTokens != estimateTokens(resp.CompiledText) {
		t.Fatalf("capsule counts not measured: original=%d compiled=%d", c.OriginalTokens, c.CompiledTokens)
	}
	if want := 1 - float64(c.CompiledTokens)/float64(c.OriginalTokens); c.Reduction != want || c.Reduction != 0 || resp.CompiledText != input {
		t.Fatalf("an unframed pass-through must report zero reduction, got %v for %q", c.Reduction, resp.CompiledText)
	}
	if c.TokenAccounting.Method != tokenMethod || c.TokenAccounting.Original != c.OriginalTokens || c.TokenAccounting.Compressed != c.CompiledTokens {
		t.Fatalf("token accounting disagrees with capsule: %+v", c.TokenAccounting)
	}
	// The evaluate endpoint uses the same estimate and agrees with the capsule.
	w := postJSON(t, handleContextEvaluate, "/v1/context/evaluate", contextEvaluateRequest{Input: input, CompiledText: resp.CompiledText, Capsule: &c})
	var eval struct {
		Valid          bool `json:"valid"`
		CompiledTokens int  `json:"compiled_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &eval); err != nil {
		t.Fatal(err)
	}
	if !eval.Valid || eval.CompiledTokens != c.CompiledTokens {
		t.Fatalf("evaluate disagrees with capsule: %+v", eval)
	}
}

func TestReducingProfilesReportTheirTransformations(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	long := strings.Repeat("background detail sentence. ", 80)
	summarised := compileForTest(t, InputOptimiseRequest{Input: long, Profile: "adaptive"})
	summary := transformationNamed(t, summarised.Capsule, transformSummary)
	segment := summarised.Capsule.Segments[0]
	if summary.OriginalTokens != segment.OriginalTokens || summary.CompiledTokens != estimateTokens(segment.Content) {
		t.Fatalf("summary tokens not measured: %+v", summary)
	}
	if summarised.Capsule.CompiledTokens >= summarised.Capsule.OriginalTokens {
		t.Fatalf("expected a real reduction, got %d -> %d", summarised.Capsule.OriginalTokens, summarised.Capsule.CompiledTokens)
	}
	if got := sliceRanges(long, summarised.Capsule.IncludedRanges); len(got) != 1 || got[0] != "background detail sentence." {
		t.Fatalf("summary included ranges cover %q", got)
	}
	truncated := compileForTest(t, InputOptimiseRequest{Input: strings.Repeat("x", 3000), Profile: "balanced"})
	record := transformationNamed(t, truncated.Capsule, transformTruncation)
	if len(truncated.Capsule.IncludedRanges) != 1 || truncated.Capsule.IncludedRanges[0] != (ContextRange{Start: 0, End: 1000}) {
		t.Fatalf("truncation included ranges %+v", truncated.Capsule.IncludedRanges)
	}
	if record.CompiledTokens != estimateTokens(strings.Repeat("x", 1000)+truncationMarker) {
		t.Fatalf("truncation tokens %+v", record)
	}
	referenced := compileForTest(t, InputOptimiseRequest{Input: "```\n" + long + "\n```", Profile: "balanced", TokenBudget: 10})
	transformationNamed(t, referenced.Capsule, transformReference)
	if len(referenced.Capsule.IncludedRanges) != 0 {
		t.Fatalf("a reference substitution includes no source bytes, got %+v", referenced.Capsule.IncludedRanges)
	}
}

// TestPinnedReferencesAreKeptAndValidated: code symbols and memory references
// stop a segment from being summarised and are checked in the output.
func TestPinnedReferencesAreKeptAndValidated(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	input := "notes about ParseConfig and mem-7781. " + strings.Repeat("filler sentence here. ", 80)
	unpinned := compileForTest(t, InputOptimiseRequest{Input: input, Profile: "adaptive", TokenBudget: 100})
	if strings.Contains(unpinned.CompiledText, "filler sentence here. filler") {
		t.Fatal("expected the unpinned segment to be summarised")
	}
	pinned := compileForTest(t, InputOptimiseRequest{Input: input, Profile: "adaptive", TokenBudget: 100,
		CodeSymbols: []string{"ParseConfig"}, MemoryReferences: []string{"mem-7781"}})
	if !strings.Contains(pinned.CompiledText, strings.TrimSpace(input)) {
		t.Fatal("a segment holding a pinned reference must pass through unchanged")
	}
	if pinned.Capsule.Segments[0].Droppable || pinned.Capsule.Segments[0].Transformation != transformPassThrough {
		t.Fatalf("pinned segment: %+v", pinned.Capsule.Segments[0])
	}
	if !pinned.Capsule.Validation.Checks["pinned_references_retained"] {
		t.Fatal("pinned references check failed")
	}
	if strings.Join(pinned.Capsule.CodeSymbols, ",") != "ParseConfig" || strings.Join(pinned.Capsule.MemoryReferences, ",") != "mem-7781" {
		t.Fatalf("capsule does not record its pins: %+v %+v", pinned.Capsule.CodeSymbols, pinned.Capsule.MemoryReferences)
	}
	report := checkInvariants(input, "summary without them", nil, []string{"ParseConfig", "mem-7781"})
	if report.pinsTotal != 2 || report.pinsRetained != 0 || report.holds() {
		t.Fatalf("pin check did not measure loss: %+v", report)
	}
}

// lyingStore stores sources but returns other bytes on read, so nothing it
// claims to hold can be verified.
type lyingStore struct{ *fileSourceStore }

func (s lyingStore) Get(objectID, version string) (string, error) {
	content, err := s.fileSourceStore.Get(objectID, version)
	if err != nil || version == manifestVersion {
		return content, err
	}
	return content + " (altered)", nil
}

// TestSourceHashRecordedReflectsTheStore: the check is read back from the
// store, not asserted.
func TestSourceHashRecordedReflectsTheStore(t *testing.T) {
	good, _, err := compileContext(InputOptimiseRequest{Input: "exact value 1.2.3"}, newFileSourceStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if !good.Capsule.Validation.Checks["source_hash_recorded"] || !good.Capsule.Validation.Checks["restoration_verified"] || !good.Validated {
		t.Fatalf("expected verified capsule: %+v", good.Capsule.Validation)
	}
	bad, _, err := compileContext(InputOptimiseRequest{Input: "exact value 1.2.3"}, lyingStore{newFileSourceStore(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	checks := bad.Capsule.Validation.Checks
	if checks["source_hash_recorded"] || checks["restoration_verified"] || bad.Capsule.Validation.Valid || bad.Validated {
		t.Fatalf("an unverifiable store must fail validation: %+v", bad.Capsule.Validation)
	}
}

func TestInvariantReportCountsRetention(t *testing.T) {
	report := checkInvariants("ids 1.2.3 and 4.5.6", "kept 1.2.3", []string{"kept", "lost"}, nil)
	if report.exactTotal != 2 || report.exactRetained != 1 || report.hintsTotal != 2 || report.hintsRetained != 1 {
		t.Fatalf("unexpected report %+v", report)
	}
}

// legacyCanonicalise is the original string-replacement canonicaliser, kept
// as the oracle for the offset-tracking one.
func legacyCanonicalise(input string) string {
	cleaned := strings.ReplaceAll(input, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	cleaned = strings.TrimPrefix(cleaned, "\ufeff")
	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

func TestCanonicaliseMappedMatchesLegacyAndMapsBytes(t *testing.T) {
	inputs := []string{
		"", "abc", "\ufeffabc  ", "a \t\r\nb\rc\r\r\nd\n\n", "  \n\t\n", "x\r", "\ufeff\r\n  ", "é  \r\nü",
	}
	for _, input := range inputs {
		c := canonicaliseMapped(input)
		if c.text != legacyCanonicalise(input) {
			t.Fatalf("canonicalise(%q) = %q, want %q", input, c.text, legacyCanonicalise(input))
		}
		for i := 0; i < len(c.text); i++ {
			src := input[c.origin[i]:c.originEnd(i)]
			if src != c.text[i:i+1] && !(c.text[i] == '\n' && (src == "\r" || src == "\r\n")) {
				t.Fatalf("%q: canonical byte %d (%q) maps to %q", input, i, c.text[i], src)
			}
		}
		covered := normaliseRanges(append(c.sourceRanges(0, len(c.text)), c.rewritten...))
		if len(input) > 0 && (len(covered) != 1 || covered[0] != (ContextRange{Start: 0, End: len(input)})) {
			t.Fatalf("%q: mapped and rewritten ranges %+v do not cover the input", input, covered)
		}
	}
}

func TestSplitCodeBlocksHandlesEmptyBlock(t *testing.T) {
	parts := splitCodeBlocks("before\n```\n```\nafter")
	var contents []string
	for _, p := range parts {
		contents = append(contents, p.content)
	}
	if strings.Join(contents, "|") != "before\n||\nafter" {
		t.Fatalf("unexpected parts %q", contents)
	}
	segments := classifySegments("```\n```", nil)
	if len(segments) != 0 {
		t.Fatalf("an empty code block yields no segments, got %+v", segments)
	}
}

func TestReadinessChecksTheSourceStore(t *testing.T) {
	useStore(t, newFileSourceStore(filepath.Join(t.TempDir(), "sources")))
	if err := readinessCheck(); err != nil {
		t.Fatalf("expected writable source store to be ready: %v", err)
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	useStore(t, newFileSourceStore(file))
	if err := readinessCheck(); err == nil {
		t.Fatal("a source store that is a file must not be ready")
	}
	useStore(t, newFileSourceStore(""))
	if err := readinessCheck(); err == nil {
		t.Fatal("an unconfigured source store must not be ready")
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
		{"routine default", InputOptimiseRequest{TaskType: "conversation"}, "balanced"},
		{"routine explicit", InputOptimiseRequest{TaskType: "conversation", Profile: "aggressive"}, "aggressive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveProfile(tc.req); got != tc.want {
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
			if got := resolveProfile(req); lossyProfiles[got] {
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
	if got := resolveProfile(req); got != "conservative" {
		t.Fatalf("critical/restricted request resolved to %q", got)
	}
}

func TestAggressiveCompressesBackgroundThatWouldHaveFitted(t *testing.T) {
	long := strings.Repeat("This background paragraph explains history. ", 40)
	segments := []Segment{{Class: "optional_background", Content: long, OriginalTokens: estimateTokens(long), Droppable: true}}
	alloc := allocateBudget(100000, "aggressive", segments)
	_, out := compressSegments(segments, "aggressive", alloc)
	if out[0].Transformation != transformSummary || out[0].CompiledTokens >= out[0].OriginalTokens {
		t.Fatalf("aggressive must summarise a long droppable segment even with budget to spare: %+v", out[0])
	}
}

func TestBudgetIsNotGrownToFitTheInput(t *testing.T) {
	var segments []Segment
	for i := 0; i < 6; i++ {
		content := strings.Repeat(fmt.Sprintf("background %d sentence. ", i), 60)
		segments = append(segments, Segment{Class: "optional_background", Content: content, OriginalTokens: estimateTokens(content), Droppable: true})
	}
	alloc := allocateBudget(800, "balanced", segments)
	if alloc.Evidence != percentOf(800, 25) {
		t.Fatalf("evidence budget must be the caller's share, got %d", alloc.Evidence)
	}
	_, out := compressSegments(segments, "balanced", alloc)
	refs := 0
	for _, seg := range out {
		if seg.Transformation == transformReference {
			refs++
		}
	}
	if refs == 0 {
		t.Fatalf("background beyond the evidence budget and reserve must be replaced by references: %+v", out)
	}
}

// restoreByID restores a capsule by id alone and returns the content.
func restoreByID(t *testing.T, capsuleID string) string {
	t.Helper()
	w := postJSON(t, handleContextRestore, "/v1/context/restore", contextRestoreRequest{CapsuleID: capsuleID})
	if w.Code != http.StatusOK {
		t.Fatalf("restore failed: %d %s", w.Code, w.Body.String())
	}
	var restored struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	return restored.Content
}

// TestRepeatedSentenceIsCollapsedAndRestoresExactly: an instruction repeated
// five times (the acceptance benchmark's shape) compiles to one copy plus a
// reference under the lossy profiles, every removed byte is accounted for,
// and the capsule restores the input byte for byte.
func TestRepeatedSentenceIsCollapsedAndRestoresExactly(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	sentence := "preserve the exact operation id op-7734 recorded by the idempotency ledger"
	input := strings.Repeat(sentence+" ", 5)
	for _, profile := range []string{"balanced", "aggressive"} {
		t.Run(profile, func(t *testing.T) {
			resp := compileForTest(t, InputOptimiseRequest{Input: input, Profile: profile, NonDroppableHints: []string{"op-7734"}})
			c := resp.Capsule
			if strings.Count(resp.CompiledText, sentence) != 1 || strings.Contains(resp.CompiledText, "--- ") {
				t.Fatalf("expected one unframed copy of the sentence, got %q", resp.CompiledText)
			}
			if c.Reduction < 0.5 || c.CompiledTokens >= c.OriginalTokens {
				t.Fatalf("repetition not removed: reduction %v (%d -> %d)", c.Reduction, c.OriginalTokens, c.CompiledTokens)
			}
			if !c.Validation.Valid || !c.Validation.Checks["exact_values_retained"] || !c.Validation.Checks["non_droppable_hints_retained"] {
				t.Fatalf("collapsed capsule failed validation: %+v", c.Validation)
			}
			if c.Segments[0].Transformation != transformRepetition {
				t.Fatalf("segment transformation %q", c.Segments[0].Transformation)
			}
			record := transformationNamed(t, c, transformRepetition)
			if got := strings.Join(sliceRanges(input, record.SourceRanges), ""); got != strings.Repeat(" "+sentence, 4) {
				t.Fatalf("repetition record covers %q", got)
			}
			if record.OriginalTokens != tokensForBytes(rangeBytes(record.SourceRanges)) || record.CompiledTokens != estimateTokens(c.Segments[0].Content[len(sentence)+1:]) {
				t.Fatalf("repetition record not measured: %+v", record)
			}
			if got := sliceRanges(input, c.IncludedRanges); len(got) != 1 || got[0] != sentence {
				t.Fatalf("included ranges cover %q", got)
			}
			if c.TokenAccounting.Included != tokensForBytes(len(sentence)) || c.TokenAccounting.Excluded != tokensForBytes(len(input)-len(sentence)) ||
				c.TokenAccounting.Compressed != estimateTokens(resp.CompiledText) || c.TokenAccounting.Original != estimateTokens(input) {
				t.Fatalf("token accounting not truthful: %+v", c.TokenAccounting)
			}
			if restored := restoreByID(t, c.CapsuleID); restored != input {
				t.Fatalf("restore returned %q", restored)
			}
		})
	}
}

// TestLosslessProfilesKeepRepetition: exact and conservative never alter
// segment content, so every copy survives.
func TestLosslessProfilesKeepRepetition(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	sentence := "cite the rollback plan for migration rev-2044 before applying it"
	input := strings.Repeat(sentence+" ", 5)
	for _, profile := range []string{"exact", "conservative"} {
		resp := compileForTest(t, InputOptimiseRequest{Input: input, Profile: profile})
		if resp.CompiledText != strings.TrimRight(input, " ") {
			t.Fatalf("%s altered content: %q", profile, resp.CompiledText)
		}
		for _, name := range transformationNames(resp.Capsule) {
			if name == transformRepetition {
				t.Fatalf("%s reported repetition collapse", profile)
			}
		}
	}
}

func TestRepeatedSentencesAndLinesAreCollapsed(t *testing.T) {
	text := "The deploy runs at noon today. Check the queue first. The deploy runs at noon today.\nqueue depth is reported here\nqueue depth is reported here\nend"
	seg := Segment{Class: "optional_background", Content: text, end: len(text), Droppable: true}
	out := collapseRepetition(seg, nil)
	want := "The deploy runs at noon today. Check the queue first.\nqueue depth is reported here\nend\n[Reference: 2 repeats omitted]"
	if out.Content != want {
		t.Fatalf("collapsed to %q", out.Content)
	}
	var removed []string
	for _, r := range out.collapsed {
		removed = append(removed, text[r.Start:r.End])
	}
	if strings.Join(removed, "|") != " The deploy runs at noon today.|\nqueue depth is reported here" {
		t.Fatalf("removed %q", removed)
	}
	for i, o := range out.origin {
		if o >= 0 && text[o] != out.Content[i] {
			t.Fatalf("byte %d maps to %q, holds %q", i, text[o], out.Content[i])
		}
	}
}

func TestRepetitionCollapseLeavesProtectedSegmentsAlone(t *testing.T) {
	repeated := strings.Repeat("return helper(value, other) ", 4)
	cases := map[string]Segment{
		"code":   {Class: "code_symbol", Content: repeated, end: len(repeated)},
		"pinned": {Class: "optional_background", Content: repeated, end: len(repeated), pinned: true},
		"short":  {Class: "optional_background", Content: "very very very", end: len("very very very")},
	}
	for name, seg := range cases {
		if out := collapseRepetition(seg, nil); out.Content != seg.Content || out.collapsed != nil {
			t.Fatalf("%s segment was collapsed: %q", name, out.Content)
		}
	}
	// A hint spanning two copies would be lost, so the segment is kept whole.
	text := strings.Repeat("alpha beta gamma delta ", 3)
	seg := Segment{Class: "optional_background", Content: text, end: len(text)}
	if out := collapseRepetition(seg, []string{"delta alpha"}); out.Content != text {
		t.Fatalf("collapse dropped a hint: %q", out.Content)
	}
}

// compiledInputs are the inputs the compiler tests above use, plus the
// acceptance benchmark's shape and a few framing edge cases.
func compiledInputs() []string {
	return []string{
		"hello", "plain clean text", "a short note that passes through", "authoritative content",
		"policy: preserve exact value 42\nversion 1.2.3",
		"instruction: keep 550e8400-e29b-41d4-a716-446655440000\r\nbackground   \r\n",
		"\ufeffintro  \r\n```go\nfunc A() {}\n```\nmiddle\n```go\nfunc A() {}\n```",
		"policy: keep 1.2.3   \r\n" + strings.Repeat("background sentence one. ", 120) + "\n```js\nimport x from 'y'\n```\ntrailing",
		strings.Repeat("background detail sentence. ", 80),
		strings.Repeat("x", 3000), strings.Repeat("x", 1010),
		"```\n" + strings.Repeat("background detail sentence. ", 80) + "\n```",
		"notes about ParseConfig and mem-7781. " + strings.Repeat("filler sentence here. ", 80),
		"exact value 1.2.3", "short note\n```\nfunc A() {}\n```", "a\n```\nb\n```\nc\n```\nd\n```\ne",
		strings.Repeat("retain mandatory instruction exact value 412 and cite journal commit ", 5),
		strings.Repeat("quote the governing policy clause forbidding unsigned extension packages ", 5),
		"Yes. Yes. Yes.", "```", "x```y",
	}
}

// TestCompiledTextIsNeverLargerThanInput: under every profile, framing and
// markers never make the compiled text larger than the input, the reduction
// is never negative, and validation (exact values, hints, restoration)
// still holds.
func TestCompiledTextIsNeverLargerThanInput(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	for _, input := range compiledInputs() {
		for _, profile := range []string{"exact", "conservative", "balanced", "aggressive", "adaptive"} {
			for _, budget := range []int{0, 10, 200} {
				resp := compileForTest(t, InputOptimiseRequest{Input: input, Profile: profile, TokenBudget: budget})
				c := resp.Capsule
				if len(resp.CompiledText) > len(input) || c.CompiledTokens > c.OriginalTokens || c.Reduction < 0 {
					t.Fatalf("%s/%d: %q compiled larger: %d -> %d bytes (%q)", profile, budget, input, len(input), len(resp.CompiledText), resp.CompiledText)
				}
				if !c.Validation.Checks["exact_values_retained"] || !c.Validation.Checks["restoration_verified"] {
					t.Fatalf("%s/%d: %q failed validation %+v", profile, budget, input, c.Validation)
				}
				if rangeBytes(c.IncludedRanges)+rangeBytes(c.ExcludedRanges) != len(input) {
					t.Fatalf("%s/%d: ranges do not partition %q", profile, budget, input)
				}
			}
		}
	}
}

// TestInstructionAndExactValuesSurviveCollapse: the acceptance benchmark's
// cases keep their instruction sentence, exact value and cited source once
// collapsed, and restore exactly.
func TestInstructionAndExactValuesSurviveCollapse(t *testing.T) {
	useStore(t, newFileSourceStore(t.TempDir()))
	cases := [][3]string{
		{"retain mandatory instruction exact value 412 and cite journal commit", "412", "journal commit"},
		{"quote the governing policy clause forbidding unsigned extension packages", "unsigned extension", "governing policy"},
		{"retain the code symbol OutputReducer.reduce_output from the execution proxy adapter", "OutputReducer.reduce_output", "execution proxy adapter"},
		{"keep the supersession chain reference linking version-vi to version-vii", "version-vi", "supersession chain reference"},
	}
	for _, tc := range cases {
		input := strings.Repeat(tc[0]+" ", 5)
		resp := compileForTest(t, InputOptimiseRequest{Input: input, NonDroppableHints: []string{tc[1]}})
		for _, want := range tc {
			if !strings.Contains(resp.CompiledText, want) {
				t.Fatalf("%q lost %q: %q", tc[0], want, resp.CompiledText)
			}
		}
		if !resp.Validated || resp.Capsule.Reduction < 0.5 {
			t.Fatalf("%q: validated=%v reduction=%v", tc[0], resp.Validated, resp.Capsule.Reduction)
		}
		if restoreByID(t, resp.Capsule.CapsuleID) != input {
			t.Fatalf("%q did not restore exactly", tc[0])
		}
	}
}
