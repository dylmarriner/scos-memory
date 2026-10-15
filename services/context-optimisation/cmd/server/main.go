package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("context-optimisation")

// ── configuration ────────────────────────────────────────────────────────

const version = "0.2.0"

var startTime = time.Now()

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

// readinessCheck reports whether the authoritative source store -- the one
// every compile and restore depends on -- can be written.
func readinessCheck() error {
	return authoritativeSourceStore.Ready()
}

// ── request / response types ─────────────────────────────────────────────

// InputOptimiseRequest is the compile contract. Every field changes the
// result: unknown fields (including the former model and session_context
// fields, which had no implementation) are rejected with 400.
type InputOptimiseRequest struct {
	Input    string `json:"input"`
	TaskType string `json:"task_type,omitempty"`
	// Sensitivity and RiskClass drive the §18 profile floor: high-impact or
	// restricted work may not be aggressively compressed.
	Sensitivity string `json:"sensitivity,omitempty"`
	RiskClass   string `json:"risk_class,omitempty"`
	// Profile must be one of compressionProfiles; empty selects the default.
	Profile string `json:"profile,omitempty"`
	// TokenBudget sizes the per-class allocation; 0 selects 8000 and a
	// negative value is rejected.
	TokenBudget int `json:"token_budget,omitempty"`
	// NonDroppableHints keep every segment that mentions them (case
	// insensitive) and must each appear verbatim in the compiled text.
	NonDroppableHints []string `json:"non_droppable_hints,omitempty"`
	// CodeSymbols and MemoryReferences are retention pins: each must occur
	// verbatim in the input (otherwise the request is rejected), every
	// segment containing one is kept uncompressed, and validation fails if
	// one is missing from the compiled text.
	CodeSymbols      []string `json:"code_symbols,omitempty"`
	MemoryReferences []string `json:"memory_references,omitempty"`
}

// Segment is one classified slice of the canonicalised input. Content is the
// compiled form; ContentHash and SourceRange describe the source slice.
type Segment struct {
	Class          string `json:"class"`
	Content        string `json:"content"`
	OriginalTokens int    `json:"original_tokens"`
	CompiledTokens int    `json:"compiled_tokens"`
	Droppable      bool   `json:"droppable"`
	ContentHash    string `json:"content_hash"`
	// SourceRanges are the byte ranges of the stored source object this
	// segment was taken from (canonicalisation can split a segment's source
	// into several ranges).
	SourceRanges []ContextRange `json:"source_ranges"`
	// Transformation names what compression did to this segment:
	// pass_through, truncation, reference_substitution or extractive_summary.
	Transformation string `json:"transformation"`

	// start and end locate the segment in the canonical text; retained is the
	// number of leading bytes of the source slice copied verbatim into
	// Content.
	start, end, retained int
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
	// CapsuleID is derived from the source hash, policy hash and compiled
	// text hash and is recorded in the source store, so POST
	// /v1/context/restore with {"capsule_id": ...} returns the source.
	CapsuleID                string                 `json:"capsule_id"`
	Profile                  string                 `json:"compression_profile"`
	SourceContentHash        string                 `json:"source_content_hash"`
	OriginalTokens           int                    `json:"original_token_count"`
	CompiledTokens           int                    `json:"compiled_token_count"`
	Reduction                float64                `json:"reduction_ratio"`
	Segments                 []Segment              `json:"segments"`
	Allocation               BudgetAllocation       `json:"allocation"`
	TokenBudget              int                    `json:"token_budget"`
	CompilerVersion          string                 `json:"compiler_version"`
	CreatedAt                string                 `json:"created_at"`
	SourceObjects            []SourceObjectRef      `json:"source_objects"`
	IncludedRanges           []ContextRange         `json:"included_ranges"`
	ExcludedRanges           []ContextRange         `json:"excluded_ranges"`
	CompressionPolicyVersion string                 `json:"compression_policy_version"`
	CompressionPolicyHash    string                 `json:"compression_policy_hash"`
	TokenAccounting          TokenAccounting        `json:"token_accounting"`
	SemanticInvariants       []string               `json:"semantic_invariants"`
	CodeSymbols              []string               `json:"code_symbols,omitempty"`
	MemoryReferences         []string               `json:"memory_references,omitempty"`
	Validation               ValidationResults      `json:"validation"`
	RestorationHandles       []RestorationHandle    `json:"restoration_handles"`
	Transformations          []TransformationRecord `json:"transformations"`
}

