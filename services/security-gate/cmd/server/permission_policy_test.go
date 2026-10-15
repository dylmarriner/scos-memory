package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testPrincipal = "11111111-1111-1111-1111-111111111111"
	testDelegator = "22222222-2222-2222-2222-222222222222"
)

func timePtr(t time.Time) *time.Time { return &t }

func readCheck(resource string, class DataClass) PermissionCheck {
	return PermissionCheck{Principal: testPrincipal, Tenant: "t", Action: "memory.read", Resource: resource, DataClass: class}
}

func TestEvaluatePermissionDeniesUnknownDataClass(t *testing.T) {
	grants := []PermissionGrant{{ID: "g", Action: "*", Resource: "*", MaxClass: ClassSecret}}
	for _, class := range []DataClass{"top-secret", "PUBLIC", "unclassified"} {
		d := evaluatePermission(readCheck("memory/a", class), permissionInputs{Now: time.Now(), TokenMaxClass: ClassSecret, Grants: grants})
		if d.Allowed {
			t.Fatalf("unknown data class %q must be denied even under a wildcard secret grant", class)
		}
		if d.Code != "unknown_data_class" || !strings.Contains(d.Reason, string(class)) {
			t.Fatalf("expected an unknown_data_class reason naming %q, got %+v", class, d)
		}
	}
}

func TestEvaluatePermissionChecksActionAndResourceAgainstGrants(t *testing.T) {
	now := time.Now()
	grants := []PermissionGrant{
		{ID: "g-read", Action: "memory.read", Resource: "memory/project-a/*", MaxClass: ClassConfidential},
		{ID: "g-write", Action: "memory.write", Resource: "memory/project-a/notes", MaxClass: ClassInternal},
	}
	in := permissionInputs{Now: now, TokenMaxClass: ClassRestricted, Grants: grants}
	cases := []struct {
		name    string
		check   PermissionCheck
		allowed bool
		grant   string
		code    string
	}{
		{"read inside prefix grant", PermissionCheck{Principal: testPrincipal, Action: "memory.read", Resource: "memory/project-a/doc-1", DataClass: ClassInternal}, true, "g-read", "permitted"},
		{"write on exact resource", PermissionCheck{Principal: testPrincipal, Action: "memory.write", Resource: "memory/project-a/notes", DataClass: ClassPublic}, true, "g-write", "permitted"},
		{"read outside resource prefix", PermissionCheck{Principal: testPrincipal, Action: "memory.read", Resource: "memory/project-b/doc-1", DataClass: ClassPublic}, false, "", "no_grant"},
		{"prefix grant does not match its own stem without separator", PermissionCheck{Principal: testPrincipal, Action: "memory.read", Resource: "memory/project-a", DataClass: ClassPublic}, false, "", "no_grant"},
		{"action not granted on resource", PermissionCheck{Principal: testPrincipal, Action: "memory.delete", Resource: "memory/project-a/doc-1", DataClass: ClassPublic}, false, "", "no_grant"},
		{"write outside exact resource", PermissionCheck{Principal: testPrincipal, Action: "memory.write", Resource: "memory/project-a/other", DataClass: ClassPublic}, false, "", "no_grant"},
		{"class above grant", PermissionCheck{Principal: testPrincipal, Action: "memory.write", Resource: "memory/project-a/notes", DataClass: ClassConfidential}, false, "", "data_class_exceeds_grant"},
		{"class above token", PermissionCheck{Principal: testPrincipal, Action: "memory.read", Resource: "memory/project-a/doc", DataClass: ClassSecret}, false, "", "data_class_exceeds_token"},
		{"empty action", PermissionCheck{Principal: testPrincipal, Resource: "memory/project-a/doc", DataClass: ClassPublic}, false, "", "missing_action_or_resource"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := evaluatePermission(tc.check, in)
			if d.Allowed != tc.allowed || d.GrantID != tc.grant || d.Code != tc.code {
				t.Fatalf("expected allowed=%v grant=%q code=%q, got %+v", tc.allowed, tc.grant, tc.code, d)
			}
			if d.Reason == "" {
				t.Fatal("every decision must carry a reason")
			}
		})
	}
}

func TestEvaluatePermissionDeniesWithNoGrants(t *testing.T) {
	d := evaluatePermission(readCheck("memory/a", ClassPublic), permissionInputs{Now: time.Now(), TokenMaxClass: ClassRestricted})
	if d.Allowed || d.Code != "no_grant" {
		t.Fatalf("a principal with no grants must be denied, got %+v", d)
	}
}

