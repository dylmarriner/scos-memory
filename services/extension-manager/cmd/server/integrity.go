package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ── manifest integrity and isolation ─────────────────────────────────────
//
// content_hash is "sha256:" followed by the hex SHA-256 of the manifest's
// JSON encoding with content_hash and signature both empty. The signature
// is an Ed25519 signature over the manifest's JSON encoding with only the
// signature empty, so it covers the content hash. Both are verified when an
// extension is installed, enabled, rolled back to, promoted from a canary,
// and when persisted state is loaded.
//
// Isolation: the extension manager never loads or executes extension code.
// The only thing it runs on an extension's behalf is a hook delivery: an
// HTTPS (or loopback HTTP) POST to the endpoint the manifest declares, with
// a timeout. Whatever runs behind that endpoint runs in its own host's
// process, under whatever isolation that host provides. That is the one
// isolation mode this runtime provides, and the only one a manifest may
// request; in_process_trusted, separate_process, container and wasm_sandbox
// would each be a claim about code the manager does not run.

const contentHashPrefix = "sha256:"

const isolationRemoteEndpoint = "remote_endpoint"

// isolationEnforcement says exactly what the remote_endpoint mode enforces.
const isolationEnforcement = "network boundary only: the extension manager never loads or executes extension code; " +
	"hooks are delivered as HTTPS (or loopback HTTP) requests with a timeout to the extension's own endpoint, " +
	"which runs with whatever isolation its host provides"

// governanceIsolationModes are the modes governance ss22.4 names. Only
// isolationRemoteEndpoint is provided by this runtime.
var governanceIsolationModes = []string{"in_process_trusted", "separate_process", "container", "wasm_sandbox", isolationRemoteEndpoint}

// manifestContentHash computes the content hash a manifest must declare.
func manifestContentHash(m ExtensionManifest) (string, error) {
	m.ContentHash = ""
	m.Signature = ""
	payload, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return contentHashPrefix + sha256Hex(payload), nil
}

// verifyContentHash checks the declared content_hash against the manifest.
func verifyContentHash(m ExtensionManifest) error {
	want, err := manifestContentHash(m)
	if err != nil {
		return fmt.Errorf("compute content hash: %w", err)
	}
	if m.ContentHash != want {
		return fmt.Errorf("content_hash does not match the manifest content (declared %q)", m.ContentHash)
	}
	return nil
}

// verifyIsolation refuses an isolation mode this runtime does not provide.
func verifyIsolation(m ExtensionManifest) error {
	if m.Isolation == isolationRemoteEndpoint {
		return nil
	}
	if contains(governanceIsolationModes, m.Isolation) {
		return fmt.Errorf("isolation %q is not provided by this runtime: extension code is never loaded or executed by the extension manager, hooks run at the extension's own endpoint (isolation %q)", m.Isolation, isolationRemoteEndpoint)
	}
	return fmt.Errorf("unknown isolation %q (supported: %s)", m.Isolation, isolationRemoteEndpoint)
}

// verifyStoredManifest checks what can be checked about a manifest already
// in state without the signing key: its content hash and isolation.
func verifyStoredManifest(m ExtensionManifest) error {
	if err := verifyContentHash(m); err != nil {
		return err
	}
	return verifyIsolation(m)
}

// verifyManifestIntegrity is the full check applied before a manifest's
// hooks and skills are made active: signature, content hash and isolation.
func (s *extServer) verifyManifestIntegrity(m ExtensionManifest) error {
	if !s.verifySignature(m) {
		return fmt.Errorf("invalid signature")
	}
	return verifyStoredManifest(m)
}

// hookTimeoutMs is the timeout every hook delivery is given.
const hookTimeoutMs = 30000

// hookType names how the dispatcher treats a hook: every delivery is
// awaited in registration order; a fail_closed hook's failure aborts the
// dispatch ("blocking"), any other hook's failure is recorded and the
// dispatch continues ("synchronous").
func hookType(fallbackPolicy string) string {
	if fallbackPolicy == "fail_closed" {
		return "blocking"
	}
	return "synchronous"
}

func newHook(manifest ExtensionManifest, event string) *Hook {
	fallback := manifest.HookFallbackPolicies[event]
	if fallback == "" {
		fallback = "continue"
	}
	return &Hook{
		Event:          event,
		ExtensionID:    manifest.PackageID,
		Version:        manifest.Version,
		Type:           hookType(fallback),
		Retryable:      false,
		TimeoutMs:      hookTimeoutMs,
		Endpoint:       manifest.HookEndpoints[event],
		FallbackPolicy: fallback,
	}
}

// ownedSkillID is the id under which an extension's exported skill is
// registered.
func ownedSkillID(packageID, skill string) string {
	return packageID + "/" + skill
}

// normalizeLocked brings state written by earlier versions of this service
// in line with what the dispatcher actually does: hooks report their real
// semantics, extension-exported skills record their owner, skills left
// behind by an extension that is no longer installed are removed, and an
// enabled extension whose stored manifest fails its integrity or isolation
// check is disabled with the reason recorded. Callers hold s.mu.
func (s *extServer) normalizeLocked() {
	for event, hooks := range s.hooks {
		kept := hooks[:0]
		for _, h := range hooks {
			ext, ok := s.extensions[h.ExtensionID]
			if !ok {
				continue
			}
			h.Type = hookType(h.FallbackPolicy)
			h.Retryable = false
			h.TimeoutMs = hookTimeoutMs
			if h.Version == "" {
				h.Version = ext.Manifest.Version
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			delete(s.hooks, event)
		} else {
			s.hooks[event] = kept
		}
	}
	for id, skill := range s.skills {
		if skill.ExtensionID != "" {
			if _, ok := s.extensions[skill.ExtensionID]; !ok {
				delete(s.skills, id)
			}
			continue
		}
		slash := strings.Index(id, "/")
		if slash < 0 {
			continue
		}
		owner := id[:slash]
		ext, ok := s.extensions[owner]
		if !ok || !contains(ext.Manifest.SkillsExported, id[slash+1:]) {
			// Registered by an extension that was uninstalled while the old
			// uninstall left its skills behind.
			delete(s.skills, id)
			continue
		}
		skill.ExtensionID = owner
	}
	for _, ext := range s.extensions {
		ext.Isolation = isolationRemoteEndpoint
		ext.IsolationEnforcement = isolationEnforcement
		if ext.Status == "enabled" {
			if err := verifyStoredManifest(ext.Manifest); err != nil {
				ext.Status = "disabled"
				ext.StatusReason = "disabled at load: " + err.Error()
			}
		}
	}
	for _, d := range s.canaries {
		if d.Status == "active" && d.Hooks == nil {
			d.Hooks = canaryHooks(d.CanaryManifest)
		}
	}
}

func canaryHooks(m ExtensionManifest) map[string]*Hook {
	hooks := make(map[string]*Hook, len(m.HooksSubscribed))
	for _, event := range m.HooksSubscribed {
		hooks[event] = newHook(m, event)
	}
	return hooks
}
