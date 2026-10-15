package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// decidePermission loads the authority a check can rely on and evaluates
// it. A direct check is decided against the token principal's own grants;
// a check naming delegated_authority is decided against that delegation
// and the delegator's grants, never the delegate's own.
func (s *securityServer) decidePermission(ctx context.Context, identity requestIdentity, check PermissionCheck, token *AuthToken, requestedUntil *time.Time, now time.Time) (permissionDecision, error) {
	in := permissionInputs{Now: now, TokenMaxClass: token.DataClass, RequestedUntil: requestedUntil}
	holder := token.Principal
	if delegationID := strings.TrimSpace(check.DelegatedAuthority); delegationID != "" {
		in.DelegationRequested = true
		d, ok, err := getPermissionDelegation(ctx, s.db, identity, delegationID)
		if err != nil {
			return permissionDecision{}, err
		}
		if !ok {
			return evaluatePermission(check, in), nil
		}
		in.Delegation = &d
		holder = d.Delegator
	}
	grants, err := listPermissionGrants(ctx, s.db, identity, holder)
	if err != nil {
		return permissionDecision{}, err
	}
	in.Grants = grants
	return evaluatePermission(check, in), nil
}

// parseOptionalExpiry parses an optional RFC3339 timestamp.
func parseOptionalExpiry(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("expires_at must be an RFC3339 timestamp")
	}
	t = t.UTC()
	return &t, nil
}

// handlePermissionGrants issues a grant. Granting authority is an
// administrative act, so it requires the issuer secret exactly as token
// issuance does.
func (s *securityServer) handlePermissionGrants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validIssuerSecret(r.Header.Get("X-SCOS-Issuer-Secret")) {
		http.Error(w, "issuer authentication required", http.StatusUnauthorized)
		return
	}
	var req struct {
		Tenant    string    `json:"tenant"`
		Principal string    `json:"principal"`
		Action    string    `json:"action"`
		Resource  string    `json:"resource"`
		MaxClass  DataClass `json:"max_data_class"`
		ExpiresAt string    `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !isUUID(req.Tenant) {
		http.Error(w, "tenant must be a tenant UUID", http.StatusBadRequest)
		return
	}
	if !isUUID(req.Principal) {
		http.Error(w, "principal must be a principal UUID", http.StatusBadRequest)
		return
	}
	if !validPattern(req.Action) || !validPattern(req.Resource) {
		http.Error(w, "action and resource must be non-empty and may contain '*' only as the final character", http.StatusBadRequest)
		return
	}
	if !knownClass(req.MaxClass) {
		http.Error(w, "max_data_class must be one of public, internal, confidential, restricted, secret", http.StatusBadRequest)
		return
	}
	expiresAt, err := parseOptionalExpiry(req.ExpiresAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if expiresAt != nil && !time.Now().UTC().Before(*expiresAt) {
		http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
		return
	}

	identity := requestIdentity{TenantID: req.Tenant, PrincipalID: req.Principal}
	grant, err := insertPermissionGrant(r.Context(), s.db, identity, PermissionGrant{
		Principal: req.Principal, Action: req.Action, Resource: req.Resource, MaxClass: req.MaxClass, ExpiresAt: expiresAt,
	})
	if errors.Is(err, errPrincipalNotInTenant) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "unable to persist grant", http.StatusInternalServerError)
		return
	}
	reason := fmt.Sprintf("permission grant issued: action %q on resource %q up to %s", grant.Action, grant.Resource, grant.MaxClass)
	if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: req.Principal, Tenant: req.Tenant, Resource: "grant:" + grant.ID, DataClass: grant.MaxClass, Result: "allowed", Reason: reason}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, grant)
}

// handlePermissionGrantRevoke revokes a grant; like issuance it requires
// the issuer secret.
func (s *securityServer) handlePermissionGrantRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validIssuerSecret(r.Header.Get("X-SCOS-Issuer-Secret")) {
		http.Error(w, "issuer authentication required", http.StatusUnauthorized)
		return
	}
	var req struct {
		Tenant    string `json:"tenant"`
		Principal string `json:"principal"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !isUUID(req.Tenant) || !isUUID(req.Principal) {
		http.Error(w, "tenant and principal must be UUIDs", http.StatusBadRequest)
		return
	}
	identity := requestIdentity{TenantID: req.Tenant, PrincipalID: req.Principal}
	revoked, err := revokePermissionGrant(r.Context(), s.db, identity, req.ID)
	if err != nil {
		http.Error(w, "unable to revoke grant", http.StatusInternalServerError)
		return
	}
	if !revoked {
		http.Error(w, "grant not found", http.StatusNotFound)
		return
	}
	if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: req.Principal, Tenant: req.Tenant, Resource: "grant:" + req.ID, Result: "allowed", Reason: "permission grant revoked"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"revoked": true})
}