func TestEvaluatePermissionDeniesExpiredGrant(t *testing.T) {
	now := time.Now()
	expired := []PermissionGrant{{ID: "g", Action: "memory.read", Resource: "memory/*", MaxClass: ClassInternal, ExpiresAt: timePtr(now.Add(-time.Minute))}}
	d := evaluatePermission(readCheck("memory/a", ClassPublic), permissionInputs{Now: now, TokenMaxClass: ClassInternal, Grants: expired})
	if d.Allowed || d.Code != "grant_expired" {
		t.Fatalf("an expired grant must deny with grant_expired, got %+v", d)
	}
	// Expiry is exclusive: a grant is dead at its expires_at instant.
	atBoundary := []PermissionGrant{{ID: "g", Action: "memory.read", Resource: "memory/*", MaxClass: ClassInternal, ExpiresAt: timePtr(now)}}
	if d := evaluatePermission(readCheck("memory/a", ClassPublic), permissionInputs{Now: now, TokenMaxClass: ClassInternal, Grants: atBoundary}); d.Allowed {
		t.Fatalf("a grant must not be usable at its expiry instant, got %+v", d)
	}
	live := []PermissionGrant{{ID: "g-live", Action: "memory.read", Resource: "memory/*", MaxClass: ClassInternal, ExpiresAt: timePtr(now.Add(time.Hour))}}
	if d := evaluatePermission(readCheck("memory/a", ClassPublic), permissionInputs{Now: now, TokenMaxClass: ClassInternal, Grants: append(expired, live...)}); !d.Allowed || d.GrantID != "g-live" {
		t.Fatalf("a live grant alongside an expired one should allow, got %+v", d)
	}
}

func TestEvaluatePermissionRequestedExpiry(t *testing.T) {
	now := time.Now()
	grants := []PermissionGrant{{ID: "g", Action: "memory.read", Resource: "memory/*", MaxClass: ClassInternal, ExpiresAt: timePtr(now.Add(time.Hour))}}
	in := permissionInputs{Now: now, TokenMaxClass: ClassInternal, Grants: grants}

	in.RequestedUntil = timePtr(now.Add(30 * time.Minute))
	if d := evaluatePermission(readCheck("memory/a", ClassPublic), in); !d.Allowed {
		t.Fatalf("requested lifetime inside the grant should allow, got %+v", d)
	}
	in.RequestedUntil = timePtr(now.Add(2 * time.Hour))
	if d := evaluatePermission(readCheck("memory/a", ClassPublic), in); d.Allowed || d.Code != "requested_expiry_exceeds_grant" {
		t.Fatalf("requested lifetime beyond the grant must deny, got %+v", d)
	}
	in.RequestedUntil = timePtr(now.Add(-time.Second))
	if d := evaluatePermission(readCheck("memory/a", ClassPublic), in); d.Allowed || d.Code != "requested_expiry_past" {
		t.Fatalf("a requested expiry in the past must deny, got %+v", d)
	}
}

func TestEvaluatePermissionSecretRequiresExplicitSecretGrant(t *testing.T) {
	now := time.Now()
	check := readCheck("vault/key", ClassSecret)
	restricted := []PermissionGrant{{ID: "g", Action: "*", Resource: "*", MaxClass: ClassRestricted}}
	if d := evaluatePermission(check, permissionInputs{Now: now, TokenMaxClass: ClassSecret, Grants: restricted}); d.Allowed {
		t.Fatalf("secret must not be allowed by a restricted grant, got %+v", d)
	}
	secret := []PermissionGrant{{ID: "g-secret", Action: "memory.read", Resource: "vault/*", MaxClass: ClassSecret}}
	if d := evaluatePermission(check, permissionInputs{Now: now, TokenMaxClass: ClassSecret, Grants: secret}); !d.Allowed {
		t.Fatalf("an explicit secret grant on a secret-class token should allow, got %+v", d)
	}
	if d := evaluatePermission(check, permissionInputs{Now: now, TokenMaxClass: ClassRestricted, Grants: secret}); d.Allowed {
		t.Fatalf("a secret grant must not lift the token's own class ceiling, got %+v", d)
	}
}

