package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestValidEmail(t *testing.T) {
	cases := map[string]bool{
		"a@b.com":                           true,
		"":                                  false,
		"no-at-sign":                        false,
		strings.Repeat("a", 321) + "@b.com": false,
	}
	for in, want := range cases {
		if got := validEmail(in); got != want {
			t.Errorf("validEmail(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPaginationParamsDefaults(t *testing.T) {
	limit, offset := paginationParams(url.Values{})
	if limit != 50 || offset != 0 {
		t.Fatalf("expected default limit=50 offset=0, got limit=%d offset=%d", limit, offset)
	}
}

func TestPaginationParamsClampsLimit(t *testing.T) {
	limit, _ := paginationParams(url.Values{"limit": {"9999"}})
	if limit != 50 {
		t.Fatalf("expected out-of-range limit to fall back to default 50, got %d", limit)
	}
	limit, _ = paginationParams(url.Values{"limit": {"10"}})
	if limit != 10 {
		t.Fatalf("expected limit=10, got %d", limit)
	}
}

func TestPaginationParamsPageOffset(t *testing.T) {
	limit, offset := paginationParams(url.Values{"limit": {"20"}, "page": {"3"}})
	if limit != 20 || offset != 40 {
		t.Fatalf("expected limit=20 offset=40, got limit=%d offset=%d", limit, offset)
	}
}

func TestUserSearchFilterEmpty(t *testing.T) {
	where, args := userSearchFilter("", "", "")
	if where != "1=1" {
		t.Fatalf("expected trivial clause, got %q", where)
	}
	if len(args) != 0 {
		t.Fatalf("expected no args, got %v", args)
	}
}

func TestUserSearchFilterCombinesClauses(t *testing.T) {
	where, args := userSearchFilter(" Ada ", "pro", "creator")
	if len(args) != 3 {
		t.Fatalf("expected 3 positional args, got %d: %v", len(args), args)
	}
	if args[0] != "%ada%" {
		t.Fatalf("expected lowercased, trimmed, wildcarded query arg, got %v", args[0])
	}
	if args[1] != "pro" || args[2] != "creator" {
		t.Fatalf("unexpected plan/role args: %v", args[1:])
	}
	if !strings.Contains(where, "$1") || !strings.Contains(where, "$2") || !strings.Contains(where, "$3") {
		t.Fatalf("expected clause to reference all three placeholders, got %q", where)
	}
}

func TestUserSearchFilterPlanOnly(t *testing.T) {
	where, args := userSearchFilter("", "expert", "")
	if len(args) != 1 || args[0] != "expert" {
		t.Fatalf("expected single plan arg, got %v", args)
	}
	if strings.Contains(where, "platform_role") {
		t.Fatalf("did not expect a role clause, got %q", where)
	}
}

func TestValidPerks(t *testing.T) {
	if err := validPerks(nil); err == nil {
		t.Fatal("expected error for empty perks")
	}
	if err := validPerks([]string{""}); err == nil {
		t.Fatal("expected error for blank perk")
	}
	if err := validPerks([]string{strings.Repeat("x", 161)}); err == nil {
		t.Fatal("expected error for over-long perk")
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = "perk"
	}
	if err := validPerks(many); err == nil {
		t.Fatal("expected error for too many perks")
	}
	if err := validPerks([]string{"Priority support"}); err != nil {
		t.Fatalf("expected valid perks to pass, got %v", err)
	}
}

func TestValidPlanUpdate(t *testing.T) {
	valid := planUpdateInput{ID: "pro", Name: "Pro", Price: "$29/month", Perks: []string{"Advanced retrieval"}}
	if err := validPlanUpdate(valid); err != nil {
		t.Fatalf("expected valid input to pass, got %v", err)
	}
	missingID := valid
	missingID.ID = ""
	if err := validPlanUpdate(missingID); err == nil {
		t.Fatal("expected error for missing id")
	}
	missingName := valid
	missingName.Name = ""
	if err := validPlanUpdate(missingName); err == nil {
		t.Fatal("expected error for missing name")
	}
	missingPrice := valid
	missingPrice.Price = ""
	if err := validPlanUpdate(missingPrice); err == nil {
		t.Fatal("expected error for missing price")
	}
	noPerks := valid
	noPerks.Perks = nil
	if err := validPlanUpdate(noPerks); err == nil {
		t.Fatal("expected error for missing perks")
	}
}

func TestDecodePerks(t *testing.T) {
	if got := decodePerks(nil); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
	got := decodePerks([]byte(`["a","b"]`))
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected decode result: %v", got)
	}
}

// TestVaultRequiresSession exercises the failure path only -- s.current
// returns before touching the database when no session cookie is present,
// so this does not need a live Postgres. The success path (proxying to
// credential-vault with a signed claims token) needs a real credential-vault
// upstream and a shared issuer secret, so it is exercised by an integration
// run rather than this unit test.
func TestVaultRequiresSession(t *testing.T) {
	s := &server{}
	r := httptest.NewRequest(http.MethodGet, "/api/vault", nil)
	rec := httptest.NewRecorder()
	s.vault(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session cookie, got %d", rec.Code)
	}
}

// TestCodeSettingsRequiresSession mirrors TestVaultRequiresSession: the
// failure path (no session cookie) returns before touching any upstream,
// so it needs no live code-intelligence. The success path -- proxying a
// GET/PUT to code-intelligence's /v2/code/settings with a signed claims
// token -- needs a real code-intelligence upstream, a shared issuer
// secret and control-plane token, so it is exercised by an integration
// run rather than this unit test (see
// services/code-intelligence/cmd/server/pghandlers_integration_test.go's
// TestHandlePGSettingsGetDefaultsEnabledThenRoundTrips for the handler
// this proxies to).
func TestCodeSettingsRequiresSession(t *testing.T) {
	s := &server{}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		r := httptest.NewRequest(method, "/api/code/settings", nil)
		rec := httptest.NewRecorder()
		s.codeSettings(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 without a session cookie, got %d", method, rec.Code)
		}
	}
}

func TestCodeSettingsRejectsUnsupportedMethod(t *testing.T) {
	s := &server{}
	r := httptest.NewRequest(http.MethodDelete, "/api/code/settings", nil)
	rec := httptest.NewRecorder()
	s.codeSettings(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for DELETE, got %d", rec.Code)
	}
}

func TestRandomTokenHasPrefixAndLength(t *testing.T) {
	tok, err := randomToken("scos_live_")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(tok, "scos_live_") {
		t.Fatalf("expected prefix scos_live_, got %q", tok)
	}
	if len(tok) < 40 {
		t.Fatalf("expected a long random token, got length %d", len(tok))
	}
}
