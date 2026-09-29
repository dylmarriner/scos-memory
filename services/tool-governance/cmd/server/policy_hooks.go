package main

import (
	"context"
	"log"

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
)

// A tool definition and its lifecycle state are this service's governance
// policy: they decide whether operations against the tool may be prepared,
// authorised and executed. Every committed change to one raises
// on_policy_change (GOVERNANCE.md §22.6).
const (
	policyCreated = "created"
	policyUpdated = "updated"
)

// policyChangeHooks reports the synchronous on_policy_change dispatch in the
// API response. Error is set when delivery failed; the change itself is
// already committed and stays in force.
type policyChangeHooks struct {
	Event   string                   `json:"event"`
	Results []hooksdk.DispatchResult `json:"results"`
	Error   string                   `json:"error,omitempty"`
}

// governedToolResponse keeps the tool's own fields at the top level (the
// existing response shape) and adds the hook outcome alongside them.
type governedToolResponse struct {
	*ToolDefinition
	LifecycleHooks *policyChangeHooks `json:"lifecycle_hooks,omitempty"`
}

// dispatchPolicyChange runs after the change is committed: policy changes
// govern safety, so extensions are told synchronously and their outcomes
// returned to the caller, but a dispatch failure never undoes an authorised
// change. It returns nil when no extension manager is configured.
func (s *toolGovernanceServer) dispatchPolicyChange(ctx context.Context, identity requestIdentity, tool *ToolDefinition, changeType string, previousState ToolState) *policyChangeHooks {
	if !s.hooks.Enabled() {
		return nil
	}
	payload := map[string]any{
		"policy_id":   tool.ID,
		"policy_name": tool.Name,
		"version":     tool.Version,
		"change_type": changeType,
		"state":       string(tool.State),
		"actor":       identity.PrincipalID,
	}
	if previousState != "" {
		payload["previous_state"] = string(previousState)
	}
	results, err := s.hooks.Dispatch(ctx, hooksdk.OnPolicyChange, identity.TenantID, identity.PrincipalID, payload)
	out := &policyChangeHooks{Event: hooksdk.OnPolicyChange, Results: results}
	if err != nil {
		log.Printf("tool-governance: on_policy_change for tool %s (%s) not delivered: %v", tool.ID, changeType, err)
		out.Error = err.Error()
	}
	if out.Results == nil {
		out.Results = []hooksdk.DispatchResult{}
	}
	return out
}