func TestEvaluatePermissionDelegation(t *testing.T) {
	now := time.Now()
	delegatorGrants := []PermissionGrant{{ID: "g-delegator", Principal: testDelegator, Action: "memory.read", Resource: "memory/project-a/*", MaxClass: ClassConfidential, ExpiresAt: timePtr(now.Add(2 * time.Hour))}}
	delegation := func() *PermissionDelegation {
		return &PermissionDelegation{
			ID: "d-1", Delegator: testDelegator, Delegate: testPrincipal,
			Action: "memory.read", Resource: "memory/project-a/shared/*", MaxClass: ClassInternal,
			ExpiresAt: now.Add(time.Hour),
		}
	}
	inputs := func(d *PermissionDelegation, grants []PermissionGrant) permissionInputs {
		return permissionInputs{Now: now, TokenMaxClass: ClassRestricted, DelegationRequested: true, Delegation: d, Grants: grants}
	}

	if d := evaluatePermission(readCheck("memory/project-a/shared/doc", ClassInternal), inputs(delegation(), delegatorGrants)); !d.Allowed || d.GrantID != "g-delegator" || d.DelegationID != "d-1" {
		t.Fatalf("a request inside the delegation and the delegator's grant should allow, got %+v", d)
	}

	cases := []struct {
		name   string
		check  PermissionCheck
		mutate func(*PermissionDelegation)
		grants []PermissionGrant
		code   string
	}{
		{"resource beyond the delegation but inside the delegator's grant", readCheck("memory/project-a/private/doc", ClassInternal), nil, delegatorGrants, "delegation_resource_out_of_scope"},
		{"class beyond the delegation", readCheck("memory/project-a/shared/doc", ClassConfidential), nil, delegatorGrants, "data_class_exceeds_delegation"},
		{"action beyond the delegation", PermissionCheck{Principal: testPrincipal, Action: "memory.write", Resource: "memory/project-a/shared/doc", DataClass: ClassPublic}, nil, delegatorGrants, "delegation_action_out_of_scope"},
		{"delegation broader than the delegator's grant", readCheck("memory/project-b/doc", ClassInternal), func(d *PermissionDelegation) { d.Resource = "memory/*" }, delegatorGrants, "no_grant"},
		{"delegator's grant has since expired", readCheck("memory/project-a/shared/doc", ClassInternal), nil, []PermissionGrant{{ID: "g", Action: "memory.read", Resource: "memory/project-a/*", MaxClass: ClassConfidential, ExpiresAt: timePtr(now.Add(-time.Minute))}}, "grant_expired"},
		{"delegator holds no grant", readCheck("memory/project-a/shared/doc", ClassInternal), nil, nil, "no_grant"},
		{"expired delegation", readCheck("memory/project-a/shared/doc", ClassInternal), func(d *PermissionDelegation) { d.ExpiresAt = now.Add(-time.Second) }, delegatorGrants, "delegation_expired"},
		{"revoked delegation", readCheck("memory/project-a/shared/doc", ClassInternal), func(d *PermissionDelegation) { d.RevokedAt = timePtr(now.Add(-time.Second)) }, delegatorGrants, "delegation_revoked"},
		{"delegation issued to someone else", readCheck("memory/project-a/shared/doc", ClassInternal), func(d *PermissionDelegation) { d.Delegate = "33333333-3333-3333-3333-333333333333" }, delegatorGrants, "delegation_wrong_delegate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := delegation()
			if tc.mutate != nil {
				tc.mutate(d)
			}
			got := evaluatePermission(tc.check, inputs(d, tc.grants))
			if got.Allowed || got.Code != tc.code {
				t.Fatalf("expected denial %q, got %+v", tc.code, got)
			}
		})
	}

	if d := evaluatePermission(readCheck("memory/project-a/shared/doc", ClassInternal), inputs(nil, delegatorGrants)); d.Allowed || d.Code != "delegation_not_found" {
		t.Fatalf("a delegated check naming no delegation must deny, got %+v", d)
	}
	in := inputs(delegation(), delegatorGrants)
	in.RequestedUntil = timePtr(now.Add(90 * time.Minute))
	if d := evaluatePermission(readCheck("memory/project-a/shared/doc", ClassInternal), in); d.Allowed || d.Code != "requested_expiry_exceeds_delegation" {
		t.Fatalf("a requested lifetime beyond the delegation must deny, got %+v", d)
	}
}