// handlePermissionDelegations lets the bearer token's principal delegate
// part of its own authority to another principal in the same tenant. The
// delegation must expire, and must fit entirely inside one of the
// delegator's live grants and within the delegator's token data class.
func (s *securityServer) handlePermissionDelegations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Delegate  string    `json:"delegate"`
		Action    string    `json:"action"`
		Resource  string    `json:"resource"`
		MaxClass  DataClass `json:"max_data_class"`
		ExpiresAt string    `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	token, ok := s.authenticatedToken(r)
	if !ok {
		http.Error(w, "valid bearer token required", http.StatusUnauthorized)
		return
	}
	if !isUUID(req.Delegate) {
		http.Error(w, "delegate must be a principal UUID", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(req.Delegate, token.Principal) {
		http.Error(w, "a principal cannot delegate to itself", http.StatusBadRequest)
		return
	}
	if !validPattern(req.Action) || !validPattern(req.Resource) {
		http.Error(w, "action and resource must be non-empty and may contain '*' only as the final character", http.StatusBadRequest)
		return
	}
	if !knownClass(req.MaxClass) {
		http.Error(w, "max_data_class must be one of public, internal, confidential, restricted, secret", http.StatusBadRequest)
		return
	}
	expiresAt, err := parseOptionalExpiry(req.ExpiresAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	if expiresAt == nil || !now.Before(*expiresAt) {
		http.Error(w, "expires_at is required and must be in the future", http.StatusBadRequest)
		return
	}

	identity := requestIdentity{TenantID: token.Tenant, PrincipalID: token.Principal}
	proposed := PermissionDelegation{Delegate: req.Delegate, Action: req.Action, Resource: req.Resource, MaxClass: req.MaxClass, ExpiresAt: *expiresAt}
	auditDenied := func(reason string) bool {
		if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: token.Principal, AgentID: token.AgentID, Tenant: token.Tenant, SessionID: token.SessionID, Resource: "delegation:" + req.Delegate, DataClass: req.MaxClass, Result: "denied", Reason: reason}); err != nil {
			http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
			return false
		}
		return true
	}
	if !classWithin(req.MaxClass, token.DataClass) {
		const reason = "delegated data class exceeds token authorisation"
		if auditDenied(reason) {
			http.Error(w, reason, http.StatusForbidden)
		}
		return
	}
	grants, err := listPermissionGrants(r.Context(), s.db, identity, token.Principal)
	if err != nil {
		http.Error(w, "unable to load grants", http.StatusInternalServerError)
		return
	}
	grantID, covered := delegationWithinGrants(proposed, grants, now)
	if !covered {
		const reason = "delegation exceeds the delegator's live grants"
		if auditDenied(reason) {
			http.Error(w, reason, http.StatusForbidden)
		}
		return
	}
	delegation, err := insertPermissionDelegation(r.Context(), s.db, identity, proposed)
	if errors.Is(err, errPrincipalNotInTenant) {
		http.Error(w, "delegate is not a principal of this tenant", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "unable to persist delegation", http.StatusInternalServerError)
		return
	}
	reason := fmt.Sprintf("delegation %s issued to %s under grant %s", delegation.ID, delegation.Delegate, grantID)
	if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: token.Principal, AgentID: token.AgentID, Tenant: token.Tenant, SessionID: token.SessionID, Resource: "delegation:" + delegation.ID, DataClass: delegation.MaxClass, Result: "allowed", Reason: reason}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, delegation)
}

// handlePermissionDelegationRevoke lets a delegator revoke a delegation it
// created.
func (s *securityServer) handlePermissionDelegationRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	token, ok := s.authenticatedToken(r)
	if !ok {
		http.Error(w, "valid bearer token required", http.StatusUnauthorized)
		return
	}
	identity := requestIdentity{TenantID: token.Tenant, PrincipalID: token.Principal}
	revoked, err := revokePermissionDelegation(r.Context(), s.db, identity, req.ID)
	if err != nil {
		http.Error(w, "unable to revoke delegation", http.StatusInternalServerError)
		return
	}
	if !revoked {
		http.Error(w, "delegation not found", http.StatusNotFound)
		return
	}
	if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: token.Principal, AgentID: token.AgentID, Tenant: token.Tenant, SessionID: token.SessionID, Resource: "delegation:" + req.ID, Result: "allowed", Reason: "delegation revoked"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"revoked": true})
}
