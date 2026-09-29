package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("context-optimisation")

// ── configuration ────────────────────────────────────────────────────────

const version = "0.1.0"

var startTime = time.Now()

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

func readinessCheck() error {
	return readiness.WritableDir(os.TempDir())
}

// ── request / response types ─────────────────────────────────────────────

type InputOptimiseRequest struct {
	Input    string `json:"input"`
	TaskType string `json:"task_type,omitempty"`
	// Sensitivity and RiskClass drive the §18 profile floor: high-impact or
	// restricted work may not be aggressively compressed.
	Sensitivity       string      `json:"sensitivity,omitempty"`
	RiskClass         string      `json:"risk_class,omitempty"`
	Model             string      `json:"model,omitempty"`
	Profile           string      `json:"profile,omitempty"`
	TokenBudget       int         `json:"token_budget,omitempty"`
	NonDroppableHints []string    `json:"non_droppable_hints,omitempty"`
	SessionContext    *SessionCtx `json:"session_context,omitempty"`
	CodeSymbols       []string    `json:"code_symbols,omitempty"`
	MemoryReferences  []string    `json:"memory_references,omitempty"`
}

type SessionCtx struct {
	SessionID       string `json:"session_id"`
	LastCapsuleID   string `json:"last_capsule_id,omitempty"`
	LastContextHash string `json:"last_context_hash,omitempty"`
}

type Segment struct {
	Class          string `json:"class"`
	Content        string `json:"content"`
	OriginalTokens int    `json:"original_tokens"`
	CompiledTokens int    `json:"compiled_tokens"`
	Droppable      bool   `json:"droppable"`
	ContentHash    string `json:"content_hash"`
}

type BudgetAllocation struct {
	// Mandatory is the non-droppable content emitted regardless of budget.
	Mandatory int `json:"mandatory"`
	Governing int `json:"governing_policy"`
	Request   int `json:"current_request"`
	TaskState int `json:"task_state"`
	Evidence  int `json:"evidence"`
	CodeTools int `json:"code_tools"`
	Reserve   int `json:"reserve"`
}

type ContextCapsule struct {
	CapsuleID                string              `json:"capsule_id"`
	Profile                  string              `json:"compression_profile"`
	SourceContentHash        string              `json:"source_content_hash"`
	OriginalTokens           int                 `json:"original_token_count"`
	CompiledTokens           int                 `json:"compiled_token_count"`
	Reduction                float64             `json:"reduction_ratio"`
	Segments                 []Segment           `json:"segments"`
	Allocation               BudgetAllocation    `json:"allocation"`
	CompilerVersion          string              `json:"compiler_version"`
	CreatedAt                string              `json:"created_at"`
	SourceObjects            []SourceObjectRef   `json:"source_objects"`
	IncludedRanges           []ContextRange      `json:"included_ranges"`
	ExcludedRanges           []ContextRange      `json:"excluded_ranges"`
	CompressionPolicyVersion string              `json:"compression_policy_version"`
	CompressionPolicyHash    string              `json:"compression_policy_hash"`
	TokenAccounting          TokenAccounting     `json:"token_accounting"`
	SemanticInvariants       []string            `json:"semantic_invariants"`
	Validation               ValidationResults   `json:"validation"`
	RestorationHandles       []RestorationHandle `json:"restoration_handles"`
	Transformations          []string            `json:"transformations"`
}

type SourceObjectRef struct {
	ObjectID    string `json:"object_id"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
}
type ContextRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
type TokenAccounting struct {
	Method     string `json:"method"`
	Original   int    `json:"original"`
	Compressed int    `json:"compressed"`
	Included   int    `json:"included"`
	Excluded   int    `json:"excluded"`
}
type ValidationResults struct {
	Valid  bool            `json:"valid"`
	Checks map[string]bool `json:"checks"`
}
type RestorationHandle struct {
	HandleID    string `json:"handle_id"`
	ObjectID    string `json:"object_id"`
	ContentHash string `json:"content_hash"`
}

type InputOptimiseResponse struct {
	Capsule      ContextCapsule `json:"capsule"`
	CompiledText string         `json:"compiled_text"`
	Validated    bool           `json:"validated"`
}

type OutputOptimiseRequest struct {
	RawOutput     string `json:"raw_output"`
	RequestIntent string `json:"request_intent,omitempty"`
	Format        string `json:"format,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
}

