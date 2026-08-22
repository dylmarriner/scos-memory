package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

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
	mu       sync.RWMutex
	statuses map[string]*RecoveryStatus
}

func newRecoveryServer() *recoveryServer {
	return &recoveryServer{
		statuses: make(map[string]*RecoveryStatus),
	}
}

func newMux(s *recoveryServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	mux.Handle("/v1/recovery/start", serviceauth.RequireScope("recovery:admin", http.HandlerFunc(s.handleRecoveryStart)))
	mux.Handle("/v1/recovery/checklist", serviceauth.RequireScope("recovery:admin", http.HandlerFunc(s.handleChecklistUpdate)))
	mux.Handle("/v1/recovery/complete", serviceauth.RequireScope("recovery:admin", http.HandlerFunc(s.handleRecoveryComplete)))
	mux.HandleFunc("/v1/recovery/status", s.handleRecoveryStatus)
	mux.HandleFunc("/v1/recovery/status/", s.handleRecoveryStatusByID)
	mux.HandleFunc("/v1/recovery/interrupted-turns", s.handleInterruptedTurns)
	return mux
}

func main() {
	s := newRecoveryServer()
	mux := newMux(s)

	addr := os.Getenv("SCOS_RECOVERY_ORCHESTRATOR_HTTP_ADDR")
	if addr == "" {
		addr = ":8094"
	}
	log.Printf("recovery orchestrator listening on %s", addr)
	if err := http.ListenAndServe(addr, serviceauth.Require(mux)); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *recoveryServer) handleRecoveryStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ServiceID string `json:"service_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	s.mu.Lock()
	s.statuses[req.ServiceID] = status
	s.mu.Unlock()

	writeJSON(w, status)
}

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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	for i := range status.Checklist {
		if status.Checklist[i].Step == req.Step {
			status.Checklist[i].Status = req.Status
			status.Checklist[i].Detail = req.Detail
			if req.Status == "ok" || req.Status == "failed" {
				status.Checklist[i].Completed = time.Now().UTC().Format(time.RFC3339)
			}
			break
		}
	}
	s.mu.Unlock()
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	status.Phase = "completed"
	status.RecoveryEnded = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()
	writeJSON(w, status)
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
