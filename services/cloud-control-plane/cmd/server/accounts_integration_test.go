//go:build integration

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestAccountFlowsThroughRestrictedRole drives signup, session auth, login,
// usage and team creation against the real schema as the runtime role
// (SCOS_APP_TEST_DATABASE_URL).
func TestAccountFlowsThroughRestrictedRole(t *testing.T) {
	appURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if appURL == "" {
		t.Fatal("SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	db, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db}
	email := "it-" + time.Now().UTC().Format("150405.000000") + "@example.test"

	call := func(handler http.HandlerFunc, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res := httptest.NewRecorder()
		handler(res, req)
		return res
	}
	sessionCookie := func(res *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range res.Result().Cookies() {
			if c.Name == "scos_session" && c.Value != "" {
				return c
			}
		}
		t.Fatalf("no session cookie in %d %s", res.Code, res.Body.String())
		return nil
	}

	res := call(s.signup, http.MethodPost, "/api/auth/signup", `{"email":"`+email+`","password":"correct horse battery","display_name":"IT User"}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", res.Code, res.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &created)
	if created["tenant_id"] == "" || created["principal_id"] == "" {
		t.Fatalf("signup returned no identity: %v", created)
	}
	cookie := sessionCookie(res)

	res = call(s.me, http.MethodGet, "/api/me", "", cookie)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), email) {
		t.Fatalf("session auth: %d %s", res.Code, res.Body.String())
	}

	if res = call(s.login, http.MethodPost, "/api/auth/login", `{"email":"`+email+`","password":"wrong"}`, nil); res.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password accepted: %d", res.Code)
	}
	res = call(s.login, http.MethodPost, "/api/auth/login", `{"email":"`+strings.ToUpper(email)+`","password":"correct horse battery"}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("login: %d %s", res.Code, res.Body.String())
	}
	var loggedIn map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &loggedIn)
	if loggedIn["tenant_id"] != created["tenant_id"] || loggedIn["principal_id"] != created["principal_id"] {
		t.Fatalf("login resolved a different identity: %v vs %v", loggedIn, created)
	}

	res = call(s.usage, http.MethodGet, "/api/usage", "", cookie)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"bytes_stored":0`) {
		t.Fatalf("usage: %d %s", res.Code, res.Body.String())
	}

	res = call(s.createTeam, http.MethodPost, "/api/teams/create", `{"name":"IT Team"}`, cookie)
	if res.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", res.Code, res.Body.String())
	}
	res = call(s.teams, http.MethodGet, "/api/teams", "", cookie)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "IT Team") {
		t.Fatalf("list teams: %d %s", res.Code, res.Body.String())
	}

	// The personal workspace stays the session identity after joining a team.
	res = call(s.me, http.MethodGet, "/api/me", "", cookie)
	var me map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &me)
	if me["tenant_id"] != created["tenant_id"] {
		t.Fatalf("session identity changed after team creation: %v", me)
	}
}
