package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type lifecycleState struct {
	State          string          `json:"state"`
	IngressStatus  string          `json:"ingress_status"`
	Stages         []stageState    `json:"stages,omitempty"`
	Reply          replyState      `json:"reply"`
	Checkpoint     checkpointState `json:"checkpoint"`
	Outbox         outboxState     `json:"outbox"`
	ReplayStatus   string          `json:"replay_status,omitempty"`
	RecoveryStatus string          `json:"recovery_status,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`

	// Request and Outputs are what a recovery needs to resume the run: the
	// original request and the results of stages that already completed.
	Request *RouteRequest `json:"request,omitempty"`
	Outputs stageOutputs  `json:"outputs"`

	// FailureDetail records why a run degraded when no stage carries the
	// cause (a failed turn-boundary hook), so recovery can classify it.
	FailureDetail string `json:"failure_detail,omitempty"`

	// Recovery bookkeeping. In PostgreSQL the lease lives in the
	// recovery_lease_* columns and the rest in the checkpoint JSON.
	LeaseOwner            string     `json:"lease_owner,omitempty"`
	LeaseExpiresAt        *time.Time `json:"lease_expires_at,omitempty"`
	RecoveryAttempts      int        `json:"recovery_attempts,omitempty"`
	RecoveryPermanent     bool       `json:"recovery_permanent,omitempty"`
	RecoveryFailureReason string     `json:"recovery_failure_reason,omitempty"`
}

// stageOutputs holds the results later stages consume, so a resumed run does
// not have to repeat completed stages.
type stageOutputs struct {
	Memory   string      `json:"memory,omitempty"`
	Code     string      `json:"code,omitempty"`
	Compiled string      `json:"compiled_context,omitempty"`
	Agent    *agentReply `json:"agent_reply,omitempty"`
}

// staleRunAfter is how long a never-claimed "running" run must go without
// recording progress before it is presumed crashed rather than live. Runs
// claimed for recovery are protected by their lease instead.
const staleRunAfter = 5 * time.Minute

// recoveryLeaseTTL is how long a recovery claim lives without a progress
// write renewing it. Every stage boundary renews it, so it only has to
// outlast the slowest single stage.
const recoveryLeaseTTL = 2 * time.Minute

// maxRecoveryAttempts bounds how often one run is recovered before it is
// marked permanently failed.
const maxRecoveryAttempts = 3

// errLeaseLost reports that another recoverer now holds the run's lease, so
// this execution must stop writing progress.
var errLeaseLost = errors.New("recovery lease lost: another recoverer holds this run")

func leaseLive(s lifecycleState, now time.Time) bool {
	return s.LeaseOwner != "" && s.LeaseExpiresAt != nil && now.Before(*s.LeaseExpiresAt)
}

// claimable reports whether a run may be claimed for recovery. The SQL claim
// in beginRecoveryDB applies the same rule.
func claimable(s lifecycleState, now time.Time) bool {
	if s.RecoveryPermanent || leaseLive(s, now) {
		return false
	}
	switch s.State {
	case "interrupted", "degraded", "failed", "recovering":
		return true
	case "running", "planned":
		return s.UpdatedAt.IsZero() || now.Sub(s.UpdatedAt) > staleRunAfter
	}
	return false
}

// fenceAllows reports whether a writer holding owner ("" for a first
// execution) may write a run's progress.
func fenceAllows(s lifecycleState, owner string, now time.Time) bool {
	return s.LeaseOwner == "" || s.LeaseOwner == owner || !leaseLive(s, now)
}

