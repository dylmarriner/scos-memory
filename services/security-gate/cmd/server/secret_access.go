package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// secretCaller is everything the resolve and audit paths may rely on about
// who is calling. It is derived exclusively from the authenticated request:
// the tenant/principal pair from resolveIdentity, and agent, trace and
// scopes from the signed claims packages/auth verified and attached to the
// context. Nothing here is read from a request body.
type secretCaller struct {
	identity requestIdentity
	agentID  string
	traceID  string
	scopes   []string
	claims   bool
}

func callerFromRequest(r *http.Request, identity requestIdentity) secretCaller {
	caller := secretCaller{identity: identity}
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		caller.claims = true
		caller.agentID = claims.AgentID
		caller.traceID = claims.TraceID
		caller.scopes = claims.Scopes
	}
	return caller
}

// Secret reference scope selectors. A reference's scope is a comma- or
// whitespace-separated list of selectors; a caller may resolve the
// reference only if at least one selector matches it. Selectors are always
// evaluated inside the reference's own tenant (row-level security already
// confines the lookup to the caller's tenant), so "tenant" means "any
// authenticated principal of the owning tenant", never a cross-tenant grant.
const (
	scopeSelectorTenant    = "tenant"
	scopeSelectorPrincipal = "principal:"
	scopeSelectorAgent     = "agent:"
)

// dataClassScopePrefix names the claims scope that grants data-class
// clearance: "data_class:confidential" lets the bearer resolve secret
// references classed public through confidential. The highest such scope
// wins. The global "admin" scope clears every class, matching
// auth.HasScope's documented cross-scope escape hatch.
const dataClassScopePrefix = "data_class:"

type scopeSelector struct {
	kind  string // tenant, principal, agent
	value string
}

// parseSecretScope validates and splits a stored scope. An empty scope
// parses to no selectors -- such a reference can be stored and listed but
// never resolved.
func parseSecretScope(scope string) ([]scopeSelector, error) {
	fields := strings.FieldsFunc(scope, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	selectors := make([]scopeSelector, 0, len(fields))
	for _, field := range fields {
		switch {
		case field == scopeSelectorTenant:
			selectors = append(selectors, scopeSelector{kind: "tenant"})
		case strings.HasPrefix(field, scopeSelectorPrincipal) && len(field) > len(scopeSelectorPrincipal):
			selectors = append(selectors, scopeSelector{kind: "principal", value: strings.TrimPrefix(field, scopeSelectorPrincipal)})
		case strings.HasPrefix(field, scopeSelectorAgent) && len(field) > len(scopeSelectorAgent):
			selectors = append(selectors, scopeSelector{kind: "agent", value: strings.TrimPrefix(field, scopeSelectorAgent)})
		default:
			return nil, fmt.Errorf("unsupported scope selector %q: use tenant, principal:<id> or agent:<id>", field)
		}
	}
	return selectors, nil
}

func (sel scopeSelector) matches(caller secretCaller) bool {
	switch sel.kind {
	case "tenant":
		return caller.identity.TenantID != ""
	case "principal":
		return sel.value == caller.identity.PrincipalID
	case "agent":
		return caller.agentID != "" && sel.value == caller.agentID
	default:
		return false
	}
}

// clearance is the highest data class the caller's signed claims authorise.
// A caller without claims (only reachable when no auth middleware is in
// front of the handler) has no clearance at all; classRank of "" is -1, so
// such a caller is refused every class.
func (caller secretCaller) clearance() DataClass {
	best := DataClass("")
	for _, scope := range caller.scopes {
		if scope == "admin" {
			return ClassSecret
		}
		class, ok := strings.CutPrefix(scope, dataClassScopePrefix)
		if !ok {
			continue
		}
		if classRank(DataClass(class)) > classRank(best) {
			best = DataClass(class)
		}
	}
	return best
}

// authorizeSecretResolve decides whether caller may receive ref's live
// value. Every refusal reason is safe to return to the caller and to record
// in the audit trail: it names the rule that failed, never the value.
func authorizeSecretResolve(caller secretCaller, ref *SecretReference) (bool, string) {
	if !caller.claims {
		return false, "resolve requires signed caller claims"
	}
	selectors, err := parseSecretScope(ref.Scope)
	if err != nil {
		return false, "secret reference scope is invalid: " + err.Error()
	}
	if len(selectors) == 0 {
		return false, "secret reference has no resolve scope"
	}
	inScope := false
	for _, sel := range selectors {
		if sel.matches(caller) {
			inScope = true
			break
		}
	}
	if !inScope {
		return false, "caller is outside the secret reference scope"
	}
	required := ref.DataClass
	if classRank(required) < 0 {
		return false, "secret reference has an unknown data class"
	}
	if classRank(caller.clearance()) < classRank(required) {
		return false, "caller clearance does not cover data class " + string(required)
	}
	return true, ""
}

// validateSecretReferencePolicy normalises and checks the access policy a
// caller sets on a secret reference at write time, so an unresolvable or
// mis-typed policy is rejected up front rather than silently denying every
// later resolve.
func validateSecretReferencePolicy(ref *SecretReference) error {
	if ref.DataClass == "" {
		ref.DataClass = ClassSecret
	}
	if classRank(ref.DataClass) < 0 {
		return fmt.Errorf("data_class must be one of public, internal, confidential, restricted, secret")
	}
	if _, err := parseSecretScope(ref.Scope); err != nil {
		return err
	}
	return nil
}
