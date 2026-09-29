package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// requestCorrelation extracts the §32 correlation IDs a request carries. A
// verified token's claims win; otherwise the gateway-set tenant header is
// used. The result labels telemetry only and is never used for authorisation.
func requestCorrelation(r *http.Request) telemetry.Correlation {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		return telemetry.Correlation{Tenant: claims.TenantID, AgentID: claims.AgentID, TraceID: claims.TraceID}
	}
	return telemetry.Correlation{Tenant: strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))}
}

// emitSignatureState reports the outcome of a manifest signature
// verification: 1 when the signature verified, 0 otherwise, with the precise
// state as a label.
func emitSignatureState(m ExtensionManifest, state string) {
	verified := 0.0
	if state == signatureValid {
		verified = 1
	}
	serviceMetrics.Gauge(telemetry.MetricSignatureState, verified, "state", map[string]string{
		"extension": m.PackageID, "version": m.Version, "state": state,
	}, telemetry.Correlation{})
}

// emitPermissions reports the permissions an extension holds after a
// lifecycle change: the declared count while active, 0 once it no longer runs.
func emitPermissions(m ExtensionManifest, action string, active bool, c telemetry.Correlation) {
	granted := 0
	if active {
		granted = len(m.Permissions)
	}
	permissions := append([]string(nil), m.Permissions...)
	sort.Strings(permissions)
	serviceMetrics.Gauge(telemetry.MetricExtensionPermissions, float64(granted), "count", map[string]string{
		"extension": m.PackageID, "version": m.Version, "action": action,
		"permissions": strings.Join(permissions, ","),
	}, c)
}

// setHealthLocked records a plugin's health as derived from its latest hook
// outcome and emits §32 plugin health when the state changes. Caller holds s.mu.
func (s *extServer) setHealthLocked(packageID, state string, consecutiveFailures int, c telemetry.Correlation) {
	ext, ok := s.extensions[packageID]
	if !ok || ext.Health == state {
		return
	}
	ext.Health = state
	healthy := 0.0
	if state == "healthy" {
		healthy = 1
	}
	serviceMetrics.Gauge(telemetry.MetricPluginHealth, healthy, "state", map[string]string{
		"extension": packageID, "state": state, "consecutive_failures": strconv.Itoa(consecutiveFailures),
	}, c)
}

// emitHookResourceConsumption reports what one hook invocation measurably
// consumed: wall-clock time spent waiting on the extension and the payload
// bytes sent to it.
func emitHookResourceConsumption(extensionID, event string, elapsed time.Duration, payloadBytes int, c telemetry.Correlation) {
	serviceMetrics.Observe(telemetry.MetricResourceConsumption, elapsed, map[string]string{
		"extension": extensionID, "event": event, "resource": "hook_call_time",
	}, c)
	serviceMetrics.Record(telemetry.MetricResourceConsumption, float64(payloadBytes), "bytes", map[string]string{
		"extension": extensionID, "event": event, "resource": "hook_payload_bytes",
	}, c)
}

// emitSkillSelection counts a skill resolution by id, labelled with the
// outcome so misses are visible alongside selections.
func emitSkillSelection(id string, skill *Skill, c telemetry.Correlation) {
	labels := map[string]string{"skill": id, "outcome": "not_found"}
	if skill != nil {
		labels["outcome"] = "selected"
		labels["name"] = skill.Name
		labels["version"] = skill.Version
		labels["status"] = skill.Status
		if packageID, _, found := strings.Cut(id, "/"); found {
			labels["extension"] = packageID
		}
	}
	serviceMetrics.Count(telemetry.MetricSkillSelection, labels, c)
}
