package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
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
	Detail         string          `json:"detail,omitempty"`
	// Request is the route request frozen at ingress. A confirmation and a
	// recovery both execute exactly this request, never a caller's re-send.
	Request *RouteRequest `json:"request,omitempty"`
	// Confirmation is the server-side record of the requesting principal's
	// answer to a decision that requires confirmation.
	Confirmation *confirmationRecord `json:"confirmation,omitempty"`
	// ExecutionAttempt fences progress writes to the executor that claimed
	// the run; a recovery clears it so a stale executor cannot overwrite the
	// recovered run.
	ExecutionAttempt string    `json:"execution_attempt,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
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

func (s *localStateStore) saveRoute(tenantID, actorID string, req RouteRequest, decision RouteDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, decision.InteractionID)
	s.data.Decisions[key] = decision
	frozen := req
	state := lifecycleState{
		State: runPlanned, IngressStatus: "committed", Stages: pendingStages(decision),
		Reply: replyState{Status: "pending"}, Checkpoint: checkpointState{Status: "pending"},
		Outbox: outboxState{Status: "pending"}, Request: &frozen, UpdatedAt: time.Now().UTC(),
	}
	if decision.ConfirmationRequired {
		state.State = runAwaitingConfirmation
		state.Detail = awaitingConfirmationDetail
		state.Confirmation = &confirmationRecord{DecisionID: decision.DecisionID, Status: confirmationAwaiting, RequestedBy: actorID}
	}
	s.data.Lifecycles[key] = state
	return s.writeLocked()
}

// claimExecution moves a run that may start executing to running and
// records the attempt that owns it.
func (s *localStateStore) claimExecution(tenantID, actorID, interactionID, attempt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return errLocalStateNotFound
	}
	if !claimableRunStatus(state.State) {
		return errExecutionNotClaimable
	}
	state.State, state.ExecutionAttempt, state.Detail = runRunning, attempt, ""
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
	return s.writeLocked()
}

// recordProgress persists the lifecycle written by the executor holding
// attempt. A write from any other attempt is refused.
func (s *localStateStore) recordProgress(tenantID, actorID, interactionID, attempt string, lifecycle lifecycleResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return errLocalStateNotFound
	}
	if state.ExecutionAttempt != attempt {
		return errExecutionSuperseded
	}
	state.State, state.Detail = lifecycle.Status, lifecycle.Detail
	state.Stages = append([]stageState(nil), lifecycle.Stages...)
	state.Reply, state.Checkpoint, state.Outbox = lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
	return s.writeLocked()
}

// resolveConfirmation records the requesting principal's answer to the
// decision awaiting confirmation and returns the frozen request it applies to.
func (s *localStateStore) resolveConfirmation(tenantID, actorID, interactionID, decisionID, resolution string) (RouteRequest, RouteDecision, *confirmationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	decision, found := s.data.Decisions[key]
	if !ok || !found {
		return RouteRequest{}, RouteDecision{}, nil, errLocalStateNotFound
	}
	if decision.DecisionID != decisionID {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationDecisionMismatch
	}
	if state.Confirmation == nil || state.Confirmation.DecisionID != decisionID {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationNotAwaiting
	}
	if state.Confirmation.RequestedBy != actorID {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationRequester
	}
	if state.Confirmation.Status != confirmationAwaiting || state.State != runAwaitingConfirmation {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationNotAwaiting
	}
	if state.Request == nil {
		return RouteRequest{}, RouteDecision{}, nil, errRouteRequestMissing
	}
	resolvedAt := time.Now().UTC()
	record := &confirmationRecord{DecisionID: decisionID, Status: resolution, RequestedBy: actorID, ResolvedBy: actorID, ResolvedAt: &resolvedAt}
	state.Confirmation = record
	if resolution == confirmationRejected {
		state.State, state.Detail = runRejected, rejectedDetail
		for i := range state.Stages {
			if state.Stages[i].Status == stagePending {
				state.Stages[i].Status, state.Stages[i].Detail = stageSkipped, rejectedDetail
			}
		}
	} else {
		state.State, state.Detail = runConfirmed, ""
	}
	state.UpdatedAt = resolvedAt
	s.data.Lifecycles[key] = state
	if err := s.writeLocked(); err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	copied := *record
	return *state.Request, decision, &copied, nil
}

// recoveryInput returns what a recovery needs to resume a run.
func (s *localStateStore) recoveryInput(tenantID, actorID, interactionID string) (recoveryInput, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	decision, found := s.data.Decisions[key]
	if !ok || !found {
		return recoveryInput{}, errLocalStateNotFound
	}
	input := recoveryInput{Decision: decision, Stages: append([]stageState(nil), state.Stages...)}
	if state.Request != nil {
		frozen := *state.Request
		input.Request = &frozen
	}
	if state.Confirmation != nil {
		record := *state.Confirmation
		input.Confirmation = &record
	}
	return input, nil
}

// markReconciliationRequired ends a recovery that cannot safely resume.
func (s *localStateStore) markReconciliationRequired(tenantID, actorID, interactionID, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return errLocalStateNotFound
	}
	if state.State != runRecovering {
		return errExecutionNotClaimable
	}
	state.State, state.Detail, state.RecoveryStatus = runReconciliationRequired, detail, "reconciliation_required"
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
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

// recentDecisions lists stored decisions newest first, scoped to the actor
// unless tenantWide.
func (s *localStateStore) recentDecisions(tenantID, actorID string, tenantWide bool, limit int) []RouteDecision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RouteDecision, 0)
	for key, decision := range s.data.Decisions {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 || parts[0] != tenantID || (!tenantWide && parts[1] != actorID) {
			continue
		}
		out = append(out, decision)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
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

func (s *localStateStore) beginRecovery(tenantID, actorID, interactionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return "", errLocalStateNotFound
	}
	if state.State != runRunning && state.State != runInterrupted {
		return state.State, nil
	}
	state.State = runRecovering
	state.RecoveryStatus = "started"
	state.ExecutionAttempt = ""
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
	if err := s.writeLocked(); err != nil {
		return "", err
	}
	return runRecovering, nil
}

var errLocalStateNotFound = errors.New("local control state not found")