func TestDelegationWithinGrants(t *testing.T) {
	now := time.Now()
	grants := []PermissionGrant{{ID: "g", Action: "memory.*", Resource: "memory/project-a/*", MaxClass: ClassConfidential, ExpiresAt: timePtr(now.Add(2 * time.Hour))}}
	base := PermissionDelegation{Action: "memory.read", Resource: "memory/project-a/shared/*", MaxClass: ClassInternal, ExpiresAt: now.Add(time.Hour)}
	if id, ok := delegationWithinGrants(base, grants, now); !ok || id != "g" {
		t.Fatalf("a narrower delegation should be covered, got %q %v", id, ok)
	}
	cases := map[string]func(*PermissionDelegation){
		"broader resource":       func(d *PermissionDelegation) { d.Resource = "memory/*" },
		"wildcard action":        func(d *PermissionDelegation) { d.Action = "*" },
		"higher class":           func(d *PermissionDelegation) { d.MaxClass = ClassRestricted },
		"outlives the grant":     func(d *PermissionDelegation) { d.ExpiresAt = now.Add(3 * time.Hour) },
		"unknown class":          func(d *PermissionDelegation) { d.MaxClass = "bogus" },
		"other project resource": func(d *PermissionDelegation) { d.Resource = "memory/project-b/x" },
	}
	for name, mutate := range cases {
		d := base
		mutate(&d)
		if _, ok := delegationWithinGrants(d, grants, now); ok {
			t.Fatalf("%s: delegation must not be covered", name)
		}
	}
	expired := []PermissionGrant{{ID: "g", Action: "*", Resource: "*", MaxClass: ClassSecret, ExpiresAt: timePtr(now.Add(-time.Second))}}
	if _, ok := delegationWithinGrants(base, expired, now); ok {
		t.Fatal("an expired grant must not cover a new delegation")
	}
}

func TestPatternHelpers(t *testing.T) {
	for _, p := range []string{"a", "a/*", "*", "memory.read"} {
		if !validPattern(p) {
			t.Fatalf("%q should be a valid pattern", p)
		}
	}
	for _, p := range []string{"", " ", "a*b", "**", "*a", " a"} {
		if validPattern(p) {
			t.Fatalf("%q should be rejected", p)
		}
	}
	if !patternMatches("*", "anything") || patternMatches("a/*", "b/x") || !patternMatches("a/*", "a/x") || patternMatches("a", "a/x") {
		t.Fatal("patternMatches semantics regressed")
	}
	if !patternWithin("a/b/*", "a/*") || patternWithin("a/*", "a/b/*") || patternWithin("a/*", "a/x") || !patternWithin("a/x", "a/x") || !patternWithin("a/x", "*") {
		t.Fatal("patternWithin semantics regressed")
	}
}

func TestPermissionGrantRequiresIssuerSecret(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	body := `{"tenant":"123e4567-e89b-12d3-a456-426614174000","principal":"123e4567-e89b-12d3-a456-426614174001","action":"read","resource":"*","max_data_class":"internal"}`
	for _, secret := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/permissions/grants", bytes.NewBufferString(body))
		if secret != "" {
			req.Header.Set("X-SCOS-Issuer-Secret", secret)
		}
		rec := httptest.NewRecorder()
		s.handlePermissionGrants(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for issuer secret %q, got %d", secret, rec.Code)
		}
	}
}

func TestPermissionGrantValidatesBeforePersisting(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	const tenant, principal = "123e4567-e89b-12d3-a456-426614174000", "123e4567-e89b-12d3-a456-426614174001"
	cases := map[string]string{
		"unknown class":      `{"tenant":"` + tenant + `","principal":"` + principal + `","action":"read","resource":"*","max_data_class":"top-secret"}`,
		"missing class":      `{"tenant":"` + tenant + `","principal":"` + principal + `","action":"read","resource":"*"}`,
		"inner wildcard":     `{"tenant":"` + tenant + `","principal":"` + principal + `","action":"read","resource":"a*b","max_data_class":"internal"}`,
		"empty action":       `{"tenant":"` + tenant + `","principal":"` + principal + `","resource":"*","max_data_class":"internal"}`,
		"non-uuid tenant":    `{"tenant":"t","principal":"` + principal + `","action":"read","resource":"*","max_data_class":"internal"}`,
		"past expiry":        `{"tenant":"` + tenant + `","principal":"` + principal + `","action":"read","resource":"*","max_data_class":"internal","expires_at":"2000-01-01T00:00:00Z"}`,
		"malformed expiry":   `{"tenant":"` + tenant + `","principal":"` + principal + `","action":"read","resource":"*","max_data_class":"internal","expires_at":"tomorrow"}`,
		"non-uuid principal": `{"tenant":"` + tenant + `","principal":"p","action":"read","resource":"*","max_data_class":"internal"}`,
	}
	for name, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/permissions/grants", bytes.NewBufferString(body))
		req.Header.Set("X-SCOS-Issuer-Secret", "issuer-secret")
		rec := httptest.NewRecorder()
		s.handlePermissionGrants(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestPermissionDelegationRequiresBearerToken(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/permissions/delegations", bytes.NewBufferString(`{"delegate":"123e4567-e89b-12d3-a456-426614174001","action":"read","resource":"*","max_data_class":"public","expires_at":"2999-01-01T00:00:00Z"}`))
	req.Header.Set("Authorization", "Bearer forged.signature")
	rec := httptest.NewRecorder()
	s.handlePermissionDelegations(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a forged token, got %d", rec.Code)
	}
}