type SourceObjectRef struct {
	ObjectID    string `json:"object_id"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
}

// ContextRange is a half-open byte range [Start, End) of the stored source
// object.
type ContextRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// TokenAccounting reports every count with tokenMethod. Original is the raw
// input, Compressed is the compiled text exactly as returned (segment
// headers included), Included and Excluded are the source bytes that do and
// do not appear verbatim in the compiled text.
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

// TransformationRecord is one transformation that changed this input.
// SourceRanges are the source bytes it acted on; OriginalTokens is their
// token count and CompiledTokens the tokens they became in the compiled
// text. segment_framing adds the per-segment headers and has no source
// range.
type TransformationRecord struct {
	Name           string         `json:"name"`
	SourceRanges   []ContextRange `json:"source_ranges"`
	OriginalTokens int            `json:"original_tokens"`
	CompiledTokens int            `json:"compiled_tokens"`
}

type InputOptimiseResponse struct {
	Capsule      ContextCapsule `json:"capsule"`
	CompiledText string         `json:"compiled_text"`
	Validated    bool           `json:"validated"`
}

// OutputOptimiseRequest compacts a model reply. Format is "standard"
// (default) or "compact"; MaxTokens 0 selects 2000 and a negative value is
// rejected.
type OutputOptimiseRequest struct {
	RawOutput string `json:"raw_output"`
	Format    string `json:"format,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

type OutputOptimiseResponse struct {
	Optimised         string `json:"optimised"`
	Reduction         int    `json:"token_reduction"`
	OriginalTokens    int    `json:"original_tokens"`
	OptimisedTokens   int    `json:"optimised_tokens"`
	TokenMethod       string `json:"token_method"`
	SourceContentHash string `json:"source_content_hash"`
	CapsuleID         string `json:"capsule_id"`
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
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("context-optimisation", auth.ControlPlaneHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

// contextRestoreRequest names a capsule by id, or supplies a whole capsule
// whose source objects must match what was recorded for its id.
type contextRestoreRequest struct {
	CapsuleID string          `json:"capsule_id,omitempty"`
	Capsule   *ContextCapsule `json:"capsule,omitempty"`
}

// SourceStore is the authority for content referenced by a capsule. Callers
// cannot provide replacement content during restoration.
type SourceStore interface {
	Put(objectID, version, contentHash, content string) error
	Get(objectID, version string) (string, error)
	// Ready reports whether the store can currently persist content.
	Ready() error
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

// Ready creates the store root the same way Put does, then proves a file can
// be written there.
func (s *fileSourceStore) Ready() error {
	if s.root == "" {
		return errors.New("context source store is not configured")
	}
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return errors.New("context source store is unavailable")
	}
	return readiness.WritableDir(s.root)
}

// defaultSourceStorePath is the durable location both reference deployments
// (Compose volume and native systemd units) mount. The store is the
// authority for context restoration, so it must never default to a
// temporary directory that is lost on restart.
const defaultSourceStorePath = "/var/lib/scos/context/sources"

func defaultSourceStore() SourceStore {
	root := os.Getenv("SCOS_CONTEXT_SOURCE_STORE")
	if root == "" {
		root = defaultSourceStorePath
	}
	return newFileSourceStore(root)
}

var authoritativeSourceStore SourceStore = defaultSourceStore()

// ── capsule manifests ────────────────────────────────────────────────────

// manifestVersion is the store version under which a capsule's manifest is
// kept, next to the source objects it names.
const manifestVersion = "manifest"

// capsuleManifest is what the store records for a capsule id. The id is a
// hash of the other fields, so a manifest edited on disk no longer matches
// the id it is filed under.
type capsuleManifest struct {
	CapsuleID           string            `json:"capsule_id"`
	Kind                string            `json:"kind"`
	SourceObjects       []SourceObjectRef `json:"source_objects"`
	PolicyHash          string            `json:"compression_policy_hash"`
	CompiledContentHash string            `json:"compiled_content_hash"`
}

var (
	errCapsuleUnknown  = errors.New("capsule is not recorded")
	errCapsuleTampered = errors.New("capsule manifest failed integrity validation")
	errSourceMissing   = errors.New("authoritative source is missing")
	errSourceTampered  = errors.New("authoritative source failed integrity validation")
)

// deriveCapsuleID names a capsule after what it contains: the kind of
// optimisation, the source, the policy and the compiled result.
func deriveCapsuleID(kind, sourceHash, policyHash, compiledHash string) string {
	return "capsule-" + sha256Hex([]byte(kind + "\x00" + sourceHash + "\x00" + policyHash + "\x00" + compiledHash))[:32]
}

func validCapsuleID(id string) bool {
	if len(id) != len("capsule-")+32 || !strings.HasPrefix(id, "capsule-") {
		return false
	}
	_, err := hex.DecodeString(id[len("capsule-"):])
	return err == nil
}

// sourceObjectFor is the store identity of a source with the given hash.
func sourceObjectFor(contentHash string) SourceObjectRef {
	return SourceObjectRef{ObjectID: "source-" + contentHash[:32], Version: "1", ContentHash: contentHash}
}

// persistCapsule stores the source content and then the manifest that makes
// the capsule id restorable.
func persistCapsule(store SourceStore, manifest capsuleManifest, content string) error {
	for _, source := range manifest.SourceObjects {
		if err := store.Put(source.ObjectID, source.Version, source.ContentHash, content); err != nil {
			return err
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return store.Put(manifest.CapsuleID, manifestVersion, sha256Hex(body), string(body))
}

// resolveCapsule loads a capsule's manifest and returns its verified source
// content.
func resolveCapsule(store SourceStore, capsuleID string) (capsuleManifest, string, error) {
	var manifest capsuleManifest
	body, err := store.Get(capsuleID, manifestVersion)
	if err != nil {
		return manifest, "", errCapsuleUnknown
	}
	if err := json.Unmarshal([]byte(body), &manifest); err != nil || len(manifest.SourceObjects) == 0 {
		return manifest, "", errCapsuleTampered
	}
	sourceHash := manifest.SourceObjects[0].ContentHash
	if manifest.CapsuleID != capsuleID || deriveCapsuleID(manifest.Kind, sourceHash, manifest.PolicyHash, manifest.CompiledContentHash) != capsuleID {
		return manifest, "", errCapsuleTampered
	}
	var restored strings.Builder
	for _, source := range manifest.SourceObjects {
		if !validSourceRef(source) {
			return manifest, "", errCapsuleTampered
		}
		content, err := store.Get(source.ObjectID, source.Version)
		if err != nil {
			return manifest, "", errSourceMissing
		}
		if sha256Hex([]byte(content)) != source.ContentHash {
			return manifest, "", errSourceTampered
		}
		restored.WriteString(content)
	}
	return manifest, restored.String(), nil
}

func sameSources(a, b []SourceObjectRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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
		"token_method":        tokenMethod,
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
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	capsuleID := req.CapsuleID
	if req.Capsule != nil {
		if capsuleID != "" && capsuleID != req.Capsule.CapsuleID {
			http.Error(w, "capsule_id does not match capsule", http.StatusBadRequest)
			return
		}
		capsuleID = req.Capsule.CapsuleID
	}
	if !validCapsuleID(capsuleID) {
		http.Error(w, "capsule_id or capsule is required", http.StatusBadRequest)
		return
	}
	manifest, content, err := resolveCapsule(authoritativeSourceStore, capsuleID)
	switch {
	case errors.Is(err, errCapsuleUnknown), errors.Is(err, errSourceMissing):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if req.Capsule != nil && !sameSources(req.Capsule.SourceObjects, manifest.SourceObjects) {
		http.Error(w, "capsule source objects do not match the recorded capsule", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{
		"capsule_id":          manifest.CapsuleID,
		"kind":                manifest.Kind,
		"restored":            true,
		"source_content_hash": manifest.SourceObjects[0].ContentHash,
		"content":             content,
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

// compressionProfiles are the §18.4 profiles the compressor implements.
var compressionProfiles = map[string]bool{
	"exact": true, "conservative": true, "balanced": true, "aggressive": true, "adaptive": true,
}

const defaultTokenBudget = 8000

// validateInputRequest rejects values the compiler would otherwise have to
// ignore or reinterpret.
func validateInputRequest(req InputOptimiseRequest) error {
	if req.Input == "" {
		return errors.New("input is required")
	}
	if requested := strings.ToLower(strings.TrimSpace(req.Profile)); requested != "" && !compressionProfiles[requested] {
		return fmt.Errorf("unknown profile %q: use exact, conservative, balanced, aggressive or adaptive", req.Profile)
	}
	if req.TokenBudget < 0 {
		return errors.New("token_budget must not be negative")
	}
	for _, pin := range pinnedReferences(req) {
		if strings.TrimSpace(pin) == "" {
			return errors.New("code_symbols and memory_references must not contain empty values")
		}
		if !strings.Contains(req.Input, pin) {
			return fmt.Errorf("pinned reference %q does not occur in input", pin)
		}
	}
	return nil
}

func pinnedReferences(req InputOptimiseRequest) []string {
	pins := make([]string, 0, len(req.CodeSymbols)+len(req.MemoryReferences))
	pins = append(pins, req.CodeSymbols...)
	return append(pins, req.MemoryReferences...)
}

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
	if err := validateInputRequest(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, report, err := compileContext(req, authoritativeSourceStore)
	if err != nil {
		http.Error(w, "failed to persist authoritative source", http.StatusInternalServerError)
		return
	}
	capsule := resp.Capsule

	// §32 context-optimisation measures, each measured on this compile. A
	// retention ratio is only emitted when there was something to retain.
	corr := telemetry.Correlation{}
	labels := map[string]string{"profile": capsule.Profile}
	if capsule.OriginalTokens > 0 {
		serviceMetrics.Gauge(telemetry.MetricTokenReduction, capsule.Reduction, "ratio", labels, corr)
	}
	if total := report.hintsTotal + report.pinsTotal; total > 0 {
		serviceMetrics.Gauge(telemetry.MetricInstructionRetention,
			float64(report.hintsRetained+report.pinsRetained)/float64(total), "ratio", labels, corr)
	}
	if report.exactTotal > 0 {
		serviceMetrics.Gauge(telemetry.MetricExactValueRetention,
			float64(report.exactRetained)/float64(report.exactTotal), "ratio", labels, corr)
	}
	// Restoration is measured by restoring the capsule just written.
	serviceMetrics.Gauge(telemetry.MetricRestorationSuccess,
		boolAsRatio(capsule.Validation.Checks["restoration_verified"]), "ratio", labels, corr)

	writeJSON(w, resp)
}

// compileContext runs the pipeline, persists the source and capsule
// manifest, and verifies the capsule restores before returning it.
func compileContext(req InputOptimiseRequest, store SourceStore) (InputOptimiseResponse, invariantReport, error) {
	profile := resolveProfile(req)
	tokenBudget := req.TokenBudget
	if tokenBudget == 0 {
		tokenBudget = defaultTokenBudget
	}
	pins := pinnedReferences(req)

	// 1. Canonicalise, keeping the origin of every byte.
	canon := canonicaliseMapped(req.Input)

	// 2. Classify segments; pinned references make a segment non-droppable.
	classified := classifySegments(canon.text, req.NonDroppableHints)
	for i := range classified {
		for _, pin := range pins {
			if strings.Contains(classified[i].Content, pin) {
				classified[i].Droppable = false
			}
		}
		classified[i].ContentHash = sha256Hex([]byte(classified[i].Content))
		classified[i].SourceRanges = canon.sourceRanges(classified[i].start, classified[i].end)
	}

	// 3. Exact deduplication (within input).
	kept, dropped := splitDuplicates(classified)

	// 4. Allocate budget per class.
	allocation := allocateBudget(tokenBudget, profile, kept)

	// 5. Compress per profile.
	compiled, compiledSegments := compressSegments(kept, profile, allocation)

	// 6. Invariant validation.
	report := checkInvariants(req.Input, compiled, req.NonDroppableHints, pins)

	// 7. Build, persist and verify the capsule.
	sourceHash := sha256Hex([]byte(req.Input))
	source := sourceObjectFor(sourceHash)
	policyVersion := "context-policy-2"
	policyHash := sha256Hex([]byte(policyVersion + ":" + profile + ":" + strconv.Itoa(tokenBudget)))
	capsuleID := deriveCapsuleID("input", sourceHash, policyHash, sha256Hex([]byte(compiled)))
	manifest := capsuleManifest{CapsuleID: capsuleID, Kind: "input", SourceObjects: []SourceObjectRef{source}, PolicyHash: policyHash, CompiledContentHash: sha256Hex([]byte(compiled))}
	if err := persistCapsule(store, manifest, req.Input); err != nil {
		return InputOptimiseResponse{}, report, err
	}
	stored, getErr := store.Get(source.ObjectID, source.Version)
	_, restored, restoreErr := resolveCapsule(store, capsuleID)

	included, excluded := canon.inclusion(compiledSegments)
	includedBytes := rangeBytes(included)
	originalTokens := estimateTokens(req.Input)
	compiledTokens := estimateTokens(compiled)
	reduction := 0.0
	if originalTokens > 0 {
		reduction = 1 - float64(compiledTokens)/float64(originalTokens)
	}
	checks := map[string]bool{
		"non_droppable_hints_retained": report.hintsRetained == report.hintsTotal,
		"exact_values_retained":        report.exactRetained == report.exactTotal,
		"pinned_references_retained":   report.pinsRetained == report.pinsTotal,
		"source_hash_recorded":         getErr == nil && sha256Hex([]byte(stored)) == sourceHash,
		"restoration_verified":         restoreErr == nil && restored == req.Input,
	}
	valid := true
	for _, ok := range checks {
		valid = valid && ok
	}
	capsule := ContextCapsule{
		CapsuleID:                capsuleID,
		Profile:                  profile,
		SourceContentHash:        sourceHash,
		OriginalTokens:           originalTokens,
		CompiledTokens:           compiledTokens,
		Reduction:                reduction,
		Segments:                 compiledSegments,
		Allocation:               allocation,
		TokenBudget:              tokenBudget,
		CompilerVersion:          version,
		CreatedAt:                time.Now().UTC().Format(time.RFC3339),
		SourceObjects:            []SourceObjectRef{source},
		IncludedRanges:           included,
		ExcludedRanges:           excluded,
		CompressionPolicyVersion: policyVersion,
		CompressionPolicyHash:    policyHash,
		TokenAccounting: TokenAccounting{
			Method:     tokenMethod,
			Original:   originalTokens,
			Compressed: compiledTokens,
			Included:   tokensForBytes(includedBytes),
			Excluded:   tokensForBytes(len(req.Input) - includedBytes),
		},
		SemanticInvariants: extractExactValues(req.Input),
		CodeSymbols:        req.CodeSymbols,
		MemoryReferences:   req.MemoryReferences,
		Validation:         ValidationResults{Valid: valid, Checks: checks},
		RestorationHandles: []RestorationHandle{{HandleID: capsuleID, ObjectID: source.ObjectID, ContentHash: sourceHash}},
		Transformations:    appliedTransformations(canon, classified, dropped, compiledSegments, compiled),
	}
	return InputOptimiseResponse{Capsule: capsule, CompiledText: compiled, Validated: valid}, report, nil
}

// boolAsRatio renders a pass/fail invariant as a 1/0 ratio.
func boolAsRatio(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

// ── output optimisation ──────────────────────────────────────────────────

// outputFormats are the reply formats optimiseOutput implements. A diff is
// deliberately not one: compaction drops blank and repeated lines, which
// corrupts a diff.
var outputFormats = map[string]bool{"standard": true, "compact": true}

const defaultOutputTokens = 2000

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
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "standard"
	}
	if !outputFormats[format] {
		http.Error(w, fmt.Sprintf("unknown format %q: use standard or compact", req.Format), http.StatusBadRequest)
		return
	}
	if req.MaxTokens < 0 {
		http.Error(w, "max_tokens must not be negative", http.StatusBadRequest)
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultOutputTokens
	}

	optimised := optimiseOutput(req.RawOutput, format, maxTokens)
	originalTokens := estimateTokens(req.RawOutput)
	optimisedTokens := estimateTokens(optimised)

	// The raw output is kept as the capsule's source, so the reply can be
	// restored in full from the capsule id.
	sourceHash := sha256Hex([]byte(req.RawOutput))
	policyHash := sha256Hex([]byte("output-policy-1:" + format + ":" + strconv.Itoa(maxTokens)))
	optimisedHash := sha256Hex([]byte(optimised))
	manifest := capsuleManifest{
		CapsuleID:           deriveCapsuleID("output", sourceHash, policyHash, optimisedHash),
		Kind:                "output",
		SourceObjects:       []SourceObjectRef{sourceObjectFor(sourceHash)},
		PolicyHash:          policyHash,
		CompiledContentHash: optimisedHash,
	}
	if err := persistCapsule(authoritativeSourceStore, manifest, req.RawOutput); err != nil {
		http.Error(w, "failed to persist authoritative source", http.StatusInternalServerError)
		return
	}

	writeJSON(w, OutputOptimiseResponse{
		Optimised:         optimised,
		Reduction:         originalTokens - optimisedTokens,
		OriginalTokens:    originalTokens,
		OptimisedTokens:   optimisedTokens,
		TokenMethod:       tokenMethod,
		SourceContentHash: sourceHash,
		CapsuleID:         manifest.CapsuleID,
	})
}

// ── pipeline stages ──────────────────────────────────────────────────────

// canonicalText is the canonicalised input together with where each of its
// bytes came from, so capsule ranges address the stored original input.
type canonicalText struct {
	input string
	text  string
	// origin[i] is the input offset of the byte(s) that produced text[i].
	origin []int
	// rewritten are the input ranges canonicalisation removed or replaced;
	// produced is how many canonical bytes replaced them.
	rewritten []ContextRange
	produced  int
}

// canonicaliseMapped removes a leading BOM, normalises CRLF and CR to LF and
// strips trailing spaces and tabs from every line.
func canonicaliseMapped(input string) canonicalText {
	buf := make([]byte, 0, len(input))
	origin := make([]int, 0, len(input))
	var rewritten []ContextRange
	produced := 0
	lineStart := 0
	trimLine := func() {
		n := len(buf)
		for n > lineStart && (buf[n-1] == ' ' || buf[n-1] == '\t') {
			n--
		}
		if n < len(buf) {
			rewritten = appendRange(rewritten, origin[n], origin[len(buf)-1]+1)
			buf = buf[:n]
			origin = origin[:n]
		}
	}
	i := 0
	if strings.HasPrefix(input, "\ufeff") {
		rewritten = appendRange(rewritten, 0, len("\ufeff"))
		i = len("\ufeff")
	}
	for i < len(input) {
		switch c := input[i]; c {
		case '\r':
			trimLine()
			width := 1
			if i+1 < len(input) && input[i+1] == '\n' {
				width = 2
			}
			rewritten = appendRange(rewritten, i, i+width)
			produced++
			buf = append(buf, '\n')
			origin = append(origin, i)
			i += width
			lineStart = len(buf)
		case '\n':
			trimLine()
			buf = append(buf, '\n')
			origin = append(origin, i)
			i++
			lineStart = len(buf)
		default:
			buf = append(buf, c)
			origin = append(origin, i)
			i++
		}
	}
	trimLine()
	return canonicalText{input: input, text: string(buf), origin: origin, rewritten: rewritten, produced: produced}
}

func canonicalise(input string) string {
	return canonicaliseMapped(input).text
}

// originEnd is the input offset just past the byte(s) that produced
// canonical byte i: a CRLF became one LF.
func (c canonicalText) originEnd(i int) int {
	o := c.origin[i]
	if c.input[o] == '\r' && o+1 < len(c.input) && c.input[o+1] == '\n' {
		return o + 2
	}
	return o + 1
}

// sourceRanges maps the canonical range [start, end) to the input ranges it
// was produced from, leaving out bytes canonicalisation removed.
func (c canonicalText) sourceRanges(start, end int) []ContextRange {
	ranges := []ContextRange{}
	for i := start; i < end; i++ {
		ranges = appendRange(ranges, c.origin[i], c.originEnd(i))
	}
	return ranges
}

// inclusion returns the input ranges copied verbatim into the compiled
// segments and their complement.
func (c canonicalText) inclusion(compiled []Segment) (included, excluded []ContextRange) {
	included = []ContextRange{}
	for _, seg := range compiled {
		for _, r := range c.sourceRanges(seg.start, seg.start+seg.retained) {
			included = appendRange(included, r.Start, r.End)
		}
	}
	included = normaliseRanges(included)
	return included, complementRanges(included, len(c.input))
}

// appendRange appends [start, end), merging it into the last range when they
// touch.
func appendRange(ranges []ContextRange, start, end int) []ContextRange {
	if end <= start {
		return ranges
	}
	if n := len(ranges); n > 0 && ranges[n-1].End == start {
		ranges[n-1].End = end
		return ranges
	}
	return append(ranges, ContextRange{Start: start, End: end})
}

// normaliseRanges sorts and merges overlapping or touching ranges.
func normaliseRanges(ranges []ContextRange) []ContextRange {
	sorted := append([]ContextRange{}, ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	merged := []ContextRange{}
	for _, r := range sorted {
		if n := len(merged); n > 0 && r.Start <= merged[n-1].End {
			if r.End > merged[n-1].End {
				merged[n-1].End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// complementRanges returns [0, length) minus the sorted, merged ranges.
func complementRanges(ranges []ContextRange, length int) []ContextRange {
	out := []ContextRange{}
	next := 0
	for _, r := range ranges {
		out = appendRange(out, next, r.Start)
		next = r.End
	}
	return appendRange(out, next, length)
}

func rangeBytes(ranges []ContextRange) int {
	n := 0
	for _, r := range ranges {
		n += r.End - r.Start
	}
	return n
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
			if part.content == "" {
				continue
			}
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
				start:          part.start,
				end:            part.end,
			})
		}
	} else if text != "" {
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
			start:          0,
			end:            len(text),
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

// codeBlockPart is a slice of the canonical text; start and end are its
// offsets there. Fence lines belong to no part.
type codeBlockPart struct {
	content    string
	isCode     bool
	start, end int
}

func splitCodeBlocks(input string) []codeBlockPart {
	var parts []codeBlockPart
	base := 0
	for {
		codeStart := strings.Index(input, "```")
		if codeStart == -1 {
			parts = append(parts, codeBlockPart{content: input, start: base, end: base + len(input)})
			break
		}
		// Text before code block
		if codeStart > 0 {
			parts = append(parts, codeBlockPart{content: input[:codeStart], start: base, end: base + codeStart})
		}
		rest := input[codeStart+3:]
		restBase := base + codeStart + 3
		langEnd := strings.Index(rest, "\n")
		if langEnd == -1 {
			parts = append(parts, codeBlockPart{content: rest, isCode: true, start: restBase, end: restBase + len(rest)})
			break
		}
		codeEnd := strings.Index(rest, "\n```")
		if codeEnd == -1 {
			parts = append(parts, codeBlockPart{content: rest, isCode: true, start: restBase, end: restBase + len(rest)})
			break
		}
		// An empty block ("```\n```") shares one newline between the opening
		// line and the closing fence.
		contentStart := langEnd + 1
		if codeEnd < contentStart {
			contentStart = codeEnd
		}
		parts = append(parts, codeBlockPart{content: rest[contentStart:codeEnd], isCode: true, start: restBase + contentStart, end: restBase + codeEnd})
		input = rest[codeEnd+4:]
		base = restBase + codeEnd + 4
	}
	return parts
}

// ── dedup ────────────────────────────────────────────────────────────────

func deduplicateSegments(segments []Segment) []Segment {
	kept, _ := splitDuplicates(segments)
	return kept
}

// splitDuplicates keeps the first segment with each trimmed content and
// returns the later copies separately.
func splitDuplicates(segments []Segment) (kept, dropped []Segment) {
	seen := make(map[string]bool)
	for _, s := range segments {
		hash := sha256Hex([]byte(strings.TrimSpace(s.Content)))
		if seen[hash] {
			dropped = append(dropped, s)
			continue
		}
		seen[hash] = true
		kept = append(kept, s)
	}
	return kept, dropped
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

	// Background segments draw on the evidence allocation.
	otherTokens := 0
	for _, seg := range segments {
		switch seg.Class {
		case "governing_policy", "current_user_instruction", "active_task_state", "acceptance_criteria",
			"confirmed_memory", "evidence", "code_symbol", "code_dependency", "tool_schema":
		default:
			otherTokens += seg.OriginalTokens
		}
	}
	allocation.Evidence += otherTokens
	return allocation
}

// ── compression (§18.3, §18.4) ───────────────────────────────────────────

const (
	transformPassThrough = "pass_through"
	transformTruncation  = "truncation"
	transformReference   = "reference_substitution"
	transformSummary     = "extractive_summary"
)

// truncationMarker is appended where balanced compression cut a segment.
const truncationMarker = "\n<!-- truncated; full content in capsule -->"

// passThrough marks a segment copied unchanged.
func passThrough(seg Segment) Segment {
	return transformed(seg, seg.Content, transformPassThrough, len(seg.Content))
}

// transformed records that seg was compiled to content, of which the first
// retained bytes are the segment's source copied verbatim.
func transformed(seg Segment, content, transformation string, retained int) Segment {
	seg.Content = content
	seg.CompiledTokens = estimateTokens(content)
	seg.Transformation = transformation
	seg.retained = retained
	return seg
}

func compressSegments(segments []Segment, profile string, allocation BudgetAllocation) (string, []Segment) {
	var compressed []Segment
	totalUsed := 0
	reserveRemaining := allocation.Reserve

	for _, seg := range segments {
		// Determine budget for this segment's class
		classBudget := classBudgetFor(seg.Class, allocation)

		// Never drop non-droppable classes
		if !seg.Droppable {
			compressed = append(compressed, passThrough(seg))
			totalUsed += seg.OriginalTokens
			continue
		}

		// Apply profile-based compression
		switch profile {
		case "exact", "conservative":
			// Only canonicalisation and exact deduplication; segment content
			// is never altered.
			compressed = append(compressed, passThrough(seg))
			totalUsed += seg.OriginalTokens

		case "balanced":
			if classBudget > 0 && totalUsed+seg.OriginalTokens <= classBudget+reserveRemaining {
				if len(seg.Content) > 1000 {
					cut := runeBoundary(seg.Content, 1000)
					next := transformed(seg, seg.Content[:cut]+truncationMarker, transformTruncation, cut)
					compressed = append(compressed, next)
					totalUsed += next.CompiledTokens
				} else {
					compressed = append(compressed, passThrough(seg))
					totalUsed += seg.OriginalTokens
				}
			} else {
				// Droppable class, over budget → replace with reference
				ref := "[Reference: " + seg.Class + " hash=" + sha256Hex([]byte(seg.Content))[:12] + "]"
				next := transformed(seg, ref, transformReference, 0)
				compressed = append(compressed, next)
				totalUsed += next.CompiledTokens
			}

		case "aggressive":
			// Strong reduction: summarise to one line per segment
			if classBudget > 0 && totalUsed+seg.OriginalTokens <= classBudget+reserveRemaining {
				compressed = append(compressed, passThrough(seg))
				totalUsed += seg.OriginalTokens
			} else {
				summary, kept := summarise(seg.Content, seg.Class)
				next := transformed(seg, summary, transformSummary, kept)
				compressed = append(compressed, next)
				totalUsed += next.CompiledTokens
			}

		default: // adaptive — bounded deterministic policy selection per segment
			if seg.OriginalTokens > 250 {
				summary, kept := summarise(seg.Content, seg.Class)
				next := transformed(seg, summary, transformSummary, kept)
				compressed = append(compressed, next)
				totalUsed += next.CompiledTokens
			} else {
				compressed = append(compressed, passThrough(seg))
				totalUsed += seg.OriginalTokens
			}
		}
	}

	// Assemble compiled text
	var parts []string
	for _, seg := range compressed {
		parts = append(parts, segmentHeader(seg.Class)+seg.Content)
	}
	return strings.Join(parts, segmentSeparator), compressed
}

const segmentSeparator = "\n\n"

func segmentHeader(class string) string {
	return "--- " + class + " ---\n"
}

// ── applied transformations ──────────────────────────────────────────────

// appliedTransformations reports, in pipeline order, only the
// transformations that changed this input.
func appliedTransformations(canon canonicalText, classified, dropped, compiled []Segment, compiledText string) []TransformationRecord {
	records := []TransformationRecord{}

	if len(canon.rewritten) > 0 {
		records = append(records, TransformationRecord{
			Name:           "canonicalisation",
			SourceRanges:   canon.rewritten,
			OriginalTokens: tokensForBytes(rangeBytes(canon.rewritten)),
			CompiledTokens: tokensForBytes(canon.produced),
		})
	}

	// Code fence lines belong to no segment: they are the canonical bytes
	// outside every classified segment.
	covered := []ContextRange{}
	for _, seg := range classified {
		covered = append(covered, ContextRange{Start: seg.start, End: seg.end})
	}
	var fences []ContextRange
	for _, gap := range complementRanges(normaliseRanges(covered), len(canon.text)) {
		for _, r := range canon.sourceRanges(gap.Start, gap.End) {
			fences = appendRange(fences, r.Start, r.End)
		}
	}
	if len(fences) > 0 {
		records = append(records, TransformationRecord{
			Name:           "code_fence_removal",
			SourceRanges:   fences,
			OriginalTokens: tokensForBytes(rangeBytes(fences)),
		})
	}

	if len(dropped) > 0 {
		record := TransformationRecord{Name: "exact_deduplication", SourceRanges: []ContextRange{}}
		for _, seg := range dropped {
			record.SourceRanges = append(record.SourceRanges, seg.SourceRanges...)
			record.OriginalTokens += seg.OriginalTokens
		}
		record.SourceRanges = normaliseRanges(record.SourceRanges)
		records = append(records, record)
	}

	for _, name := range []string{transformTruncation, transformReference, transformSummary} {
		record := TransformationRecord{Name: name, SourceRanges: []ContextRange{}}
		for _, seg := range compiled {
			if seg.Transformation != name {
				continue
			}
			record.SourceRanges = append(record.SourceRanges, seg.SourceRanges...)
			record.OriginalTokens += seg.OriginalTokens
			record.CompiledTokens += seg.CompiledTokens
		}
		if len(record.SourceRanges) > 0 {
			record.SourceRanges = normaliseRanges(record.SourceRanges)
			records = append(records, record)
		}
	}

	// Segment headers and separators are the only compiled bytes that do
	// not come from a segment.
	if len(compiled) > 0 {
		contentBytes := 0
		for _, seg := range compiled {
			contentBytes += len(seg.Content)
		}
		records = append(records, TransformationRecord{
			Name:           "segment_framing",
			SourceRanges:   []ContextRange{},
			CompiledTokens: tokensForBytes(len(compiledText) - contentBytes),
		})
	}
	return records
}

// ── validation (§18.3 invariant 14) ──────────────────────────────────────

// invariantReport counts, per invariant, how many required values the
// compiled text retained.
type invariantReport struct {
	hintsTotal, hintsRetained int
	exactTotal, exactRetained int
	pinsTotal, pinsRetained   int
}

func (r invariantReport) holds() bool {
	return r.hintsRetained == r.hintsTotal && r.exactRetained == r.exactTotal && r.pinsRetained == r.pinsTotal
}

func checkInvariants(original, compiled string, hints, pins []string) invariantReport {
	var report invariantReport
	for _, hint := range hints {
		report.hintsTotal++
		if strings.Contains(compiled, hint) {
			report.hintsRetained++
		}
	}
	// Exact identifiers, paths and versions must be preserved.
	for _, val := range extractExactValues(original) {
		report.exactTotal++
		if strings.Contains(compiled, val) {
			report.exactRetained++
		}
	}
	for _, pin := range pins {
		report.pinsTotal++
		if strings.Contains(compiled, pin) {
			report.pinsRetained++
		}
	}
	return report
}

func validateInvariants(original string, compiled string, hints []string) bool {
	return checkInvariants(original, compiled, hints, nil).holds()
}

func extractExactValues(text string) []string {
	values := []string{}
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
	if format == "compact" {
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
		for _, hedge := range []string{"i think ", "i believe "} {
			if strings.HasPrefix(lower, hedge) {
				trimmed = trimmed[len(hedge):]
				break
			}
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

// ── utility ──────────────────────────────────────────────────────────────

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// tokenMethod names the single token estimate used for every count this
// service reports: UTF-8 bytes divided by four, rounded up, so any non-empty
// text costs at least one token.
const tokenMethod = "utf8_bytes_div_4_ceil"

func tokensForBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

func estimateTokens(text string) int {
	return tokensForBytes(len(text))
}

func percentOf(total, pct int) int {
	return total * pct / 100
}

// runeBoundary returns the largest offset <= n that does not split a UTF-8
// sequence in s.
func runeBoundary(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
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

// summarise keeps the first sentence (or the first 200 bytes) and reports how
// many leading bytes of content it copied verbatim.
func summarise(content string, class string) (string, int) {
	kept := len(content)
	if idx := strings.Index(content, "."); idx > 0 && idx < 300 {
		kept = idx + 1
	}
	suffix := ""
	if kept > 200 {
		kept = runeBoundary(content, 200)
		suffix = "..."
	}
	return "[" + class + " summary] " + content[:kept] + suffix, kept
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
// validateInputRequest has already rejected unknown profile names.
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
