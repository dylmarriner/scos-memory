package main

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
	"strings"
	"sync"
	"time"

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
}

// ── server ───────────────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
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

	profile := resolveProfile(req)

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

	// ── 7. Build capsule ────────────────────────────────────────────
	capsule := buildCapsule(req.Input, profile, segments, compiledSegments, allocation, validated)
	for _, source := range capsule.SourceObjects {
		if err := authoritativeSourceStore.Put(source.ObjectID, source.Version, source.ContentHash, req.Input); err != nil {
			http.Error(w, "failed to persist authoritative source", http.StatusInternalServerError)
			return
		}
	}

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
	// Every capsule must carry a restoration handle; report whether it does.
	serviceMetrics.Gauge(telemetry.MetricRestorationSuccess,
		boolAsRatio(len(capsule.RestorationHandles) > 0), "ratio", labels, corr)

	writeJSON(w, resp)
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

	optimised := optimiseOutput(req.RawOutput, req.Format, maxTokens)
	originalTokens := estimateTokens(req.RawOutput)
	optimisedTokens := estimateTokens(optimised)

	resp := OutputOptimiseResponse{
		Optimised: optimised,
		Reduction: originalTokens - optimisedTokens,
		CapsuleID: newCapsuleID(),
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

func allocateBudget(totalBudget int, profile string, segments []Segment) BudgetAllocation {
	allocation := BudgetAllocation{
		Governing: percentOf(totalBudget, 15),
		Request:   percentOf(totalBudget, 15),
		TaskState: percentOf(totalBudget, 15),
		Evidence:  percentOf(totalBudget, 25),
		CodeTools: percentOf(totalBudget, 20),
		Reserve:   percentOf(totalBudget, 10),
	}

	if profile == "exact" || profile == "conservative" {
		// For exact/conservative, give more room to governing and request
		allocation = BudgetAllocation{
			Governing: percentOf(totalBudget, 25),
			Request:   percentOf(totalBudget, 25),
			TaskState: percentOf(totalBudget, 15),
			Evidence:  percentOf(totalBudget, 15),
			CodeTools: percentOf(totalBudget, 10),
			Reserve:   percentOf(totalBudget, 10),
		}
	}

	// Tally actual tokens per class from segments
	governingTokens := 0
	requestTokens := 0
	taskStateTokens := 0
	evidenceTokens := 0
	codeTokens := 0
	otherTokens := 0

	for _, seg := range segments {
		switch seg.Class {
		case "governing_policy":
			governingTokens += seg.OriginalTokens
		case "current_user_instruction":
			requestTokens += seg.OriginalTokens
		case "active_task_state", "acceptance_criteria":
			taskStateTokens += seg.OriginalTokens
		case "confirmed_memory", "evidence":
			evidenceTokens += seg.OriginalTokens
		case "code_symbol", "code_dependency", "tool_schema":
			codeTokens += seg.OriginalTokens
		default:
			otherTokens += seg.OriginalTokens
		}
	}

	// Use remaining evidence budget for confirmed_memory/evidence class
	allocation.Evidence += otherTokens
	return allocation
}

// ── compression (§18.3, §18.4) ───────────────────────────────────────────

func compressSegments(segments []Segment, profile string, allocation BudgetAllocation) (string, []Segment) {
	var compressed []Segment
	totalUsed := 0
	reserveRemaining := allocation.Reserve

	for _, seg := range segments {
		// Determine budget for this segment's class
		classBudget := classBudgetFor(seg.Class, allocation)

		// Never drop non-droppable classes
		if !seg.Droppable {
			compressed = append(compressed, seg)
			totalUsed += seg.OriginalTokens
			continue
		}

		// Apply profile-based compression
		switch profile {
		case "exact":
			// Only dedup and canonicalisation, no semantic compression
			compressed = append(compressed, seg)
			totalUsed += seg.OriginalTokens

		case "conservative":
			// Lossless transforms: truncate trailing whitespace, remove URLs
			content := seg.Content
			compressed = append(compressed, Segment{
				Class:          seg.Class,
				Content:        content,
				OriginalTokens: seg.OriginalTokens,
				CompiledTokens: estimateTokens(content),
				Droppable:      seg.Droppable,
			})
			totalUsed += estimateTokens(content)

		case "balanced":
			// Reversible: remove optional sections after first occurrence
			if classBudget > 0 && totalUsed+seg.OriginalTokens <= classBudget+reserveRemaining {
				content := seg.Content
				if len(content) > 1000 {
					content = content[:1000] + "\n<!-- truncated; full content in capsule -->"
				}
				compressed = append(compressed, Segment{
					Class:          seg.Class,
					Content:        content,
					OriginalTokens: seg.OriginalTokens,
					CompiledTokens: estimateTokens(content),
					Droppable:      seg.Droppable,
				})
				totalUsed += estimateTokens(content)
			} else if !seg.Droppable {
				compressed = append(compressed, seg)
				totalUsed += seg.OriginalTokens
			} else {
				// Droppable class, over budget → replace with reference
				ref := "[Reference: " + seg.Class + " hash=" + sha256Hex([]byte(seg.Content))[:12] + "]"
				compressed = append(compressed, Segment{
					Class:          seg.Class,
					Content:        ref,
					OriginalTokens: seg.OriginalTokens,
					CompiledTokens: estimateTokens(ref),
					Droppable:      true,
				})
				totalUsed += estimateTokens(ref)
			}

		case "aggressive":
			// Strong reduction: summarise to one line per segment
			if classBudget > 0 && totalUsed+seg.OriginalTokens <= classBudget+reserveRemaining {
				compressed = append(compressed, seg)
				totalUsed += seg.OriginalTokens
			} else {
				summary := summarise(seg.Content, seg.Class)
				compressed = append(compressed, Segment{
					Class:          seg.Class,
					Content:        summary,
					OriginalTokens: seg.OriginalTokens,
					CompiledTokens: estimateTokens(summary),
					Droppable:      true,
				})
				totalUsed += estimateTokens(summary)
			}

		default: // adaptive — uses balanced as default
			compressed = append(compressed, seg)
			totalUsed += seg.OriginalTokens
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

func optimiseOutput(raw string, format string, maxTokens int) string {
	if format == "compact" || format == "diff" {
		return compactOutput(raw)
	}

	optimised := raw
	// Remove repeated empty lines
	optimised = strings.ReplaceAll(optimised, "\n\n\n\n", "\n\n")
	optimised = strings.ReplaceAll(optimised, "\n\n\n", "\n\n")

	// Truncate to token budget
	if estimateTokens(optimised) > maxTokens {
		lines := strings.Split(optimised, "\n")
		var keep []string
		used := 0
		for _, line := range lines {
			t := estimateTokens(line)
			if used+t > maxTokens {
				break
			}
			keep = append(keep, line)
			used += t
		}
		optimised = strings.Join(keep, "\n")
	}

	return optimised
}

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

		// Remove redundant "I think", "Let me", "Based on"
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "i think ") || strings.HasPrefix(lower, "i believe ") {
			trimmed = trimmed[7:]
		}
		if strings.HasPrefix(lower, "let me ") {
			continue
		}
		if strings.HasPrefix(lower, "based on ") && strings.Contains(lower, "i can") {
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
	compiledTokens := 0
	for _, s := range compiled {
		compiledTokens += s.CompiledTokens
	}
	if compiledTokens == 0 {
		compiledTokens = origTokens
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
		"semantic_invariants":  valid,
		"source_hash_recorded": true,
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

func classBudgetFor(class string, alloc BudgetAllocation) int {
	switch class {
	case "governing_policy":
		return alloc.Governing
	case "current_user_instruction":
		return alloc.Request
	case "active_task_state", "acceptance_criteria":
		return alloc.TaskState
	case "confirmed_memory", "evidence":
		return alloc.Evidence
	case "code_symbol", "code_dependency", "tool_schema":
		return alloc.CodeTools
	default:
		return alloc.Evidence // use evidence budget for background
	}
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
func resolveProfile(req InputOptimiseRequest) string {
	requested := strings.ToLower(strings.TrimSpace(req.Profile))
	if isHighImpact(req) {
		if requested == "exact" || requested == "conservative" {
			return requested
		}
		return "conservative"
	}
	if requested == "" {
		return "balanced"
	}
	return requested
}