type OutputOptimiseResponse struct {
	Optimised string `json:"optimised"`
	Reduction int    `json:"token_reduction"`
	CapsuleID string `json:"capsule_id,omitempty"`
	// SourceObjects names the stored raw output; POST it with capsule_id to
	// /v1/context/restore to recover anything the optimisation left out.
	SourceObjects []SourceObjectRef `json:"source_objects,omitempty"`
	OmittedLines  int               `json:"omitted_lines"`
}

// ── server ───────────────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("context-optimisation"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return readinessCheck()
	}))
	mux.HandleFunc("/v1/version", handleVersion)
	mux.HandleFunc("/v1/optimize/input", handleInputOptimise)
	mux.HandleFunc("/v1/optimize/output", handleOutputOptimise)
	// Governing names. The legacy optimise paths remain supported for clients
	// that adopted the initial service contract.
	mux.HandleFunc("/v1/context/compile", handleInputOptimise)
	mux.HandleFunc("/v1/context/restore", handleContextRestore)
	mux.HandleFunc("/v1/context/evaluate", handleContextEvaluate)

	addr := os.Getenv("SCOS_CONTEXT_HTTP_ADDR")
	if addr == "" {
		addr = ":8083"
	}
	log.Printf("context optimisation layer listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.ControlPlaneHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

type contextRestoreRequest struct {
	Capsule ContextCapsule `json:"capsule"`
}

// SourceStore is the authority for content referenced by a capsule. Callers
// cannot provide replacement content during restoration.
type SourceStore interface {
	Put(objectID, version, contentHash, content string) error
	Get(objectID, version string) (string, error)
}

type fileSourceStore struct {
	root string
	mu   sync.RWMutex
}

func newFileSourceStore(root string) *fileSourceStore { return &fileSourceStore{root: root} }
func (s *fileSourceStore) path(objectID, version string) string {
	return filepath.Join(s.root, objectID+"-"+version+".source")
}
func (s *fileSourceStore) Put(objectID, version, contentHash, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return err
	}
	if sha256Hex([]byte(content)) != contentHash {
		return fmt.Errorf("source hash does not match content")
	}
	return os.WriteFile(s.path(objectID, version), []byte(content), 0600)
}
func (s *fileSourceStore) Get(objectID, version string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := os.ReadFile(s.path(objectID, version))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func defaultSourceStore() SourceStore {
	root := os.Getenv("SCOS_CONTEXT_SOURCE_STORE")
	if root == "" {
		root = filepath.Join(os.TempDir(), "scos-context-sources")
	}
	return newFileSourceStore(root)
}

var authoritativeSourceStore SourceStore = defaultSourceStore()

type contextEvaluateRequest struct {
	Input        string          `json:"input"`
	CompiledText string          `json:"compiled_text"`
	Capsule      *ContextCapsule `json:"capsule,omitempty"`
	Hints        []string        `json:"non_droppable_hints,omitempty"`
}

func handleContextEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contextEvaluateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" || strings.TrimSpace(req.CompiledText) == "" {
		http.Error(w, "input and compiled_text are required", http.StatusBadRequest)
		return
	}

	originalTokens := estimateTokens(req.Input)
	compiledTokens := estimateTokens(req.CompiledText)
	valid := validateInvariants(req.Input, req.CompiledText, req.Hints)
	if req.Capsule != nil {
		if req.Capsule.SourceContentHash == "" || sha256Hex([]byte(req.Input)) != req.Capsule.SourceContentHash {
			http.Error(w, "input does not match capsule", http.StatusConflict)
			return
		}
		if req.Capsule.CompiledTokens > 0 && req.Capsule.CompiledTokens != compiledTokens {
			valid = false
		}
	}
	reduction := 0.0
	if originalTokens > 0 {
		reduction = 1 - float64(compiledTokens)/float64(originalTokens)
	}
	writeJSON(w, map[string]any{
		"valid":               valid,
		"source_content_hash": sha256Hex([]byte(req.Input)),
		"original_tokens":     originalTokens,
		"compiled_tokens":     compiledTokens,
		"reduction_ratio":     reduction,
	})
}

func handleContextRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contextRestoreRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Capsule.CapsuleID == "" || len(req.Capsule.SourceObjects) == 0 {
		http.Error(w, "capsule with source_objects is required", http.StatusBadRequest)
		return
	}
	var restored strings.Builder
	for _, source := range req.Capsule.SourceObjects {
		if !validSourceRef(source) {
			http.Error(w, "invalid source reference", http.StatusBadRequest)
			return
		}
		content, err := authoritativeSourceStore.Get(source.ObjectID, source.Version)
		if err != nil {
			http.Error(w, "authoritative source is missing", http.StatusNotFound)
			return
		}
		if sha256Hex([]byte(content)) != source.ContentHash {
			http.Error(w, "authoritative source failed integrity validation", http.StatusConflict)
			return
		}
		restored.WriteString(content)
	}
	writeJSON(w, map[string]any{
		"capsule_id": req.Capsule.CapsuleID,
		"restored":   true,
		"content":    restored.String(),
	})
}

func validSourceRef(source SourceObjectRef) bool {
	if source.ObjectID == "" || source.Version == "" || len(source.ContentHash) != 64 {
		return false
	}
	if strings.ContainsAny(source.ObjectID, `/\\`) || strings.ContainsAny(source.Version, `/\\`) {
		return false
	}
	_, err := hex.DecodeString(source.ContentHash)
	return err == nil
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── input optimisation ───────────────────────────────────────────────────

func handleInputOptimise(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req InputOptimiseRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Input == "" {
		http.Error(w, "input is required", http.StatusBadRequest)
		return
	}

	profile, err := resolveProfile(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tokenBudget := req.TokenBudget
	if tokenBudget <= 0 {
		tokenBudget = 8000
	}

	// ── 1. Canonicalise ─────────────────────────────────────────────
	canonical := canonicalise(req.Input)

	// ── 2. Classify segments ────────────────────────────────────────
	segments := classifySegments(canonical, req.NonDroppableHints)

	// ── 3. Exact deduplication (within input) ──────────────────────
	segments = deduplicateSegments(segments)

	// ── 4. Allocate budget per class ────────────────────────────────
	allocation := allocateBudget(tokenBudget, profile, segments)

	// ── 5. Compress per profile ─────────────────────────────────────
	compiled, compiledSegments := compressSegments(segments, profile, allocation)

	// ── 6. Invariant validation ─────────────────────────────────────
	validated := validateInvariants(req.Input, compiled, req.NonDroppableHints)
	applied := []string{"canonicalisation", "exact_deduplication", "class_budget_allocation", "profile_compression:" + profile, "invariant_validation"}
	if !validated && profile != "exact" {
		// A compilation that loses a protected value is not returned: fall
		// back to the lossless exact profile and say so.
		profile = "exact"
		compiled, compiledSegments = compressSegments(segments, profile, allocation)
		validated = validateInvariants(req.Input, compiled, req.NonDroppableHints)
		applied = append(applied, "fallback:exact")
	}

	// ── 7. Build capsule ────────────────────────────────────────────
	capsule := buildCapsule(req.Input, profile, segments, compiledSegments, allocation, validated)
	// The capsule records the transformations this run applied -- not the
	// §18.3 catalogue of every transform the compiler could name.
	capsule.Transformations = applied
	for _, source := range capsule.SourceObjects {
		if err := authoritativeSourceStore.Put(source.ObjectID, source.Version, source.ContentHash, req.Input); err != nil {
			http.Error(w, "failed to persist authoritative source", http.StatusInternalServerError)
			return
		}
	}
	capsule.Validation.Checks["source_hash_recorded"] = true

	resp := InputOptimiseResponse{
		Capsule:      capsule,
		CompiledText: compiled,
		Validated:    validated,
	}

	// §32 context-optimisation measures. Retention values come from the
	// invariant validator, which is what actually checked them -- they are
	// not restated assumptions.
	corr := telemetry.Correlation{}
	labels := map[string]string{"profile": profile}
	if capsule.OriginalTokens > 0 {
		reduction := 1 - float64(capsule.CompiledTokens)/float64(capsule.OriginalTokens)
		serviceMetrics.Gauge(telemetry.MetricTokenReduction, reduction, "ratio", labels, corr)
	}
	// `validated` is the invariant check: non-droppable hints and exact
	// values survived compression.
	serviceMetrics.Gauge(telemetry.MetricInstructionRetention,
		boolAsRatio(validated), "ratio", labels, corr)
	serviceMetrics.Gauge(telemetry.MetricExactValueRetention,
		boolAsRatio(capsule.Validation.Valid), "ratio", labels, corr)
	// Semantic retention is measured on the text actually handed back, not
	// inferred from the invariant flag. Inputs with no salient terms have no
	// measurable content to retain, so no point is emitted for them.
	if retention, ok := semanticRetention(req.Input, compiled); ok {
		serviceMetrics.Gauge(telemetry.MetricSemanticRetention, retention, "ratio", labels, corr)
	}
	// Every capsule must carry a restoration handle; report whether it does.
	serviceMetrics.Gauge(telemetry.MetricRestorationSuccess,
		boolAsRatio(len(capsule.RestorationHandles) > 0), "ratio", labels, corr)

	writeJSON(w, resp)
}

// semanticRetention is the fraction of the original input's distinct salient
// terms that still appear in the compiled text. Method (embedding-free lexical
// coverage): both texts are lower-cased and split on every non-letter/non-digit
// rune; a salient term is a token of at least four runes that is not in
// semanticStopwords. The result is |salient(original) ∩ terms(compiled)| /
// |salient(original)|. ok is false when the original has no salient terms.
func semanticRetention(original, compiled string) (float64, bool) {
	split := func(text string) []string {
		return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
	}
	kept := make(map[string]struct{})
	for _, t := range split(compiled) {
		kept[t] = struct{}{}
	}
	salient := make(map[string]struct{})
	for _, t := range split(original) {
		if utf8.RuneCountInString(t) >= 4 && !semanticStopwords[t] {
			salient[t] = struct{}{}
		}
	}
	if len(salient) == 0 {
		return 0, false
	}
	retained := 0
	for t := range salient {
		if _, ok := kept[t]; ok {
			retained++
		}
	}
	return float64(retained) / float64(len(salient)), true
}

// semanticStopwords are high-frequency function words of four or more letters
// that carry no task meaning and would otherwise inflate retention.
var semanticStopwords = map[string]bool{
	"about": true, "after": true, "also": true, "been": true, "before": true,
	"being": true, "could": true, "does": true, "each": true, "from": true,
	"have": true, "into": true, "just": true, "more": true, "most": true,
	"must": true, "only": true, "other": true, "over": true, "same": true,
	"should": true, "some": true, "such": true, "than": true, "that": true,
	"their": true, "them": true, "then": true, "there": true, "these": true,
	"they": true, "this": true, "those": true, "very": true, "were": true,
	"what": true, "when": true, "where": true, "which": true, "while": true,
	"will": true, "with": true, "would": true, "your": true,
}

// boolAsRatio renders a pass/fail invariant as a 1/0 ratio.
func boolAsRatio(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

// ── output optimisation ──────────────────────────────────────────────────

func handleOutputOptimise(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OutputOptimiseRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RawOutput == "" {
		http.Error(w, "raw_output is required", http.StatusBadRequest)
		return
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2000
	}

	// The raw output is the authoritative source: store it first so the
	// capsule returned below really can restore it (§18.1/§18.5).
	contentHash := sha256Hex([]byte(req.RawOutput))
	source := SourceObjectRef{ObjectID: "output-" + contentHash[:32], Version: "1", ContentHash: contentHash}
	if err := authoritativeSourceStore.Put(source.ObjectID, source.Version, source.ContentHash, req.RawOutput); err != nil {
		http.Error(w, "store raw output: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	capsuleID := newCapsuleID()
	optimised, omitted := optimiseOutput(req.RawOutput, req.Format, maxTokens, capsuleID)
	originalTokens := estimateTokens(req.RawOutput)
	optimisedTokens := estimateTokens(optimised)

	resp := OutputOptimiseResponse{
		Optimised:     optimised,
		Reduction:     originalTokens - optimisedTokens,
		CapsuleID:     capsuleID,
		SourceObjects: []SourceObjectRef{source},
		OmittedLines:  omitted,
	}
	writeJSON(w, resp)
}

// ── pipeline stages ──────────────────────────────────────────────────────

func canonicalise(input string) string {
	// Remove BOM, normalise line endings, strip trailing whitespace per line
	cleaned := strings.ReplaceAll(input, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	cleaned = strings.TrimPrefix(cleaned, "\ufeff")

	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

// ── segment classification (§18.2) ───────────────────────────────────────

var nonDroppableClasses = map[string]bool{
	"governing_policy":         true,
	"current_user_instruction": true,
	"acceptance_criteria":      true,
	"active_task_state":        true,
}

func classifySegments(input string, hints []string) []Segment {
	text := input
	var segments []Segment

	// Try to detect code blocks as code_symbol or code_dependency
	if strings.Contains(text, "```") {
		parts := splitCodeBlocks(text)
		for _, part := range parts {
			class := "optional_background"
			if part.isCode {
				if strings.Contains(part.content, "import ") || strings.Contains(part.content, "require(") {
					class = "code_dependency"
				} else {
					class = "code_symbol"
				}
			} else if strings.Contains(part.content, "policy:") || strings.Contains(part.content, "governing") {
				class = "governing_policy"
			} else if strings.Contains(part.content, "instruction:") || strings.Contains(part.content, "Instruction:") {
				class = "current_user_instruction"
			} else if strings.Contains(part.content, "criteria:") || strings.Contains(part.content, "acceptance:") {
				class = "acceptance_criteria"
			} else if strings.Contains(part.content, "task state:") || strings.Contains(part.content, "state:") {
				class = "active_task_state"
			} else if strings.Contains(part.content, "conversation") || strings.Contains(part.content, "previous") {
				class = "conversation_history"
			} else if strings.Contains(part.content, "schema") || strings.Contains(part.content, "tool:") {
				class = "tool_schema"
			} else if strings.Contains(part.content, "result:") || strings.Contains(part.content, "output:") {
				class = "tool_result"
			}
			segments = append(segments, Segment{
				Class:          class,
				Content:        part.content,
				OriginalTokens: estimateTokens(part.content),
				Droppable:      !nonDroppableClasses[class],
			})
		}
	} else {
		// Single segment with heuristic classification
		class := "optional_background"
		if strings.Contains(text, "policy:") || strings.Contains(text, "governing") {
			class = "governing_policy"
		} else if strings.Contains(text, "instruction:") || strings.Contains(text, "Instruction:") {
			class = "current_user_instruction"
		} else if strings.Contains(text, "criteria:") || strings.Contains(text, "acceptance:") {
			class = "acceptance_criteria"
		} else if strings.Contains(text, "state:") || strings.Contains(text, "task state:") {
			class = "active_task_state"
		}
		segments = append(segments, Segment{
			Class:          class,
			Content:        text,
			OriginalTokens: estimateTokens(text),
			Droppable:      !nonDroppableClasses[class],
		})
	}

	// Apply user hints for non-droppable content
	for _, hint := range hints {
		hintLower := strings.ToLower(hint)
		for i, seg := range segments {
			if strings.Contains(strings.ToLower(seg.Content), hintLower) {
				segments[i].Droppable = false
			}
		}
	}

	return segments
}

type codeBlockPart struct {
	content string
	isCode  bool
}

func splitCodeBlocks(input string) []codeBlockPart {
	var parts []codeBlockPart
	for {
		codeStart := strings.Index(input, "```")
		if codeStart == -1 {
			parts = append(parts, codeBlockPart{content: input, isCode: false})
			break
		}
		// Text before code block
		if codeStart > 0 {
			parts = append(parts, codeBlockPart{content: input[:codeStart], isCode: false})
		}
		rest := input[codeStart+3:]
		langEnd := strings.Index(rest, "\n")
		if langEnd == -1 {
			parts = append(parts, codeBlockPart{content: rest, isCode: true})
			break
		}
		codeEnd := strings.Index(rest, "\n```")
		if codeEnd == -1 {
			parts = append(parts, codeBlockPart{content: rest, isCode: true})
			break
		}
		codeContent := rest[langEnd+1 : codeEnd]
		parts = append(parts, codeBlockPart{content: codeContent, isCode: true})
		input = rest[codeEnd+4:]
	}
	return parts
}

// ── dedup ────────────────────────────────────────────────────────────────

func deduplicateSegments(segments []Segment) []Segment {
	seen := make(map[string]bool)
	var deduped []Segment
	for _, s := range segments {
		hash := sha256Hex([]byte(strings.TrimSpace(s.Content)))
		if seen[hash] {
			continue
		}
		seen[hash] = true
		deduped = append(deduped, s)
	}
	return deduped
}

// ── budget allocation (§18.6) ────────────────────────────────────────────

// budgetClass names the §18.6 bucket a segment class draws from; ""
// (optional background) has no bucket of its own and lives on reserve.
func budgetClass(class string) string {
	switch class {
	case "governing_policy":
		return "governing"
	case "current_user_instruction":
		return "request"
	case "active_task_state", "acceptance_criteria":
		return "task_state"
	case "confirmed_memory", "evidence":
		return "evidence"
	case "code_symbol", "code_dependency", "tool_schema":
		return "code_tools"
	default:
		return ""
	}
}

// allocateBudget implements §18.6. Mandatory (non-droppable) content is
// reserved first -- it is always emitted -- and only what remains is split
// across the classes by the profile's percentages. A class whose content
// needs less than its share returns the surplus to the shared reserve, so
// the allocation never exceeds totalBudget and nothing is budgeted twice.
func allocateBudget(totalBudget int, profile string, segments []Segment) BudgetAllocation {
	mandatory := 0
	demand := map[string]int{}
	for _, seg := range segments {
		if !seg.Droppable {
			mandatory += seg.OriginalTokens
			continue
		}
		demand[budgetClass(seg.Class)] += seg.OriginalTokens
	}
	available := totalBudget - mandatory
	if available < 0 {
		available = 0
	}
	split := map[string]int{"governing": 15, "request": 15, "task_state": 15, "evidence": 25, "code_tools": 20, "reserve": 10}
	if profile == "exact" || profile == "conservative" {
		split = map[string]int{"governing": 25, "request": 25, "task_state": 15, "evidence": 15, "code_tools": 10, "reserve": 10}
	}
	share := map[string]int{}
	allocated := 0
	for class, pct := range split {
		share[class] = percentOf(available, pct)
		allocated += share[class]
	}
	share["reserve"] += available - allocated // rounding remainder
	for _, class := range []string{"governing", "request", "task_state", "evidence", "code_tools"} {
		if surplus := share[class] - demand[class]; surplus > 0 {
			share[class] -= surplus
			share["reserve"] += surplus
		}
	}
	return BudgetAllocation{
		Mandatory: mandatory,
		Governing: share["governing"],
		Request:   share["request"],
		TaskState: share["task_state"],
		Evidence:  share["evidence"],
		CodeTools: share["code_tools"],
		Reserve:   share["reserve"],
	}
}

// ── compression (§18.3, §18.4) ───────────────────────────────────────────

func compressSegments(segments []Segment, profile string, allocation BudgetAllocation) (string, []Segment) {
	var compressed []Segment
	remaining := map[string]int{
		"governing":  allocation.Governing,
		"request":    allocation.Request,
		"task_state": allocation.TaskState,
		"evidence":   allocation.Evidence,
		"code_tools": allocation.CodeTools,
	}
	reserve := allocation.Reserve
	// fits charges tokens to the segment's own class first, then to the
	// shared reserve; it reports false (charging nothing) when neither can
	// cover them.
	fits := func(class string, tokens int) bool {
		own := remaining[class]
		if own >= tokens {
			remaining[class] -= tokens
			return true
		}
		if own+reserve >= tokens {
			remaining[class] = 0
			reserve -= tokens - own
			return true
		}
		return false
	}
	emit := func(seg Segment, content string, droppable bool) {
		compressed = append(compressed, Segment{Class: seg.Class, Content: content, OriginalTokens: seg.OriginalTokens, CompiledTokens: estimateTokens(content), Droppable: droppable})
	}

	for _, seg := range segments {
		// Mandatory content is always emitted verbatim; its size was
		// reserved before the classes were funded.
		if !seg.Droppable {
			emit(seg, seg.Content, false)
			continue
		}
		class := budgetClass(seg.Class)
		if profile == "exact" || profile == "conservative" || fits(class, seg.OriginalTokens) {
			// exact and conservative are lossless: content that does not
			// fit is still emitted rather than dropped or rewritten.
			emit(seg, seg.Content, seg.Droppable)
			continue
		}
		// Over budget under a lossy profile.
		switch profile {
		case "balanced":
			// Reversible: a reference the capsule's restoration handle resolves.
			ref := "[Reference: " + seg.Class + " hash=" + sha256Hex([]byte(seg.Content))[:12] + " (restorable from the capsule)]"
			fits(class, estimateTokens(ref))
			emit(seg, ref, true)
		default: // aggressive, adaptive
			summary := summarise(seg.Content, seg.Class)
			fits(class, estimateTokens(summary))
			emit(seg, summary, true)
		}
	}

	// Assemble compiled text
	var parts []string
	for _, seg := range compressed {
		parts = append(parts, "--- "+seg.Class+" ---\n"+seg.Content)
	}
	return strings.Join(parts, "\n\n"), compressed
}

// ── validation (§18.3 invariant 14) ──────────────────────────────────────

func validateInvariants(original string, compiled string, hints []string) bool {
	// Check non-droppable hints are still present
	for _, hint := range hints {
		if !strings.Contains(compiled, hint) {
			return false
		}
	}
	// Check that exact identifiers, paths, versions are preserved
	extracted := extractExactValues(original)
	for _, val := range extracted {
		if !strings.Contains(compiled, val) {
			return false
		}
	}
	return true
}

func extractExactValues(text string) []string {
	var values []string
	// Extract UUIDs
	words := strings.Fields(text)
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:'\"()[]{},")
		if looksLikeUUID(w) || looksLikeVersion(w) {
			values = append(values, w)
		}
	}
	return values
}

func looksLikeUUID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	return len(parts[0]) == 8 && len(parts[1]) == 4 && len(parts[2]) == 4 && len(parts[3]) == 4 && len(parts[4]) == 12
}

func looksLikeVersion(s string) bool {
	return strings.Count(s, ".") >= 1 && len(s) >= 3 && len(s) <= 20
}

// ── output optimisation (§18.7) ──────────────────────────────────────────

// outputSignal marks lines §18.2 never allows to be dropped: errors,
// warnings and failures carry the result the caller acts on.
var outputSignal = regexp.MustCompile(`(?i)\b(error|errors|warn|warning|fail|failed|failure|fatal|panic|exception|traceback|denied|refused)\b`)

// optimiseOutput fits raw output to maxTokens without silently discarding
// what matters (§18.7). When the budget is exceeded it keeps every error or
// warning line plus as much surrounding output as fits, in the original
// order, and ends with an explicit marker naming how many lines were left
// out and the capsule that restores them. It returns the text and the
// number of omitted lines.
func optimiseOutput(raw, format string, maxTokens int, capsuleID string) (string, int) {
	optimised := raw
	if format == "compact" || format == "diff" {
		optimised = compactOutput(raw)
	} else {
		optimised = strings.ReplaceAll(optimised, "\n\n\n\n", "\n\n")
		optimised = strings.ReplaceAll(optimised, "\n\n\n", "\n\n")
	}
	if estimateTokens(optimised) <= maxTokens {
		return optimised, 0
	}
	lines := strings.Split(optimised, "\n")
	keep := make([]bool, len(lines))
	used := 0
	for i, line := range lines {
		if outputSignal.MatchString(line) {
			keep[i] = true
			used += estimateTokens(line) + 1
		}
	}
	for i, line := range lines {
		if keep[i] {
			continue
		}
		t := estimateTokens(line) + 1
		if used+t > maxTokens {
			break
		}
		keep[i] = true
		used += t
	}
	var out []string
	omitted := 0
	for i, line := range lines {
		if keep[i] {
			out = append(out, line)
		} else {
			omitted++
		}
	}
	if omitted > 0 {
		out = append(out, fmt.Sprintf("[%d of %d lines omitted to fit %d tokens; restore the full output with capsule %s]", omitted, len(lines), maxTokens, capsuleID))
	}
	return strings.Join(out, "\n"), omitted
}

// fillerLine matches a whole line of pure narration with no content.
var fillerLine = regexp.MustCompile(`(?i)^(let me (check|think|see|look)( (at|into) (this|that|it))?|one moment|okay|ok|sure)[.!…]*$`)

func compactOutput(raw string) string {
	lines := strings.Split(raw, "\n")
	var compact []string
	seen := make(map[string]bool)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Dedup repeated messages
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true

		// Hedges stay: "I think X" and "X" are different claims, and a
		// sentence that merely starts "Let me" can carry content ("Let me
		// know the deadline is Friday"). Only lines that are nothing but
		// conversational filler are dropped.
		if fillerLine.MatchString(trimmed) {
			continue
		}
		compact = append(compact, trimmed)
	}
	return strings.Join(compact, "\n")
}

// ── capsule ──────────────────────────────────────────────────────────────

func buildCapsule(original, profile string, segments, compiled []Segment, allocation BudgetAllocation, validation ...bool) ContextCapsule {
	origTokens := 0
	for _, s := range segments {
		origTokens += s.OriginalTokens
	}
	// Count what is actually emitted: segments passed through unchanged
	// carry no CompiledTokens of their own, and summing that field
	// under-reported the compiled size and inflated the reduction ratio.
	compiledTokens := 0
	for _, s := range compiled {
		compiledTokens += estimateTokens(s.Content)
	}

	reduction := 0.0
	if origTokens > 0 {
		reduction = 1.0 - float64(compiledTokens)/float64(origTokens)
	}

	contentHash := sha256Hex([]byte(original))
	objectID := "source-" + contentHash[:32]
	policyVersion := "context-policy-1"
	policyHash := sha256Hex([]byte(policyVersion + ":" + profile))
	valid := true
	if len(validation) > 0 {
		valid = validation[0]
	}
	checks := map[string]bool{
		"semantic_invariants": valid,
		// Set true only once the source is actually persisted.
		"source_hash_recorded": false,
	}
	return ContextCapsule{
		CapsuleID:                newCapsuleID(),
		Profile:                  profile,
		SourceContentHash:        contentHash,
		OriginalTokens:           origTokens,
		CompiledTokens:           compiledTokens,
		Reduction:                reduction,
		Segments:                 compiled,
		Allocation:               allocation,
		CompilerVersion:          version,
		CreatedAt:                time.Now().UTC().Format(time.RFC3339),
		SourceObjects:            []SourceObjectRef{{ObjectID: objectID, Version: "1", ContentHash: contentHash}},
		IncludedRanges:           []ContextRange{{Start: 0, End: len(original)}},
		ExcludedRanges:           []ContextRange{},
		CompressionPolicyVersion: policyVersion,
		CompressionPolicyHash:    policyHash,
		TokenAccounting:          TokenAccounting{Method: "utf8_bytes_div_4", Original: origTokens, Compressed: compiledTokens, Included: origTokens, Excluded: 0},
		SemanticInvariants:       extractExactValues(original),
		Validation:               ValidationResults{Valid: valid, Checks: checks},
		RestorationHandles:       []RestorationHandle{{HandleID: "restore-" + contentHash[:32], ObjectID: objectID, ContentHash: contentHash}},
	}
}

// ── utility ──────────────────────────────────────────────────────────────

func newCapsuleID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func estimateTokens(text string) int {
	return len(text) / 4
}

func percentOf(total, pct int) int {
	return total * pct / 100
}

func summarise(content string, class string) string {
	// Simple summarisation: take first sentence or first 200 chars
	firstSentence := content
	if idx := strings.Index(content, "."); idx > 0 && idx < 300 {
		firstSentence = content[:idx+1]
	}
	if len(firstSentence) > 200 {
		firstSentence = firstSentence[:200] + "..."
	}
	return "[" + class + " summary] " + firstSentence
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// highImpactTaskTypes are the task classes §18 names as fragile: governance,
// legal, security and similar work where lossy compression is unsafe.
var highImpactTaskTypes = map[string]bool{
	"governance": true, "legal": true, "security": true, "policy": true,
	"compliance": true, "audit": true, "financial": true, "production_change": true,
}

// restrictedSensitivity are the classifications that force a lossless profile.
var restrictedSensitivity = map[string]bool{
	"restricted": true, "secret": true, "confidential": true,
}

// lossyProfiles are the profiles that may discard material. §18 forbids them
// for high-impact or restricted work.
var lossyProfiles = map[string]bool{
	"balanced": true, "aggressive": true, "adaptive": true,
}

// isHighImpact reports whether §18's "high-impact or restricted" rule applies.
func isHighImpact(req InputOptimiseRequest) bool {
	if highImpactTaskTypes[strings.ToLower(strings.TrimSpace(req.TaskType))] {
		return true
	}
	if restrictedSensitivity[strings.ToLower(strings.TrimSpace(req.Sensitivity))] {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(req.RiskClass)) {
	case "high", "critical":
		return true
	}
	return false
}

// resolveProfile applies the §18 compression profile rules. High-impact and
// restricted tasks default to conservative, and an explicitly requested lossy
// profile is refused for them rather than silently honoured: §42 forbids
// dropping governing instructions, exact values or evidence during
// compression, so the caller does not get to opt out of that floor.
// contextProfiles are the §18.4 compression profiles.
var contextProfiles = map[string]bool{"exact": true, "conservative": true, "balanced": true, "aggressive": true, "adaptive": true}

// resolveProfile applies §18.4: conservative is the default for work the
// caller has not characterised, high-impact or restricted work is floored
// at conservative, and an unknown profile name is an error rather than a
// silent choice of some other compression.
func resolveProfile(req InputOptimiseRequest) (string, error) {
	requested := strings.ToLower(strings.TrimSpace(req.Profile))
	if requested != "" && !contextProfiles[requested] {
		return "", fmt.Errorf("unknown context profile %q", req.Profile)
	}
	if isHighImpact(req) {
		if requested == "exact" || requested == "conservative" {
			return requested, nil
		}
		return "conservative", nil
	}
	if requested == "" {
		return "conservative", nil
	}
	return requested, nil
}
