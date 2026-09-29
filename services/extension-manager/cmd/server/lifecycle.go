package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// Hook execution properties every declared hook must state (GOVERNANCE.md §22.6).
const (
	hookTypeSynchronous  = "synchronous"
	hookTypeAsynchronous = "asynchronous"
	hookTypeBlocking     = "blocking"
	hookTypeMutating     = "mutating"

	// Fallback policies accepted by schemas/extensions/extension-manifest.schema.json.
	fallbackContinue   = "continue"
	fallbackFailClosed = "fail_closed"

	// maxHookTimeoutMs matches the ceiling dispatch enforces, so a declaration
	// can never promise a longer wait than the runtime will grant.
	maxHookTimeoutMs = 30000
)

var hookTypes = []string{hookTypeSynchronous, hookTypeAsynchronous, hookTypeBlocking, hookTypeMutating}

// acceptedHookEvents is the §22.6 lifecycle set plus the turn stages the
// autonomous control plane dispatches (services/autonomous-control-plane
// execute.go: pre_turn, post_turn).
var acceptedHookEvents = func() map[string]bool {
	events := map[string]bool{
		string(hooksdk.StagePreTurn):  true,
		string(hooksdk.StagePostTurn): true,
	}
	for _, event := range hooksdk.RequiredLifecycleEvents {
		events[event] = true
	}
	return events
}()

// HookDeclaration is a manifest's statement of how one subscribed hook
// executes. Retryable and Idempotent are pointers so an omitted value is
// rejected rather than silently read as false.
type HookDeclaration struct {
	Type           string `json:"type"`
	Retryable      *bool  `json:"retryable"`
	Idempotent     *bool  `json:"idempotent"`
	TimeoutMs      int    `json:"timeout_ms"`
	FallbackPolicy string `json:"fallback_policy"`
}

// validateHookDeclarations rejects unknown events and hooks whose execution
// properties are missing or invalid. Its errors are client errors (400).
func validateHookDeclarations(m ExtensionManifest) error {
	subscribed := make(map[string]bool, len(m.HooksSubscribed))
	for _, event := range m.HooksSubscribed {
		if !acceptedHookEvents[event] {
			return fmt.Errorf("hook event %q is not a supported lifecycle event", event)
		}
		if subscribed[event] {
			return fmt.Errorf("hook event %q is subscribed more than once", event)
		}
		subscribed[event] = true
		d, ok := m.HookDeclarations[event]
		if !ok {
			return fmt.Errorf("hook %s must declare type, retryable, idempotent, timeout_ms and fallback_policy", event)
		}
		if !contains(hookTypes, d.Type) {
			return fmt.Errorf("hook %s type %q must be one of %s", event, d.Type, strings.Join(hookTypes, ", "))
		}
		if d.Retryable == nil || d.Idempotent == nil {
			return fmt.Errorf("hook %s must declare retryable and idempotent", event)
		}
		if d.TimeoutMs <= 0 || d.TimeoutMs > maxHookTimeoutMs {
			return fmt.Errorf("hook %s timeout_ms must be between 1 and %d", event, maxHookTimeoutMs)
		}
		if d.FallbackPolicy != fallbackContinue && d.FallbackPolicy != fallbackFailClosed {
			return fmt.Errorf("hook %s fallback_policy %q must be %s or %s", event, d.FallbackPolicy, fallbackContinue, fallbackFailClosed)
		}
		// A detached hook cannot gate its caller, so fail_closed would be a
		// promise dispatch cannot keep.
		if d.Type == hookTypeAsynchronous && d.FallbackPolicy == fallbackFailClosed {
			return fmt.Errorf("hook %s cannot be asynchronous and fail_closed", event)
		}
	}
	for event := range m.HookDeclarations {
		if !subscribed[event] {
			return fmt.Errorf("hook declaration for %q has no matching hooks_subscribed entry", event)
		}
	}
	return nil
}

