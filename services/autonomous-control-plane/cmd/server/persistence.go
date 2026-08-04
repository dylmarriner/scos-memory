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

func (s *localStateStore) beginRecovery(tenantID, actorID, interactionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stateKey(tenantID, actorID, interactionID)
	state, ok := s.data.Lifecycles[key]
	if !ok {
		return "", errLocalStateNotFound
	}
	if state.State != "running" && state.State != "interrupted" {
		return state.State, nil
	}
	state.State = "recovering"
	state.RecoveryStatus = "started"
	state.UpdatedAt = time.Now().UTC()
	s.data.Lifecycles[key] = state
	if err := s.writeLocked(); err != nil {
		return "", err
	}
	return "recovering", nil
}

var errLocalStateNotFound = errors.New("local control state not found")
