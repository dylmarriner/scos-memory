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
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestHostedMCPEnforcesPlanEntitlements drives the hosted /mcp proxy as the
// runtime role (SCOS_APP_TEST_DATABASE_URL) in front of a stand-in gateway
// that authenticates the forwarded claims token: a free-plan key is refused
// graphify, is refused once its monthly request quota is used, and never
// carries operator scopes; the same key carries backups:admin and
// recovery:admin once its account holds the creator platform role. The
// owner connection (DATABASE_URL) only seeds usage and the role change.
func TestHostedMCPEnforcesPlanEntitlements(t *testing.T) {
	appURL, adminURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if appURL == "" || adminURL == "" {
		t.Fatal("SCOS_APP_TEST_DATABASE_URL (runtime role) and DATABASE_URL (owner) must be set")
	}
	db, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	const secret = "entitlements-it-issuer-secret"
	var mu sync.Mutex
	var gatewayCalls int
	var lastScopes []string
	gateway := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.CanonicalClaims(r.Context())
		if !ok {
			http.Error(w, "no claims", 401)
			return
		}
		mu.Lock()
		gatewayCalls++
		lastScopes = claims.Scopes
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}), auth.Config{Token: "unused-static", Identity: "gateway", IssuerSecret: secret}))
	defer gateway.Close()

	s := &server{db: db, issuerSecret: secret, gatewayURL: gateway.URL, client: gateway.Client()}
	email := "ent-it-" + time.Now().UTC().Format("150405.000000") + "@example.test"
	call := func(handler http.HandlerFunc, method, path, body string, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res := httptest.NewRecorder()
		handler(res, req)
		return res
	}
	res := call(s.signup, http.MethodPost, "/api/auth/signup", `{"email":"`+email+`","password":"correct horse battery","display_name":"Entitlements IT"}`, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", res.Code, res.Body.String())
	}
	var created struct {
		UserID   string `json:"user_id"`
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &created)
	var cookie *http.Cookie
	for _, c := range res.Result().Cookies() {
		if c.Name == "scos_session" {
			cookie = c
		}
	}
	res = call(s.keys, http.MethodPost, "/api/keys", `{"name":"it"}`, cookie, "")
	if res.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", res.Code, res.Body.String())
	}
	var key struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &key)
	setUsage := func(n int) {
		t.Helper()
		if _, err := admin.Exec(`INSERT INTO cloud_usage_daily(tenant_id,usage_date,api_requests) VALUES($1,current_date,$2)
			ON CONFLICT (tenant_id,usage_date) DO UPDATE SET api_requests=EXCLUDED.api_requests`, created.TenantID, n); err != nil {
			t.Fatal(err)
		}
	}
	mcp := func(tool string) *httptest.ResponseRecorder {
		return call(s.mcp, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`, nil, key.Key)
	}

	// A free-plan key reaches the gateway for an ungated tool, without
	// operator scopes, and the request is counted.
	if res = mcp("scos_recall"); res.Code != http.StatusOK {
		t.Fatalf("free plan recall: %d %s", res.Code, res.Body.String())
	}
	for _, s := range lastScopes {
		if s == "backups:admin" || s == "recovery:admin" || s == "extensions:admin" {
			t.Fatalf("customer key carried operator scope %s", s)
		}
	}
	var used int64
	if err := admin.QueryRow(`SELECT api_requests FROM cloud_usage_daily WHERE tenant_id=$1 AND usage_date=current_date`, created.TenantID).Scan(&used); err != nil || used != 1 {
		t.Fatalf("request not counted: %d %v", used, err)
	}

	// graphify is not in the free plan: refused before the gateway.
	before := gatewayCalls
	if res = mcp("scos_graphify_query"); res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "feature_not_in_plan") {
		t.Fatalf("free plan graphify: %d %s", res.Code, res.Body.String())
	}
	if gatewayCalls != before {
		t.Fatal("a refused request reached the gateway")
	}
	res = call(s.graphifyQuery, http.MethodPost, "/api/graphify/query", `{"question":"x"}`, cookie, "")
	if res.Code != http.StatusForbidden {
		t.Fatalf("dashboard graphify on the free plan: %d %s", res.Code, res.Body.String())
	}

	// The monthly quota (10,000 on free) is enforced and the refusal is not
	// counted.
	setUsage(10000)
	if res = mcp("scos_recall"); res.Code != http.StatusTooManyRequests || res.Header().Get("Retry-After") == "" {
		t.Fatalf("request over quota: %d %s", res.Code, res.Body.String())
	}
	if err := admin.QueryRow(`SELECT api_requests FROM cloud_usage_daily WHERE tenant_id=$1 AND usage_date=current_date`, created.TenantID).Scan(&used); err != nil || used != 10000 {
		t.Fatalf("refused request was counted: %d %v", used, err)
	}

	// An account holding the creator platform role carries the operator
	// scopes the backup and recovery admin routes demand.
	setUsage(0)
	if _, err := admin.Exec(`UPDATE cloud_users SET platform_role='creator' WHERE id=$1`, created.UserID); err != nil {
		t.Fatal(err)
	}
	if res = mcp("scos_backup_plan"); res.Code != http.StatusOK {
		t.Fatalf("creator call: %d %s", res.Code, res.Body.String())
	}
	joined := strings.Join(lastScopes, ",")
	if !strings.Contains(joined, "backups:admin") || !strings.Contains(joined, "recovery:admin") {
		t.Fatalf("creator key scopes = %s", joined)
	}

	// A disabled account's key no longer works.
	if _, err := admin.Exec(`UPDATE cloud_users SET status='disabled' WHERE id=$1`, created.UserID); err != nil {
		t.Fatal(err)
	}
	if res = mcp("scos_recall"); res.Code != http.StatusUnauthorized {
		t.Fatalf("disabled account key: %d %s", res.Code, res.Body.String())
	}

	// The pricing page advertises exactly the enforced limits.
	res = call(s.plans, http.MethodGet, "/api/plans", "", nil, "")
	var plans []planDefinition
	if err := json.Unmarshal(res.Body.Bytes(), &plans); err != nil {
		t.Fatal(err)
	}
	byID := map[string]planDefinition{}
	for _, p := range plans {
		byID[p.ID] = p
	}
	free, pro := byID["free"], byID["pro"]
	if free.Limits.MonthlyMCPRequests == nil || *free.Limits.MonthlyMCPRequests != 10000 || free.Perks[0] != "10,000 MCP requests/month" || free.Perks[1] != "1 GB memory storage" {
		t.Fatalf("free plan = %+v", free)
	}
	if !strings.Contains(strings.Join(pro.Perks, "|"), "Graphify code knowledge graph") {
		t.Fatalf("pro plan = %+v", pro)
	}
	for _, p := range plans {
		for _, line := range p.Perks {
			if strings.Contains(strings.ToLower(line), "retention") || strings.Contains(strings.ToLower(line), "advanced retrieval") {
				t.Fatalf("plan %s still advertises an unenforced capability: %q", p.ID, line)
			}
		}
	}
}
