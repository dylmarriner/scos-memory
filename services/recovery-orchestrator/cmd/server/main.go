package main
import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"strconv"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("recovery-orchestrator")

const version = "0.1.0"

// ── recovery types (§30) ────────────────────────────────────────────────

type RecoveryChecklist []ChecklistItem

type ChecklistItem struct {
	Step      int    `json:"step"`
	Name      string `json:"name"`
	Status    string `json:"status"` // pending, verifying, ok, failed, skipped
	Detail    string `json:"detail,omitempty"`
	Completed string `json:"completed_at,omitempty"`
}

type RecoveryStatus struct {
	RecoveryRunID    string            `json:"recovery_run_id,omitempty"`
	ServiceID        string            `json:"service_id"`
	Phase            string            `json:"phase"` // startup, recovery, completed, failed
	Checklist        RecoveryChecklist `json:"checklist"`
	InterruptedTurns []string          `json:"interrupted_turns"`
	OpenOperations   []string          `json:"open_operations"`
	PendingOutbox    int               `json:"pending_outbox_jobs"`
	RecoveryStarted  string            `json:"recovery_started"`
	RecoveryEnded    string            `json:"recovery_ended,omitempty"`
}

// ── server ───────────────────────────────────────────────────────────────

type recoveryServer struct {
	mu         sync.RWMutex
	statuses   map[string]*RecoveryStatus
	readyCheck func(context.Context) error
	// discover runs every recovery check and corrective action for one
	// service and reports what it found and did.
	discover func(ctx context.Context, serviceID string) (recoveryFindings, error)
	db       *pgxpool.Pool
}

type recoveryFindings struct {
	interruptedTurns []string
	openOperations   []string
	pendingOutbox    int
	staleRequeued    int
	deadLetters      int
	storageOK        bool
	storageDetail    string
	okfOK            bool
	okfDetail        string
	checkpointDetail string
	leasesReset      int
	runsSuperseded   int
}

func newRecoveryServer() *recoveryServer {
	return &recoveryServer{
		statuses: make(map[string]*RecoveryStatus),
	}
}

func main() {
	s := newRecoveryServer()
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		if err := database.RequireTLS(databaseURL); err != nil {
			log.Fatal(err)
		}
		db, err := database.NewPool(context.Background(), databaseURL)
		if err != nil {
			log.Fatalf("connect to database: %v", err)
		}
		s.readyCheck = func(ctx context.Context) error {
			if err := db.Ping(ctx); err != nil {
				return err
			}
			var schema string
			return db.QueryRow(ctx, `SELECT to_regclass('public.turns')`).Scan(&schema)
		}
		s.db = db
		s.discover = func(ctx context.Context, serviceID string) (recoveryFindings, error) {
			return discoverRecoveryFindings(ctx, db, serviceID)
		}
		defer db.Close()
	}
	mux := newRecoveryMux(s)

	addr := os.Getenv("SCOS_RECOVERY_ORCHESTRATOR_HTTP_ADDR")
	if addr == "" {
		addr = ":8094"
	}
	log.Printf("recovery orchestrator listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("recovery-orchestrator", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

// newRecoveryMux wires the routes, kept separate from main so tests can
// exercise the auth.RequireScope gates below without a listening server.
func newRecoveryMux(s *recoveryServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("recovery-orchestrator"))
	mux.HandleFunc("/health/ready", s.handleReady)

	// Mutating recovery routes are operator-only: without a scope gate any
	// authenticated caller could mark a disaster-recovery checklist
	// "completed" without a single step having run, masking a degraded
	// service. Read-only status routes below stay open to any authenticated
	// caller, matching the sibling repository's AUDIT-007 remediation.
	mux.Handle("/v1/recovery/start", auth.RequireScope("recovery:admin", http.HandlerFunc(s.handleRecoveryStart)))
	mux.Handle("/v1/recovery/checklist", auth.RequireScope("recovery:admin", http.HandlerFunc(s.handleChecklistUpdate)))
	mux.Handle("/v1/recovery/complete", auth.RequireScope("recovery:admin", http.HandlerFunc(s.handleRecoveryComplete)))
	mux.HandleFunc("/v1/recovery/status", s.handleRecoveryStatus)
	mux.HandleFunc("/v1/recovery/status/", s.handleRecoveryStatusByID)
	mux.HandleFunc("/v1/recovery/interrupted-turns", s.handleInterruptedTurns)
	return mux
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *recoveryServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.readyCheck == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "dependency": "postgres"})
		return
	}
	if err := s.readyCheck(r.Context()); err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "dependency": "postgres"})
		return
	}
	writeJSON(w, map[string]string{"status": "ready"})
}