// sortHooks gives every event a dispatch order that depends only on the
// registered set: extension id, then declaration order (stable sort). It is
// applied on registration and on load, so order survives restarts (§22.6).
func sortHooks(hooks []*Hook) {
	sort.SliceStable(hooks, func(i, j int) bool { return hooks[i].ExtensionID < hooks[j].ExtensionID })
}

// undeliverableLocked reports why an extension's hooks must not be invoked,
// or "" when they may: only enabled extensions receive hooks, except the one
// named by deliverTo. Caller holds s.mu.
func (s *extServer) undeliverableLocked(extensionID, deliverTo string) string {
	if extensionID == deliverTo {
		return ""
	}
	ext, ok := s.extensions[extensionID]
	if !ok {
		return "extension not installed"
	}
	if ext.Status != "enabled" {
		return "extension " + ext.Status
	}
	return ""
}

// packageSnapshot captures everything one package owns so an enabling change
// can be undone when a critical lifecycle hook rejects it.
type packageSnapshot struct {
	id      string
	ext     *Extension // copy; nil when the package was not installed
	history []ExtensionManifest
	hooks   map[string][]*Hook
	skills  map[string]*Skill
}

func (s *extServer) snapshotPackageLocked(id string) packageSnapshot {
	snap := packageSnapshot{
		id:      id,
		history: append([]ExtensionManifest(nil), s.history[id]...),
		hooks:   map[string][]*Hook{},
		skills:  map[string]*Skill{},
	}
	if ext, ok := s.extensions[id]; ok {
		copied := *ext
		snap.ext = &copied
	}
	for event, hooks := range s.hooks {
		for _, hook := range hooks {
			if hook.ExtensionID == id {
				snap.hooks[event] = append(snap.hooks[event], hook)
			}
		}
	}
	for skillID, skill := range s.skills {
		if strings.HasPrefix(skillID, id+"/") {
			snap.skills[skillID] = skill
		}
	}
	return snap
}

// restorePackageLocked puts a package back exactly as snapshotted, leaving
// every other package untouched. An existing *Extension is overwritten in
// place so pointers held by the caller stay valid.
func (s *extServer) restorePackageLocked(snap packageSnapshot) {
	s.removeExtensionRegistrationsLocked(snap.id)
	for event, hooks := range snap.hooks {
		s.hooks[event] = append(s.hooks[event], hooks...)
		sortHooks(s.hooks[event])
	}
	for skillID, skill := range snap.skills {
		s.skills[skillID] = skill
	}
	switch current, ok := s.extensions[snap.id]; {
	case snap.ext == nil:
		delete(s.extensions, snap.id)
	case ok:
		*current = *snap.ext
	default:
		restored := *snap.ext
		s.extensions[snap.id] = &restored
	}
	if len(snap.history) == 0 {
		delete(s.history, snap.id)
	} else {
		s.history[snap.id] = snap.history
	}
}

// lifecycleReport is one manager-raised lifecycle event's outcome, returned
// to the caller of the route that raised it.
type lifecycleReport struct {
	Event   string               `json:"event"`
	Results []hookDispatchResult `json:"results"`
	Error   string               `json:"error,omitempty"`
}

