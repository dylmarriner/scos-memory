package hooksdk

import (
	"context"
	"fmt"
	"sync"
)

// HookStage defines point of execution in lifecycle (§22.6).
type HookStage string

const (
	StagePreTurn         HookStage = "pre_turn"
	StagePostTurn        HookStage = "post_turn"
	StageProxyExecute    HookStage = "proxy_execute"
	StageRetrievalFilter HookStage = "retrieval_filter"
)

// HookContext provides context passed to hook handlers.
type HookContext struct {
	SessionID  string            `json:"session_id"`
	TenantID   string            `json:"tenant_id"`
	Stage      HookStage         `json:"stage"`
	Payload    string            `json:"payload"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// HookResult defines return outcome from hook invocation.
type HookResult struct {
	ModifiedPayload string `json:"modified_payload,omitempty"`
	AbortExecution  bool   `json:"abort_execution"`
	Reason          string `json:"reason,omitempty"`
}

// Handler interface implemented by hooks.
type Handler interface {
	Execute(ctx context.Context, hctx *HookContext) (*HookResult, error)
}

// Registry manages hook registrations for stages.
type Registry struct {
	mu    sync.RWMutex
	hooks map[HookStage][]Handler
}

// NewRegistry initializes a hook registry.
func NewRegistry() *Registry {
	return &Registry{
		hooks: make(map[HookStage][]Handler),
	}
}

// Register adds a handler for a lifecycle stage.
func (r *Registry) Register(stage HookStage, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[stage] = append(r.hooks[stage], handler)
}

// Dispatch executes registered hooks for a stage in sequence (§22.7).
func (r *Registry) Dispatch(ctx context.Context, hctx *HookContext) (*HookResult, error) {
	r.mu.RLock()
	handlers := append([]Handler(nil), r.hooks[hctx.Stage]...)
	r.mu.RUnlock()

	currentPayload := hctx.Payload
	for i, h := range handlers {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		res, err := h.Execute(ctx, hctx)
		if err != nil {
			return nil, fmt.Errorf("hook %d failed: %w", i, err)
		}
		if res == nil {
			continue
		}
		if res.AbortExecution {
			return res, nil
		}
		if res.ModifiedPayload != "" {
			currentPayload = res.ModifiedPayload
			hctx.Payload = currentPayload
		}
	}

	return &HookResult{ModifiedPayload: currentPayload}, nil
}