func (s *recoveryServer) handleRecoveryStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ServiceID string `json:"service_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ServiceID == "" {
		req.ServiceID = "default"
	}

	status := &RecoveryStatus{
		ServiceID:       req.ServiceID,
		Phase:           "startup",
		RecoveryStarted: time.Now().UTC().Format(time.RFC3339),
		Checklist: RecoveryChecklist{
			{Step: 1, Name: "Verify schema and database connectivity", Status: "pending"},
			{Step: 2, Name: "Verify required storage", Status: "pending"},
			{Step: 3, Name: "Confirm OKF repository state", Status: "pending"},
			{Step: 4, Name: "Load latest checkpoints", Status: "pending"},
			{Step: 5, Name: "Identify interrupted turns", Status: "pending"},
			{Step: 6, Name: "Identify open tool operations", Status: "pending"},
			{Step: 7, Name: "Resume pending outbox jobs", Status: "pending"},
			{Step: 8, Name: "Reset abandoned leases", Status: "pending"},
			{Step: 9, Name: "Rebuild temporary caches", Status: "pending"},
			{Step: 10, Name: "Mark readiness", Status: "pending"},
		},
	}
	if s.discover != nil {
		findings, err := s.discover(r.Context(), req.ServiceID)
		if err != nil {
			status.Phase = "failed"
			status.Checklist[0].Status = "failed"
			status.Checklist[0].Detail = err.Error()
		} else {
			applyFindings(status, findings)
		}
	}
	if err := s.persistRecoveryStatus(r.Context(), status, "recovery_started"); err != nil {
		status.Phase = "failed"
		status.Checklist[0].Status = "failed"
		status.Checklist[0].Detail = "persist recovery run: " + err.Error()
	}

	s.mu.Lock()
	s.statuses[req.ServiceID] = status
	s.mu.Unlock()

	writeJSON(w, status)
}

// operatorResolvableSteps are the checks whose findings an operator works
// off by hand (reconciling interrupted turns and open tool operations). Every
// other step is verified or performed by the orchestrator itself on start.
var operatorResolvableSteps = map[int]bool{5: true, 6: true}

// handleChecklistUpdate lets an operator record progress on a step. "ok" is
// never taken on trust: it is accepted only for an operator-resolvable step,
// and only after the orchestrator re-runs discovery and finds nothing left
// for that step. Any step may be marked "failed" or "verifying" with detail.
func (s *recoveryServer) handleChecklistUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ServiceID string `json:"service_id"`
		Step      int    `json:"step"`
		Status    string `json:"status"`
		Detail    string `json:"detail,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Status {
	case "ok", "failed", "verifying":
	default:
		http.Error(w, "status must be ok, failed or verifying", http.StatusBadRequest)
		return
	}
	if req.Step == 10 {
		http.Error(w, "readiness is marked by /v1/recovery/complete", http.StatusConflict)
		return
	}
	s.mu.RLock()
	_, ok := s.statuses[req.ServiceID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	detail := req.Detail
	if req.Status == "ok" {
		if !operatorResolvableSteps[req.Step] {
			http.Error(w, fmt.Sprintf("step %d is verified by the orchestrator; restart recovery to re-run it", req.Step), http.StatusConflict)
			return
		}
		if s.discover == nil {
			http.Error(w, "cannot verify the step: recovery database is not configured", http.StatusServiceUnavailable)
			return
		}
		findings, err := s.discover(r.Context(), req.ServiceID)
		if err != nil {
			http.Error(w, "re-verify step: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		remaining := len(findings.interruptedTurns)
		noun := "interrupted turn"
		if req.Step == 6 {
			remaining, noun = len(findings.openOperations), "open operation"
		}
		if remaining > 0 {
			http.Error(w, fmt.Sprintf("step %d is not resolved: %s", req.Step, countDetail(remaining, noun)), http.StatusConflict)
			return
		}
		verified := "re-verified: " + countDetail(0, noun)
		if detail == "" {
			detail = verified
		} else {
			detail = verified + "; " + detail
		}
	}
	s.mu.Lock()
	status, ok := s.statuses[req.ServiceID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	found := false
	for i := range status.Checklist {
		if status.Checklist[i].Step == req.Step {
			found = true
			status.Checklist[i].Status = req.Status
			status.Checklist[i].Detail = detail
			if req.Status == "ok" || req.Status == "failed" {
				status.Checklist[i].Completed = time.Now().UTC().Format(time.RFC3339)
			}
			break
		}
	}
	s.mu.Unlock()
	if !found {
		http.Error(w, "unknown checklist step", http.StatusBadRequest)
		return
	}
	if err := s.persistRecoveryStatus(r.Context(), status, "checklist_updated"); err != nil {
		http.Error(w, "persist recovery checklist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func (s *recoveryServer) handleRecoveryComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ServiceID string `json:"service_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	status, ok := s.statuses[req.ServiceID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !checklistReady(status.Checklist) {
		s.mu.Unlock()
		http.Error(w, "mandatory recovery checks are not complete", http.StatusConflict)
		return
	}
	status.Phase = "completed"
	status.RecoveryEnded = time.Now().UTC().Format(time.RFC3339)
	setRecoveryItem(status, 10, "ok", "all recovery checks verified; readiness marked")
	interrupted := len(status.InterruptedTurns)
	s.mu.Unlock()
	// §32 recoveries: count each completed recovery, labelled by whether it
	// had interrupted turns to reconcile.
	serviceMetrics.Count(telemetry.MetricRecoveries, map[string]string{
		"service":           req.ServiceID,
		"had_interruptions": strconv.FormatBool(interrupted > 0),
	}, telemetry.Correlation{})
	if err := s.persistRecoveryStatus(r.Context(), status, "recovery_completed"); err != nil {
		http.Error(w, "persist recovery completion: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func applyFindings(status *RecoveryStatus, findings recoveryFindings) {
	status.InterruptedTurns = findings.interruptedTurns
	status.OpenOperations = findings.openOperations
	status.PendingOutbox = findings.pendingOutbox
	setRecoveryItem(status, 1, "ok", "database and schema available")
	setRecoveryItem(status, 2, okOrFailed(findings.storageOK), findings.storageDetail)
	setRecoveryItem(status, 3, okOrFailed(findings.okfOK), findings.okfDetail)
	setRecoveryItem(status, 4, "ok", findings.checkpointDetail)
	setRecoveryItem(status, 5, findingStatus(len(findings.interruptedTurns)), countDetail(len(findings.interruptedTurns), "interrupted turn"))
	setRecoveryItem(status, 6, findingStatus(len(findings.openOperations)), countDetail(len(findings.openOperations), "open operation"))
	outbox := fmt.Sprintf("%s requeued from expired worker leases; %s available to workers",
		countNoun(findings.staleRequeued, "stale job"), countNoun(findings.pendingOutbox, "pending job"))
	if findings.deadLetters > 0 {
		outbox += "; " + countNoun(findings.deadLetters, "dead letter") + " awaiting operator review"
	}
	setRecoveryItem(status, 7, "ok", outbox)
	setRecoveryItem(status, 8, "ok", leaseResetDetail(findings.leasesReset))
	setRecoveryItem(status, 9, "ok", fmt.Sprintf("recovery state cache rebuilt; %s superseded", countNoun(findings.runsSuperseded, "abandoned recovery run")))
	setRecoveryItem(status, 10, "pending", "complete recovery checks before marking readiness")
	status.Phase = "recovery"
}

func okOrFailed(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func countNoun(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

func setRecoveryItem(status *RecoveryStatus, step int, state, detail string) {
	for i := range status.Checklist {
		if status.Checklist[i].Step == step {
			status.Checklist[i].Status = state
			status.Checklist[i].Detail = detail
			if state == "ok" {
				status.Checklist[i].Completed = time.Now().UTC().Format(time.RFC3339)
			}
		}
	}
}

func findingStatus(count int) string {
	if count == 0 {
		return "ok"
	}
	return "verifying"
}

func countDetail(count int, noun string) string {
	suffix := "s"
	if count == 1 {
		suffix = ""
	}
	return fmt.Sprintf("%d %s%s discovered", count, noun, suffix)
}

func leaseResetDetail(count int) string {
	if count == 0 {
		return "no abandoned leases found"
	}
	suffix := "s"
	if count == 1 {
		suffix = ""
	}
	return fmt.Sprintf("%d abandoned lease%s reset", count, suffix)
}

func checklistReady(checklist RecoveryChecklist) bool {
	for _, item := range checklist {
		if item.Step != 10 && item.Status != "ok" {
			return false
		}
	}
	return true
}

// discoverRecoveryFindings runs every database-backed recovery check for
// serviceID. Fabric-wide facts come from the scos_recovery_* SECURITY
// DEFINER functions (migrations/000040): this service connects as the
// restricted scos_app role without a tenant context, under which direct
// queries against the tenant-scoped tables would see nothing.
func discoverRecoveryFindings(ctx context.Context, db *pgxpool.Pool, serviceID string) (recoveryFindings, error) {
	var findings recoveryFindings

	// Step 7: requeue jobs whose worker died mid-processing, then count what
	// is waiting so the detail reflects the post-recovery queue.
	if err := db.QueryRow(ctx, `SELECT scos_recovery_requeue_stale_outbox()`).Scan(&findings.staleRequeued); err != nil {
		return findings, fmt.Errorf("requeue stale outbox jobs: %w", err)
	}
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT scos_recovery_findings()`).Scan(&raw); err != nil {
		return findings, fmt.Errorf("read recovery findings: %w", err)
	}
	var fabric struct {
		InterruptedTurns []string   `json:"interrupted_turns"`
		OpenOperations   []string   `json:"open_operations"`
		PendingOutbox    int        `json:"pending_outbox"`
		DeadLetters      int        `json:"dead_letters"`
		CheckpointCount  int        `json:"checkpoint_count"`
		LatestCheckpoint *time.Time `json:"latest_checkpoint"`
		OKFLatest        *struct {
			CommitHash string          `json:"commit_hash"`
			Errors     json.RawMessage `json:"errors"`
		} `json:"okf_latest"`
	}
	if err := json.Unmarshal(raw, &fabric); err != nil {
		return findings, fmt.Errorf("decode recovery findings: %w", err)
	}
	findings.interruptedTurns = fabric.InterruptedTurns
	findings.openOperations = fabric.OpenOperations
	findings.pendingOutbox = fabric.PendingOutbox
	findings.deadLetters = fabric.DeadLetters

	// Step 3: OKF repository state from the most recent sync bundle.
	if fabric.OKFLatest == nil {
		findings.okfOK = true
		findings.okfDetail = "no OKF sync recorded yet"
	} else {
		var okfErrList []any
		if len(fabric.OKFLatest.Errors) > 0 && string(fabric.OKFLatest.Errors) != "null" {
			if err := json.Unmarshal(fabric.OKFLatest.Errors, &okfErrList); err != nil {
				return findings, fmt.Errorf("decode OKF sync errors: %w", err)
			}
		}
		findings.okfOK = len(okfErrList) == 0
		if findings.okfOK {
			findings.okfDetail = fmt.Sprintf("OKF repository synced at commit %s", fabric.OKFLatest.CommitHash)
		} else {
			findings.okfDetail = fmt.Sprintf("OKF repository sync at commit %s reported %d error(s)", fabric.OKFLatest.CommitHash, len(okfErrList))
		}
	}

	// Step 4: checkpoints available to resume from.
	if fabric.CheckpointCount == 0 || fabric.LatestCheckpoint == nil {
		findings.checkpointDetail = "no checkpoints recorded"
	} else {
		findings.checkpointDetail = fmt.Sprintf("%d checkpoint(s) loaded, latest at %s", fabric.CheckpointCount, fabric.LatestCheckpoint.UTC().Format(time.RFC3339))
	}

	// Step 2: storage the fabric writes to must accept writes.
	findings.storageOK, findings.storageDetail = verifyStorage(ctx, db, requiredStoragePaths())

	// Steps 8 and 9 act on this service's own recovery state.
	tx, err := db.Begin(ctx)
	if err != nil {
		return findings, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('scos.service_id', $1, true)`, serviceID); err != nil {
		return findings, err
	}
	tag, err := tx.Exec(ctx, `UPDATE recovery_leases SET state='released', released_at=now()
		WHERE state='active' AND expires_at < now()`)
	if err != nil {
		return findings, err
	}
	findings.leasesReset = int(tag.RowsAffected())
	tag, err = tx.Exec(ctx, `UPDATE recovery_runs
		SET phase='superseded', status='superseded', recovery_ended=now(),
		    error_detail=COALESCE(error_detail, 'superseded by a newer recovery run')
		WHERE service_id=$1 AND phase NOT IN ('completed', 'failed', 'superseded')`, serviceID)
	if err != nil {
		return findings, err
	}
	findings.runsSuperseded = int(tag.RowsAffected())
	if err := tx.Commit(ctx); err != nil {
		return findings, err
	}
	return findings, nil
}

// requiredStoragePaths lists the filesystem locations recovery must find
// writable, from SCOS_RECOVERY_REQUIRED_PATHS (comma-separated).
func requiredStoragePaths() []string {
	var paths []string
	for _, p := range strings.Split(os.Getenv("SCOS_RECOVERY_REQUIRED_PATHS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// databaseWritability reports whether the connected database accepts writes.
type databaseWritability interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// verifyStorage checks that the database is a writable primary and that
// every required path is a directory that accepts a durable write.
func verifyStorage(ctx context.Context, db databaseWritability, paths []string) (bool, string) {
	var readOnly string
	var inRecovery bool
	var size int64
	if err := db.QueryRow(ctx, `SELECT current_setting('transaction_read_only'), pg_is_in_recovery(), pg_database_size(current_database())`).Scan(&readOnly, &inRecovery, &size); err != nil {
		return false, "database storage check failed: " + err.Error()
	}
	if readOnly == "on" || inRecovery {
		return false, "database is read-only or a standby; recovery requires a writable primary"
	}
	details := []string{fmt.Sprintf("database writable (%s)", formatBytes(size))}
	for _, path := range paths {
		free, err := probeWritablePath(path)
		if err != nil {
			return false, fmt.Sprintf("required path %s is not writable: %v", path, err)
		}
		details = append(details, fmt.Sprintf("%s writable (%s free)", path, formatBytes(free)))
	}
	return true, strings.Join(details, "; ")
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// persistRecoveryStatus stores the recovery run's relational state and an
// append-only event. The in-memory map is only a response cache.
func (s *recoveryServer) persistRecoveryStatus(ctx context.Context, status *RecoveryStatus, eventType string) error {
	if s.db == nil {
		return nil
	}
	checklist, err := json.Marshal(status.Checklist)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.service_id', $1, true)`, status.ServiceID); err != nil {
		return err
	}
	if status.RecoveryRunID == "" {
		err = tx.QueryRow(ctx, `INSERT INTO recovery_runs
			(service_id, phase, status, checklist, interrupted_turn_count, open_operation_count, pending_outbox_count, recovery_ended, error_detail)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::timestamptz,NULLIF($9,'')) RETURNING id::text`,
			status.ServiceID, status.Phase, status.Phase, checklist, len(status.InterruptedTurns), len(status.OpenOperations), status.PendingOutbox, status.RecoveryEnded, checklistError(status)).Scan(&status.RecoveryRunID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO recovery_leases (lease_key, recovery_run_id, owner, expires_at)
			VALUES ($1,$2,$3,now()+interval '5 minutes')
			ON CONFLICT (lease_key) DO UPDATE SET recovery_run_id=EXCLUDED.recovery_run_id,
				owner=EXCLUDED.owner, state='active', acquired_at=now(), expires_at=EXCLUDED.expires_at, released_at=NULL`,
			"recovery:"+status.ServiceID, status.RecoveryRunID, status.ServiceID)
		if err != nil {
			return err
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE recovery_runs SET phase=$2, status=$3, checklist=$4,
			interrupted_turn_count=$5, open_operation_count=$6, pending_outbox_count=$7,
			recovery_ended=NULLIF($8,'')::timestamptz, error_detail=NULLIF($9,'') WHERE id=$1`,
			status.RecoveryRunID, status.Phase, status.Phase, checklist, len(status.InterruptedTurns), len(status.OpenOperations), status.PendingOutbox, status.RecoveryEnded, checklistError(status))
		if err != nil {
			return err
		}
	}
	payload, err := json.Marshal(map[string]any{
		"phase": status.Phase, "checklist": status.Checklist,
		"interrupted_turns": status.InterruptedTurns, "open_operations": status.OpenOperations,
		"pending_outbox_jobs": status.PendingOutbox,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO recovery_events (recovery_run_id, sequence, event_type, payload)
		VALUES ($1, COALESCE((SELECT max(sequence)+1 FROM recovery_events WHERE recovery_run_id=$1), 1), $2, $3)`, status.RecoveryRunID, eventType, payload)
	if err != nil {
		return err
	}
	if eventType == "recovery_completed" {
		_, err = tx.Exec(ctx, `UPDATE recovery_events SET replay_state='replayed', replayed_at=now()
			WHERE recovery_run_id=$1 AND replay_state='pending'`, status.RecoveryRunID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE recovery_runs SET replay_cursor=COALESCE((SELECT max(sequence) FROM recovery_events WHERE recovery_run_id=$1),0) WHERE id=$1`, status.RecoveryRunID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE recovery_leases SET state='released', released_at=now(), expires_at=now()
			WHERE lease_key=$1 AND recovery_run_id=$2`, "recovery:"+status.ServiceID, status.RecoveryRunID)
	}
	return tx.Commit(ctx)
}

func checklistError(status *RecoveryStatus) string {
	for _, item := range status.Checklist {
		if item.Status == "failed" {
			return item.Detail
		}
	}
	return ""
}

func (s *recoveryServer) handleRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var statuses []*RecoveryStatus
	for _, st := range s.statuses {
		statuses = append(statuses, st)
	}
	s.mu.RUnlock()
	writeJSON(w, statuses)
}

func (s *recoveryServer) handleRecoveryStatusByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/recovery/status/")
	s.mu.RLock()
	status, ok := s.statuses[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, status)
}

func (s *recoveryServer) handleInterruptedTurns(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var allTurns []string
	for _, st := range s.statuses {
		allTurns = append(allTurns, st.InterruptedTurns...)
	}
	s.mu.RUnlock()
	writeJSON(w, allTurns)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