type stageState struct {
	Order     int       `json:"order"`
	System    string    `json:"system"`
	Action    string    `json:"action"`
	Required  bool      `json:"required"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

type replyState struct {
	Status  string `json:"status"`
	Content string `json:"content,omitempty"`
}

type checkpointState struct {
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
}

type outboxState struct {
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
}

type controlState struct {
	Decisions  map[string]RouteDecision  `json:"decisions"`
	Lifecycles map[string]lifecycleState `json:"lifecycles"`
}

type localStateStore struct {
	mu   sync.RWMutex
	path string
	data controlState
}

func newLocalStateStore(path string) (*localStateStore, error) {
	s := &localStateStore{
		path: path,
		data: controlState{
			Decisions:  make(map[string]RouteDecision),
			Lifecycles: make(map[string]lifecycleState),
		},
	}
	if strings.TrimSpace(path) == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	if s.data.Decisions == nil {
		s.data.Decisions = make(map[string]RouteDecision)
	}
	if s.data.Lifecycles == nil {
		s.data.Lifecycles = make(map[string]lifecycleState)
	}
	return s, nil
}

func stateKey(tenantID, actorID, interactionID string) string {
	return tenantID + "\x00" + actorID + "\x00" + interactionID
}

func (s *localStateStore) writeLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".control-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s *localStateStore) saveRoute(tenantID, actorID string, decision RouteDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, decision.InteractionID)
	s.data.Decisions[key] = decision
	stages := make([]stageState, len(decision.SelectedPlan))
	for i, step := range decision.SelectedPlan {
		stages[i] = stageState{Order: step.Order, System: step.System, Action: step.Action, Required: step.Required, Status: "pending"}
	}
	s.data.Lifecycles[key] = lifecycleState{
		State: "planned", IngressStatus: "committed", Stages: stages,
		Reply: replyState{Status: "pending"}, Checkpoint: checkpointState{Status: "pending"},
		Outbox: outboxState{Status: "pending"}, UpdatedAt: time.Now().UTC(),
	}
	return s.writeLocked()
}

func (s *localStateStore) updateLifecycle(tenantID, actorID, interactionID string, update func(*lifecycleState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return errLocalStateNotFound
	}
	update(&state)
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
	return s.writeLocked()
}

func (s *localStateStore) decision(tenantID, actorID, interactionID string) (RouteDecision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	decision, ok := s.data.Decisions[stateKey(tenantID, actorID, interactionID)]
	if !ok {
		return RouteDecision{}, errLocalStateNotFound
	}
	return decision, nil
}

func (s *localStateStore) markReplay(tenantID, actorID, interactionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return errLocalStateNotFound
	}
	state.ReplayStatus = "inspected"
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
	return s.writeLocked()
}

// claim applies claimable and, when it holds, takes the lease.
func (s *localStateStore) claim(tenantID, actorID, interactionID, owner string) (recoveryClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return recoveryClaim{}, errLocalStateNotFound
	}
	now := time.Now().UTC()
	if !claimable(state, now) {
		return recoveryClaim{State: state.State}, nil
	}
	expires := now.Add(recoveryLeaseTTL)
	state.State, state.RecoveryStatus = "recovering", "started"
	state.LeaseOwner, state.LeaseExpiresAt = owner, &expires
	state.RecoveryAttempts++
	state.UpdatedAt = now
	s.data.Lifecycles[key] = state
	if err := s.writeLocked(); err != nil {
		return recoveryClaim{}, err
	}
	return recoveryClaim{State: "recovering", Claimed: true, Owner: owner, Attempts: state.RecoveryAttempts}, nil
}

// fencedUpdate applies update only while owner may write the run, renewing
// the lease for a recovery owner or releasing it when release is set.
func (s *localStateStore) fencedUpdate(tenantID, actorID, interactionID, owner string, release bool, update func(*lifecycleState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return errLocalStateNotFound
	}
	now := time.Now().UTC()
	if !fenceAllows(state, owner, now) {
		return errLeaseLost
	}
	update(&state)
	switch {
	case release:
		state.LeaseOwner, state.LeaseExpiresAt = "", nil
	case owner != "":
		expires := now.Add(recoveryLeaseTTL)
		state.LeaseOwner, state.LeaseExpiresAt = owner, &expires
	}
	state.UpdatedAt = now
	s.data.Lifecycles[key] = state
	return s.writeLocked()
}

func (s *localStateStore) runState(tenantID, actorID, interactionID string) (lifecycleState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.data.Lifecycles[stateKey(tenantID, actorID, interactionID)]
	if !ok {
		return lifecycleState{}, errLocalStateNotFound
	}
	state.Stages = append([]stageState(nil), state.Stages...)
	return state, nil
}

var errLocalStateNotFound = errors.New("local control state not found")