// raiseLifecycle dispatches a lifecycle event in-process for a change that has
// already been persisted. extra hooks are dispatched alongside the registered
// ones (an uninstalled extension's own on_uninstall hook), and deliverTo names
// the extension whose hooks are delivered despite not being enabled -- the one
// being disabled or uninstalled (empty for none). The identity is the
// request's own, resolved exactly as /v1/hooks/dispatch resolves it; a request
// without one is never given an invented identity -- its hooks are reported
// skipped, and a fail_closed hook turns that into an error. A non-nil error
// means a critical hook failed or could not be dispatched.
func (s *extServer) raiseLifecycle(r *http.Request, event string, ext Extension, extra []*Hook, deliverTo string) (lifecycleReport, error) {
	s.mu.RLock()
	hooks := append(append([]*Hook(nil), s.hooks[event]...), extra...)
	s.mu.RUnlock()
	report := lifecycleReport{Event: event, Results: []hookDispatchResult{}}
	if len(hooks) == 0 {
		return report, nil
	}
	sortHooks(hooks)
	req := hookDispatchRequest{
		Event: event,
		Payload: map[string]any{
			"extension_id": ext.Manifest.PackageID,
			"version":      ext.Manifest.Version,
			"status":       ext.Status,
			"permissions":  append([]string{}, ext.Manifest.Permissions...),
		},
		corr:      requestCorrelation(r),
		deliverTo: deliverTo,
	}
	started := time.Now()
	var results []hookDispatchResult
	var err error
	if identity, status, msg := resolveIdentity(r, "", ""); status != 0 {
		for _, hook := range hooks {
			s.mu.RLock()
			undeliverable := s.undeliverableLocked(hook.ExtensionID, deliverTo)
			s.mu.RUnlock()
			if undeliverable != "" {
				results = append(results, hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "skipped", Reason: undeliverable})
				continue
			}
			if hook.FallbackPolicy == fallbackFailClosed {
				err = fmt.Errorf("critical hook %s cannot be dispatched: %s", hook.ExtensionID, msg)
				break
			}
			results = append(results, hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "skipped", Reason: "request has no tenant/principal identity: " + msg})
		}
	} else {
		req.TenantID, req.PrincipalID = identity.TenantID, identity.PrincipalID
		req.corr.Tenant = identity.TenantID
		// The state change is already durable; a caller disconnecting must not
		// cancel delivery. Each hook remains bounded by its own timeout.
		results, err = s.dispatchHookList(context.WithoutCancel(r.Context()), req, hooks)
	}
	recordHookDispatch(event, started, results, err, req.corr)
	if results != nil {
		report.Results = results
	}
	if err != nil {
		report.Error = err.Error()
	}
	return report, err
}

// recordHookDispatch emits the §32 hook latency and failure measures for one
// dispatch. Latency covers the whole bounded dispatch; failures are counted
// per hook so one failing extension is attributable.
func recordHookDispatch(event string, started time.Time, results []hookDispatchResult, err error, corr telemetry.Correlation) {
	serviceMetrics.Observe(telemetry.MetricHookLatency, time.Since(started), map[string]string{"event": event}, corr)
	if err != nil {
		serviceMetrics.Count(telemetry.MetricHookFailure, map[string]string{"event": event, "reason": "dispatch_error"}, corr)
		return
	}
	for _, result := range results {
		switch result.Status {
		case "ok", "succeeded", "queued":
			continue
		}
		serviceMetrics.Count(telemetry.MetricHookFailure, map[string]string{
			"event": event, "extension": result.ExtensionID, "reason": result.Status,
		}, corr)
	}
}

// rejectLifecycleChange undoes an enabling change a critical hook rejected,
// persists the restored state and answers 424 with the hook outcome.
func (s *extServer) rejectLifecycleChange(w http.ResponseWriter, r *http.Request, snap packageSnapshot, report lifecycleReport, action string, restore func()) {
	s.mu.Lock()
	s.restorePackageLocked(snap)
	if restore != nil {
		restore()
	}
	s.mu.Unlock()
	body := map[string]any{
		"status":          "rolled_back",
		"package_id":      snap.id,
		"error":           "critical " + report.Event + " hook rejected the change: " + report.Error,
		"lifecycle_hooks": []lifecycleReport{report},
	}
	if err := s.persist(r.Context()); err != nil {
		body["persist_error"] = "persist restored extension state: " + err.Error()
	}
	if snap.ext != nil {
		emitPermissions(snap.ext.Manifest, action+"_rolled_back", snap.ext.Status == "enabled", requestCorrelation(r))
	}
	writeJSONStatus(w, http.StatusFailedDependency, body)
}
