package main

import (
	"fmt"
	"strings"
	"time"
)

// ── permission grants and delegations (§27) ─────────────────────────────
//
// A permission check is decided against the grants the requesting
// principal actually holds, never against the data class alone. A grant
// names one action pattern and one resource pattern, caps the data class it
// covers, and may expire. A delegation lets one principal (the delegator)
// pass a bounded slice of its own grant to another principal (the
// delegate); it always expires, and it confers nothing the delegator does
// not itself still hold at the moment of the check.
//
// Patterns are either an exact value or a prefix ending in a single
// trailing "*" ("memory/project-a/*"); "*" alone matches everything. A "*"
// anywhere other than the last character is rejected when a grant or
// delegation is created, so a pattern can never be ambiguous.

// PermissionGrant is one durable authority a principal holds.
type PermissionGrant struct {
	ID        string     `json:"id"`
	Tenant    string     `json:"tenant"`
	Principal string     `json:"principal"`
	Action    string     `json:"action"`
	Resource  string     `json:"resource"`
	MaxClass  DataClass  `json:"max_data_class"`
	GrantedAt string     `json:"granted_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// PermissionDelegation is a bounded, expiring hand-off of part of a
// delegator's grant to a delegate.
type PermissionDelegation struct {
	ID        string     `json:"id"`
	Tenant    string     `json:"tenant"`
	Delegator string     `json:"delegator"`
	Delegate  string     `json:"delegate"`
	Action    string     `json:"action"`
	Resource  string     `json:"resource"`
	MaxClass  DataClass  `json:"max_data_class"`
	CreatedAt string     `json:"created_at,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// permissionInputs is everything a permission decision depends on beyond
// the check itself. Grants are the requesting principal's own grants for a
// direct check, or the delegator's grants for a delegated one.
type permissionInputs struct {
	Now                 time.Time
	TokenMaxClass       DataClass
	RequestedUntil      *time.Time
	Grants              []PermissionGrant
	DelegationRequested bool
	Delegation          *PermissionDelegation
}

// permissionDecision is the outcome of one evaluation. Code is a stable,
// low-cardinality label for metrics; Reason is the human-readable
// explanation returned to the caller and written to the audit trail.
type permissionDecision struct {
	Allowed      bool
	Reason       string
	Code         string
	GrantID      string
	DelegationID string
}

func deny(code, reason string) permissionDecision {
	return permissionDecision{Allowed: false, Code: code, Reason: reason}
}

// validPattern reports whether p is a non-empty exact value or a prefix
// with exactly one trailing "*".
func validPattern(p string) bool {
	if strings.TrimSpace(p) == "" || p != strings.TrimSpace(p) {
		return false
	}
	star := strings.Index(p, "*")
	return star == -1 || star == len(p)-1
}

func splitPattern(p string) (base string, wildcard bool) {
	if strings.HasSuffix(p, "*") {
		return strings.TrimSuffix(p, "*"), true
	}
	return p, false
}

// patternMatches reports whether the concrete value v falls under pattern.
func patternMatches(pattern, v string) bool {
	base, wildcard := splitPattern(pattern)
	if wildcard {
		return strings.HasPrefix(v, base)
	}
	return v == base
}

// patternWithin reports whether every value inner can match is also matched
// by outer, i.e. inner is no broader than outer.
func patternWithin(inner, outer string) bool {
	outerBase, outerWild := splitPattern(outer)
	innerBase, innerWild := splitPattern(inner)
	if !outerWild {
		return !innerWild && innerBase == outerBase
	}
	return strings.HasPrefix(innerBase, outerBase)
}

// knownClass reports whether c is one of the five §27 classes.
func knownClass(c DataClass) bool {
	return classRank(c) >= 0
}

// classWithin reports whether requested is a known class no more sensitive
// than limit (which must itself be known).
func classWithin(requested, limit DataClass) bool {
	return knownClass(requested) && knownClass(limit) && classRank(requested) <= classRank(limit)
}

// evaluatePermission decides check against the supplied grants and, for a
// delegated check, the delegation. It fails closed: an unknown data class,
// an empty action or resource, a missing, revoked or expired delegation, or
// the absence of a live grant covering the action, resource, data class
// and requested lifetime all deny.
func evaluatePermission(check PermissionCheck, in permissionInputs) permissionDecision {
	if !knownClass(check.DataClass) {
		return deny("unknown_data_class", fmt.Sprintf("unknown data class %q", check.DataClass))
	}
	if strings.TrimSpace(check.Action) == "" || strings.TrimSpace(check.Resource) == "" {
		return deny("missing_action_or_resource", "action and resource are required")
	}
	if !classWithin(check.DataClass, in.TokenMaxClass) {
		return deny("data_class_exceeds_token", "requested data class exceeds token authorisation")
	}
	if in.RequestedUntil != nil && !in.Now.Before(*in.RequestedUntil) {
		return deny("requested_expiry_past", "requested expiry is not in the future")
	}

	holder := "principal"
	var delegationID string
	if in.DelegationRequested {
		d := in.Delegation
		if d == nil {
			return deny("delegation_not_found", "delegation not found")
		}
		delegationID = d.ID
		switch {
		case d.Delegate != check.Principal:
			return deny("delegation_wrong_delegate", "delegation was not granted to this principal")
		case d.RevokedAt != nil:
			return deny("delegation_revoked", "delegation has been revoked")
		case !in.Now.Before(d.ExpiresAt):
			return deny("delegation_expired", "delegation has expired")
		case !patternMatches(d.Action, check.Action):
			return deny("delegation_action_out_of_scope", fmt.Sprintf("action %q is outside the delegation's scope", check.Action))
		case !patternMatches(d.Resource, check.Resource):
			return deny("delegation_resource_out_of_scope", fmt.Sprintf("resource %q is outside the delegation's scope", check.Resource))
		case !classWithin(check.DataClass, d.MaxClass):
			return deny("data_class_exceeds_delegation", "requested data class exceeds the delegation's limit")
		case in.RequestedUntil != nil && in.RequestedUntil.After(d.ExpiresAt):
			return deny("requested_expiry_exceeds_delegation", "requested expiry is later than the delegation's expiry")
		}
		holder = "delegator"
	}

	// Among grants that cover the action and resource, remember the most
	// specific reason they failed so the caller learns why, not just that.
	const (
		failNone = iota
		failExpired
		failRequestedExpiry
		failClass
	)
	worst := failNone
	for _, g := range in.Grants {
		if !patternMatches(g.Action, check.Action) || !patternMatches(g.Resource, check.Resource) {
			continue
		}
		switch {
		case g.ExpiresAt != nil && !in.Now.Before(*g.ExpiresAt):
			worst = max(worst, failExpired)
		case in.RequestedUntil != nil && g.ExpiresAt != nil && in.RequestedUntil.After(*g.ExpiresAt):
			worst = max(worst, failRequestedExpiry)
		case !classWithin(check.DataClass, g.MaxClass):
			worst = max(worst, failClass)
		default:
			reason := "permitted by grant " + g.ID
			if delegationID != "" {
				reason += " held by delegator " + in.Delegation.Delegator + " via delegation " + delegationID
			}
			return permissionDecision{Allowed: true, Code: "permitted", Reason: reason, GrantID: g.ID, DelegationID: delegationID}
		}
	}
	var d permissionDecision
	switch worst {
	case failClass:
		d = deny("data_class_exceeds_grant", fmt.Sprintf("requested data class exceeds every %s grant covering action %q on resource %q", holder, check.Action, check.Resource))
	case failRequestedExpiry:
		d = deny("requested_expiry_exceeds_grant", fmt.Sprintf("requested expiry is later than every %s grant covering action %q on resource %q", holder, check.Action, check.Resource))
	case failExpired:
		d = deny("grant_expired", fmt.Sprintf("every %s grant covering action %q on resource %q has expired", holder, check.Action, check.Resource))
	default:
		d = deny("no_grant", fmt.Sprintf("%s holds no grant covering action %q on resource %q", holder, check.Action, check.Resource))
	}
	d.DelegationID = delegationID
	return d
}

// delegationWithinGrants reports whether a proposed delegation is covered
// in full by one of the delegator's live grants: its action and resource
// patterns no broader than the grant's, its data class no higher, and its
// expiry no later than the grant's. It returns the covering grant's id.
func delegationWithinGrants(d PermissionDelegation, grants []PermissionGrant, now time.Time) (string, bool) {
	for _, g := range grants {
		if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
			continue
		}
		if g.ExpiresAt != nil && d.ExpiresAt.After(*g.ExpiresAt) {
			continue
		}
		if patternWithin(d.Action, g.Action) && patternWithin(d.Resource, g.Resource) && classWithin(d.MaxClass, g.MaxClass) {
			return g.ID, true
		}
	}
	return "", false
}
