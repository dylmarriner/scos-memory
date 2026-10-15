package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"

import "github.com/dylmarriner/scos-memory/packages/release"

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/tlsserve"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

type server struct {
	db                                                                                                                                    *sql.DB
	issuerSecret, gatewayURL, publicGatewayURL, vaultURL, memIntelligenceURL, codeIntelligenceURL, okfURL, graphifyURL, controlPlaneToken string
	client                                                                                                                                *http.Client
}
type credentials struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}
type identity struct{ UserID, TenantID, PrincipalID, DisplayName, Email, PlatformRole string }

type planDefinition struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Price string   `json:"price"`
	Perks []string `json:"perks"`
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if err := database.RequireTLS(dsn); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	database.TuneSQLDB(db)
	defer db.Close()
	client, err := tlsserve.TrustedClient(30 * time.Second)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		db:                  db,
		issuerSecret:        strings.TrimSpace(os.Getenv("SCOS_SECURITY_ISSUER_SECRET")),
		gatewayURL:          strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_MCP_GATEWAY_URL")), "/"),
		publicGatewayURL:    strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_PUBLIC_GATEWAY_URL")), "/"),
		vaultURL:            strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_CREDENTIAL_VAULT_URL")), "/"),
		memIntelligenceURL:  strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_MEMORY_INTELLIGENCE_URL")), "/"),
		codeIntelligenceURL: strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_CODE_INTELLIGENCE_URL")), "/"),
		okfURL:              strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_OKF_SYNCHRONISER_URL")), "/"),
		graphifyURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_GRAPHIFY_PROVIDER_URL")), "/"),
		controlPlaneToken:   strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")),
		client:              client,
	}
	if s.gatewayURL == "" {
		s.gatewayURL = "https://mcp-gateway:8098"
	}
	if s.publicGatewayURL == "" {
		// Best-effort fallback for a deployment that never set this --
		// correct for an all-internal test setup, wrong (docker-internal
		// DNS name, unreachable from a user's own machine) for any real
		// hosted deployment. Set SCOS_PUBLIC_GATEWAY_URL explicitly there.
		s.publicGatewayURL = s.gatewayURL + "/mcp"
		log.Printf("cloud-control-plane: SCOS_PUBLIC_GATEWAY_URL not set, falling back to %s -- this is almost certainly wrong for a real deployment reachable from outside the docker network", s.publicGatewayURL)
	}
	if s.vaultURL == "" {
		s.vaultURL = "https://credential-vault:8100"
	}
	if s.memIntelligenceURL == "" {
		s.memIntelligenceURL = "https://memory-intelligence:8089"
	}
	if s.codeIntelligenceURL == "" {
		s.codeIntelligenceURL = "https://code-intelligence:8084"
	}
	if s.okfURL == "" {
		s.okfURL = "https://okf-synchroniser:8090"
	}
	if s.graphifyURL == "" {
		s.graphifyURL = "https://graphify-provider:8099"
	}
	if s.issuerSecret == "" {
		log.Fatal("SCOS_SECURITY_ISSUER_SECRET is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", health)
	mux.HandleFunc("/health/version", release.Handler("cloud-control-plane"))
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			http.Error(w, "unready", 503)
			return
		}
		health(w, r)
	})
	mux.HandleFunc("/api/auth/signup", s.signup)
	mux.HandleFunc("/api/auth/login", s.login)
	mux.HandleFunc("/api/auth/logout", s.logout)
	mux.HandleFunc("/api/me", s.me)
	mux.HandleFunc("/api/keys", s.keys)
	mux.HandleFunc("/api/keys/revoke", s.revokeKey)
	mux.HandleFunc("/api/cli/provisioning-token", s.cliProvisioningToken)
	mux.HandleFunc("/api/cli/exchange", s.cliExchange)
	mux.HandleFunc("/api/usage", s.usage)
	mux.HandleFunc("/api/billing", s.billing)
	mux.HandleFunc("/api/plans", s.plans)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("/api/admin/overview", s.adminOverview)
	mux.HandleFunc("/api/admin/users", s.adminUsers)
	mux.HandleFunc("/api/admin/set-plan", s.adminSetPlan)
	mux.HandleFunc("/api/admin/set-role", s.adminSetRole)
	mux.HandleFunc("/api/admin/plans", s.adminPlanUpdate)
	mux.HandleFunc("/api/admin/audit", s.auditLog)
	mux.HandleFunc("/api/admin/tenant-usage", s.adminTenantUsage)
	mux.HandleFunc("/mcp", s.mcp)
	mux.HandleFunc("/api/vault", s.vault)
	mux.HandleFunc("/api/vault/", s.vault)
	mux.HandleFunc("/api/memory/graph", s.memoryGraph)
	mux.HandleFunc("/api/memory/facts", s.memoryFacts)
	mux.HandleFunc("/api/code/repos", s.codeRepos)
	mux.HandleFunc("/api/code/index-status", s.codeIndexStatus)
	mux.HandleFunc("/api/code/settings", s.codeSettings)
	mux.HandleFunc("/api/okf/documents", s.okfDocuments)
	mux.HandleFunc("/api/graphify/query", s.graphifyQuery)
	mux.HandleFunc("/api/graphify/explain", s.graphifyExplain)
	mux.HandleFunc("/api/teams", s.teams)
	mux.HandleFunc("/api/teams/create", s.createTeam)
	mux.HandleFunc("/api/teams/invite", s.inviteTeam)
	mux.HandleFunc("/api/teams/accept", s.acceptInvite)
	mux.HandleFunc("/api/teams/members", s.teamMembers)
	addr := os.Getenv("SCOS_CLOUD_HTTP_ADDR")
	if addr == "" {
		addr = ":8079"
	}
	log.Printf("cloud-control-plane listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("cloud-control-plane", mux)); err != nil {
		log.Fatal(err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}
func jsonWrite(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("request body required")
	}
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
func randomToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
func hash(v string) string     { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func validEmail(v string) bool { return strings.Contains(v, "@") && len(v) <= 320 }

// decodePerks tolerates a nil/empty column so callers never have to nil-check
// before ranging over the result.
func decodePerks(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	_ = json.Unmarshal(b, &out)
	return out
}

// paginationParams centralizes limit/offset parsing so every listing
// endpoint enforces the same bounds instead of trusting raw query params.
func paginationParams(v url.Values) (limit, offset int) {
	limit = 50
	if n, err := strconv.Atoi(v.Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	if n, err := strconv.Atoi(v.Get("page")); err == nil && n > 1 {
		offset = (n - 1) * limit
	}
	return
}

// userSearchFilter builds the WHERE clause and positional args for the admin
// user listing. Kept pure (no DB access) so the query-building logic can be
// unit tested without a live database.
func userSearchFilter(q, plan, role string) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if q = strings.TrimSpace(q); q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		clauses = append(clauses, "(lower(u.email) LIKE $"+strconv.Itoa(len(args))+" OR lower(u.display_name) LIKE $"+strconv.Itoa(len(args))+")")
	}
	if plan = strings.TrimSpace(plan); plan != "" {
		args = append(args, plan)
		clauses = append(clauses, "cs.plan = $"+strconv.Itoa(len(args)))
	}
	if role = strings.TrimSpace(role); role != "" {
		args = append(args, role)
		clauses = append(clauses, "u.platform_role = $"+strconv.Itoa(len(args)))
	}
	return strings.Join(clauses, " AND "), args
}

// validPerks rejects the empty/oversized inputs a creator-console form could
// submit; the DB column has no such constraint so the API is the only gate.
func validPerks(perks []string) error {
	if len(perks) == 0 {
		return errors.New("at least one perk is required")
	}
	if len(perks) > 20 {
		return errors.New("no more than 20 perks are allowed")
	}
	for _, p := range perks {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 160 {
			return errors.New("each perk must be 1-160 characters")
		}
	}
	return nil
}

type planUpdateInput struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Price string   `json:"price"`
	Perks []string `json:"perks"`
}

func validPlanUpdate(in planUpdateInput) error {
	if strings.TrimSpace(in.ID) == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 60 {
		return errors.New("name must be 1-60 characters")
	}
	if strings.TrimSpace(in.Price) == "" || len(in.Price) > 40 {
		return errors.New("price must be 1-40 characters")
	}
	return validPerks(in.Perks)
}

func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in credentials
	if decode(r, &in) != nil || !validEmail(in.Email) || len(in.Password) < 12 || len(in.DisplayName) < 1 {
		http.Error(w, "email, display_name and a 12-character password are required", 400)
		return
	}
	pw, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "password setup failed", 500)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback()
	var uid, tid, pid, wid, nid string
	err = tx.QueryRowContext(r.Context(), `INSERT INTO cloud_users(email,password_hash,display_name) VALUES($1,$2,$3) RETURNING id`, strings.ToLower(strings.TrimSpace(in.Email)), string(pw), strings.TrimSpace(in.DisplayName)).Scan(&uid)
	if err != nil && strings.Contains(err.Error(), "unique") {
		http.Error(w, "an account with this email already exists", 409)
		return
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO tenants(name) VALUES($1) RETURNING id`, strings.TrimSpace(in.DisplayName)+"'s workspace").Scan(&tid)
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$3) RETURNING id`, tid, uid, in.DisplayName).Scan(&pid)
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO workspaces(tenant_id,name) VALUES($1,$2) RETURNING id`, tid, "Memory workspace").Scan(&wid)
	}
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO namespaces(tenant_id,owner_principal_id,workspace_id,canonical_name) VALUES($1,$2,$3,'default') RETURNING id`, tid, pid, wid).Scan(&nid)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates,can_promote,can_administer) VALUES($1,$2,true,true,true,true)`, nid, pid)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO cloud_subscriptions(tenant_id,plan,status) VALUES($1,'free','active')`, tid)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, "account creation failed", 500)
		return
	}
	s.issueSession(w, r, identity{UserID: uid, TenantID: tid, PrincipalID: pid, DisplayName: in.DisplayName, Email: in.Email, PlatformRole: "user"})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in credentials
	if decode(r, &in) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	var id identity
	var pw, status string
	err := s.db.QueryRowContext(r.Context(), `SELECT u.id,u.password_hash,u.display_name,u.email,u.status,u.platform_role,p.id,p.tenant_id FROM cloud_users u JOIN principals p ON p.external_id=u.id::text JOIN tenants t ON t.id=p.tenant_id WHERE lower(u.email)=lower($1) LIMIT 1`, in.Email).Scan(&id.UserID, &pw, &id.DisplayName, &id.Email, &status, &id.PlatformRole, &id.PrincipalID, &id.TenantID)
	if err != nil || status != "active" || bcrypt.CompareHashAndPassword([]byte(pw), []byte(in.Password)) != nil {
		http.Error(w, "invalid email or password", 401)
		return
	}
	s.issueSession(w, r, id)
}

func (s *server) issueSession(w http.ResponseWriter, r *http.Request, id identity) {
	tok, err := randomToken("scs_")
	if err != nil {
		http.Error(w, "session unavailable", 500)
		return
	}
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO cloud_sessions(user_id,token_hash,expires_at) VALUES($1,$2,now()+interval '30 days')`, id.UserID, hash(tok))
	if err != nil {
		http.Error(w, "session unavailable", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "scos_session", Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 2592000})
	jsonWrite(w, 200, map[string]any{"user_id": id.UserID, "email": id.Email, "display_name": id.DisplayName, "platform_role": id.PlatformRole, "tenant_id": id.TenantID, "principal_id": id.PrincipalID})
}
func (s *server) current(r *http.Request) (identity, error) {
	c, err := r.Cookie("scos_session")
	if err != nil {
		return identity{}, err
	}
	var id identity
	err = s.db.QueryRowContext(r.Context(), `SELECT u.id,u.email,u.display_name,u.platform_role,p.tenant_id,p.id FROM cloud_sessions cs JOIN cloud_users u ON u.id=cs.user_id JOIN principals p ON p.external_id=u.id::text WHERE cs.token_hash=$1 AND cs.revoked_at IS NULL AND cs.expires_at>now() AND u.status='active' LIMIT 1`, hash(c.Value)).Scan(&id.UserID, &id.Email, &id.DisplayName, &id.PlatformRole, &id.TenantID, &id.PrincipalID)
	if err == nil {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE cloud_sessions SET last_seen_at=now() WHERE token_hash=$1`, hash(c.Value))
	}
	return id, err
}
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("scos_session"); e == nil {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE cloud_sessions SET revoked_at=now() WHERE token_hash=$1`, hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "scos_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true})
	jsonWrite(w, 200, map[string]bool{"ok": true})
}
func (s *server) me(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	jsonWrite(w, 200, id)
}

func (s *server) keys(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.QueryContext(r.Context(), `SELECT id,name,prefix,scopes,created_at,last_used_at FROM cloud_api_keys WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC`, id.UserID)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer rows.Close()
		out := []any{}
		for rows.Next() {
			var kid, name, prefix string
			var scopes []byte
			var created time.Time
			var last sql.NullTime
			if rows.Scan(&kid, &name, &prefix, &scopes, &created, &last) == nil {
				out = append(out, map[string]any{"id": kid, "name": name, "prefix": prefix, "scopes": json.RawMessage(scopes), "created_at": created, "last_used_at": last.Time})
			}
		}
		jsonWrite(w, 200, out)
	case http.MethodPost:
		var in struct {
			Name   string `json:"name"`
			TeamID string `json:"team_id"`
		}
		if decode(r, &in) != nil || strings.TrimSpace(in.Name) == "" {
			http.Error(w, "name is required", 400)
			return
		}
		keyTenant, keyPrincipal := id.TenantID, id.PrincipalID
		if strings.TrimSpace(in.TeamID) != "" {
			e := s.db.QueryRowContext(r.Context(), `SELECT t.tenant_id,m.principal_id FROM cloud_teams t JOIN cloud_team_members m ON m.team_id=t.id WHERE t.id=$1 AND m.user_id=$2`, strings.TrimSpace(in.TeamID), id.UserID).Scan(&keyTenant, &keyPrincipal)
			if e != nil {
				http.Error(w, "team membership required", 403)
				return
			}
		}
		outID, key, e := issueAPIKey(r.Context(), s.db, id.UserID, keyTenant, keyPrincipal, strings.TrimSpace(in.Name))
		if e != nil {
			http.Error(w, "key creation failed", 500)
			return
		}
		jsonWrite(w, 201, map[string]string{"id": outID, "name": in.Name, "key": key, "warning": "Copy this key now. It will not be shown again."})
	default:
		http.Error(w, "method not allowed", 405)
	}
}
func (s *server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if decode(r, &in) != nil || in.ID == "" {
		http.Error(w, "id is required", 400)
		return
	}
	res, e := s.db.ExecContext(r.Context(), `UPDATE cloud_api_keys SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, in.ID, id.UserID)
	if e != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "key not found", 404)
		return
	}
	jsonWrite(w, 200, map[string]bool{"ok": true})
}

// queryRowContexter is satisfied by both *sql.DB and *sql.Tx, so
// issueAPIKey can run standalone (keys()) or as part of a larger
// transaction (cliExchange(), which must consume the provisioning token and
// mint the key atomically).
type queryRowContexter interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// issueAPIKey mints a long-lived scos_live_... bearer token for the given
// tenant/principal and records it against userID for later listing/revoke.
// Shared by keys() (POST /api/keys) and cliExchange() (POST
// /api/cli/exchange) so both surfaces issue keys the same way.
func issueAPIKey(ctx context.Context, q queryRowContexter, userID, tenantID, principalID, name string) (id, key string, err error) {
	key, err = randomToken("scos_live_")
	if err != nil {
		return "", "", err
	}
	err = q.QueryRowContext(ctx, `INSERT INTO cloud_api_keys(user_id,tenant_id,principal_id,name,prefix,key_hash) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, userID, tenantID, principalID, name, key[:18], hash(key)).Scan(&id)
	if err != nil {
		return "", "", err
	}
	return id, key, nil
}

// cliProvisioningToken mints a short-lived, single-use token for the
// dashboard's "Connect this machine" one-liner. Session-cookie
// authenticated, like the rest of the account surface -- the token it
// returns is what a local install script exchanges (cliExchange, below) for
// a real API key, so it's safe to display/copy without becoming a standing
// credential.
func (s *server) cliProvisioningToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	tok, err := randomToken("scos_prov_")
	if err != nil {
		http.Error(w, "token unavailable", 500)
		return
	}
	_, err = s.db.ExecContext(r.Context(), `INSERT INTO cloud_provisioning_tokens(user_id,tenant_id,principal_id,token_hash,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, id.UserID, id.TenantID, id.PrincipalID, hash(tok))
	if err != nil {
		http.Error(w, "token unavailable", 500)
		return
	}
	jsonWrite(w, 201, map[string]any{"token": tok, "expires_in_seconds": 600})
}

// cliExchange consumes a single-use provisioning token (no session cookie --
// the token itself is the authentication) and mints a real API key for it,
// atomically with marking the token used so a token can never be redeemed
// twice even under concurrent requests (SELECT ... FOR UPDATE holds the row
// lock for the transaction's duration).
func (s *server) cliExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if decode(r, &in) != nil || strings.TrimSpace(in.Token) == "" {
		http.Error(w, "token is required", 400)
		return
	}
	tokenHash := hash(strings.TrimSpace(in.Token))

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback()

	var userID, tenantID, principalID string
	err = tx.QueryRowContext(r.Context(), `SELECT user_id,tenant_id,principal_id FROM cloud_provisioning_tokens WHERE token_hash=$1 AND used_at IS NULL AND expires_at>now() FOR UPDATE`, tokenHash).Scan(&userID, &tenantID, &principalID)
	if err != nil {
		http.Error(w, "token is invalid, expired, or already used", 401)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE cloud_provisioning_tokens SET used_at=now() WHERE token_hash=$1`, tokenHash); err != nil {
		http.Error(w, "token unavailable", 500)
		return
	}
	_, key, err := issueAPIKey(r.Context(), tx, userID, tenantID, principalID, "cli-install")
	if err != nil {
		http.Error(w, "key creation failed", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "key creation failed", 500)
		return
	}
	jsonWrite(w, 200, map[string]string{
		"gateway_url":  s.publicGatewayURL,
		"bearer_token": key,
		"tenant_id":    tenantID,
		"principal_id": principalID,
	})
}

func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	var requests, reads, writes int64
	err = s.db.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(api_requests),0),COALESCE(SUM(memory_reads),0),COALESCE(SUM(memory_writes),0) FROM cloud_usage_daily WHERE tenant_id=$1`, id.TenantID).Scan(&requests, &reads, &writes)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	// Storage is a stock: measure it now rather than summing daily rows.
	bytesStored, err := s.measureBytesStored(r.Context(), id.TenantID, id.PrincipalID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	jsonWrite(w, 200, map[string]any{"api_requests": requests, "memory_reads": reads, "memory_writes": writes, "bytes_stored": bytesStored})
}

func (s *server) billing(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	var plan, status string
	err = s.db.QueryRowContext(r.Context(), `SELECT plan,status FROM cloud_subscriptions WHERE tenant_id=$1`, id.TenantID).Scan(&plan, &status)
	if err != nil {
		http.Error(w, "billing unavailable", 503)
		return
	}
	jsonWrite(w, 200, map[string]string{"plan": plan, "status": status})
}

// plans is public: pricing copy is read by anonymous visitors on the portal
// as well as by the logged-in dashboard.
func (s *server) plans(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT id,name,price,perks FROM cloud_plan_definitions ORDER BY sort_order`)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []planDefinition{}
	for rows.Next() {
		var p planDefinition
		var perks []byte
		if rows.Scan(&p.ID, &p.Name, &p.Price, &perks) == nil {
			p.Perks = decodePerks(perks)
			out = append(out, p)
		}
	}
	jsonWrite(w, 200, out)
}

func (s *server) validPlanID(ctx context.Context, id string) bool {
	var exists bool
	_ = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cloud_plan_definitions WHERE id=$1)`, id).Scan(&exists)
	return exists
}

// recordAudit is best-effort: a failed audit write must never block the
// admin action it is describing, so errors are swallowed here.
func (s *server) recordAudit(ctx context.Context, actorUserID, action, target string, details any) {
	b, _ := json.Marshal(details)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO cloud_admin_audit_log(actor_user_id,action,target,details) VALUES($1,$2,$3,$4)`, actorUserID, action, target, b)
}

func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	if r.Method == http.MethodGet {
		jsonWrite(w, 200, id)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if decode(r, &in) != nil || strings.TrimSpace(in.DisplayName) == "" || len(in.DisplayName) > 120 {
		http.Error(w, "display_name is required", 400)
		return
	}
	_, err = s.db.ExecContext(r.Context(), `UPDATE cloud_users SET display_name=$1,updated_at=now() WHERE id=$2`, strings.TrimSpace(in.DisplayName), id.UserID)
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	id.DisplayName = strings.TrimSpace(in.DisplayName)
	jsonWrite(w, 200, id)
}

func (s *server) creator(r *http.Request) (identity, bool) {
	id, err := s.current(r)
	return id, err == nil && id.PlatformRole == "creator"
}

func (s *server) adminOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.creator(r); !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	var users, tenants, keys int
	if err := s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM cloud_users`).Scan(&users); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	_ = s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM tenants`).Scan(&tenants)
	_ = s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM cloud_api_keys WHERE revoked_at IS NULL`).Scan(&keys)
	jsonWrite(w, 200, map[string]int{"users": users, "tenants": tenants, "active_api_keys": keys})
}

func (s *server) adminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.creator(r); !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	where, args := userSearchFilter(r.URL.Query().Get("q"), r.URL.Query().Get("plan"), r.URL.Query().Get("role"))
	limit, offset := paginationParams(r.URL.Query())
	args = append(args, limit, offset)
	query := `SELECT u.id,u.email,u.display_name,u.platform_role,u.status,cs.plan,p.tenant_id FROM cloud_users u JOIN principals p ON p.external_id=u.id::text JOIN cloud_subscriptions cs ON cs.tenant_id=p.tenant_id WHERE ` + where +
		` ORDER BY u.created_at DESC LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, email, name, role, status, plan, tenantID string
		if rows.Scan(&id, &email, &name, &role, &status, &plan, &tenantID) == nil {
			out = append(out, map[string]string{"id": id, "email": email, "display_name": name, "platform_role": role, "status": status, "plan": plan, "tenant_id": tenantID})
		}
	}
	jsonWrite(w, 200, out)
}

func (s *server) adminSetPlan(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.creator(r)
	if !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	var in struct {
		TenantID string `json:"tenant_id"`
		Plan     string `json:"plan"`
	}
	if decode(r, &in) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if in.TenantID == "" || !s.validPlanID(r.Context(), in.Plan) {
		http.Error(w, "valid tenant_id and plan are required", 400)
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE cloud_subscriptions SET plan=$1,updated_at=now() WHERE tenant_id=$2`, in.Plan, in.TenantID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "tenant subscription not found", 404)
		return
	}
	s.recordAudit(r.Context(), actor.UserID, "subscription.plan_changed", in.TenantID, map[string]string{"plan": in.Plan})
	jsonWrite(w, 200, map[string]any{"ok": true, "tenant_id": in.TenantID, "plan": in.Plan})
}

func (s *server) adminSetRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.creator(r)
	if !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	var in struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if decode(r, &in) != nil || (in.Role != "user" && in.Role != "creator") || in.UserID == "" {
		http.Error(w, "valid user_id and role are required", 400)
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE cloud_users SET platform_role=$1,updated_at=now() WHERE id=$2`, in.Role, in.UserID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "user not found", 404)
		return
	}
	s.recordAudit(r.Context(), actor.UserID, "user.role_changed", in.UserID, map[string]string{"role": in.Role})
	jsonWrite(w, 200, map[string]any{"ok": true, "user_id": in.UserID, "role": in.Role})
}

// adminPlanUpdate is the creator-only settings surface for editing tier
// pricing copy and perk lists without a code deploy.
func (s *server) adminPlanUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.creator(r)
	if !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in planUpdateInput
	if decode(r, &in) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if err := validPlanUpdate(in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	perksJSON, err := json.Marshal(in.Perks)
	if err != nil {
		http.Error(w, "invalid perks", 400)
		return
	}
	var before planDefinition
	var beforePerks []byte
	_ = s.db.QueryRowContext(r.Context(), `SELECT name,price,perks FROM cloud_plan_definitions WHERE id=$1`, in.ID).Scan(&before.Name, &before.Price, &beforePerks)
	before.ID, before.Perks = in.ID, decodePerks(beforePerks)
	res, err := s.db.ExecContext(r.Context(), `UPDATE cloud_plan_definitions SET name=$1,price=$2,perks=$3,updated_at=now() WHERE id=$4`, strings.TrimSpace(in.Name), strings.TrimSpace(in.Price), perksJSON, in.ID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "plan not found", 404)
		return
	}
	after := planDefinition{ID: in.ID, Name: strings.TrimSpace(in.Name), Price: strings.TrimSpace(in.Price), Perks: in.Perks}
	s.recordAudit(r.Context(), actor.UserID, "plan.updated", in.ID, map[string]any{"before": before, "after": after})
	jsonWrite(w, 200, map[string]any{"ok": true, "id": in.ID})
}

func (s *server) auditLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.creator(r); !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	limit, offset := paginationParams(r.URL.Query())
	rows, err := s.db.QueryContext(r.Context(), `SELECT a.id,a.action,a.target,a.details,a.created_at,u.email FROM cloud_admin_audit_log a JOIN cloud_users u ON u.id=a.actor_user_id ORDER BY a.created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, action, target, actorEmail string
		var details []byte
		var created time.Time
		if rows.Scan(&id, &action, &target, &details, &created, &actorEmail) == nil {
			out = append(out, map[string]any{"id": id, "action": action, "target": target, "details": json.RawMessage(details), "created_at": created, "actor_email": actorEmail})
		}
	}
	jsonWrite(w, 200, out)
}

func (s *server) adminTenantUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.creator(r); !ok {
		http.Error(w, "creator access required", 403)
		return
	}
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenantID == "" {
		http.Error(w, "tenant_id is required", 400)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT usage_date,api_requests,memory_reads,memory_writes,bytes_stored FROM cloud_usage_daily WHERE tenant_id=$1 AND usage_date >= current_date - interval '30 days' ORDER BY usage_date`, tenantID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var day time.Time
		var requests, reads, writes, bytesStored int64
		if rows.Scan(&day, &requests, &reads, &writes, &bytesStored) == nil {
			out = append(out, map[string]any{"date": day.Format("2006-01-02"), "api_requests": requests, "memory_reads": reads, "memory_writes": writes, "bytes_stored": bytesStored})
		}
	}
	jsonWrite(w, 200, out)
}

func (s *server) mcp(w http.ResponseWriter, r *http.Request) {
	h := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if h == "" || h == r.Header.Get("Authorization") {
		http.Error(w, "API key required", 401)
		return
	}
	var id identity
	err := s.db.QueryRowContext(r.Context(), `SELECT user_id,tenant_id,principal_id FROM cloud_api_keys WHERE key_hash=$1 AND revoked_at IS NULL`, hash(h)).Scan(&id.UserID, &id.TenantID, &id.PrincipalID)
	if err != nil {
		http.Error(w, "invalid API key", 401)
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `UPDATE cloud_api_keys SET last_used_at=now() WHERE key_hash=$1`, hash(h))
	_, _ = s.db.ExecContext(r.Context(), `INSERT INTO cloud_usage_daily(tenant_id,usage_date,api_requests) VALUES($1,current_date,1) ON CONFLICT (tenant_id,usage_date) DO UPDATE SET api_requests=cloud_usage_daily.api_requests+1`, id.TenantID)
	claims := auth.Claims{TenantID: id.TenantID, PrincipalID: id.PrincipalID, Subject: id.UserID, JTI: hash(h)[:32], Scopes: []string{"memory:read", "memory:write"}, ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	token, err := auth.SignClaims(claims, s.issuerSecret)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	u, err := url.Parse(s.gatewayURL + "/mcp")
	if err != nil {
		http.Error(w, "gateway unavailable", 503)
		return
	}
	// Keep a bounded copy of the request for usage metering while the full
	// body is still streamed to the gateway.
	var requestCopy bytes.Buffer
	var body io.Reader
	if r.Body != nil {
		body = io.TeeReader(r.Body, &limitedBuffer{buf: &requestCopy, limit: maxMeteredBody})
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		http.Error(w, "gateway unavailable", 503)
		return
	}
	req.Header = r.Header.Clone()
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "gateway unavailable", 503)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	var responseCopy bytes.Buffer
	_, _ = io.Copy(flushWriter{w}, io.TeeReader(resp.Body, &limitedBuffer{buf: &responseCopy, limit: maxMeteredBody}))
	if resp.StatusCode/100 == 2 {
		reads, writes := countMemoryOps(requestCopy.Bytes(), responseCopy.Bytes(), resp.Header.Get("Content-Type"))
		if err := s.recordMemoryOps(context.WithoutCancel(r.Context()), id.TenantID, reads, writes); err != nil {
			log.Printf("record memory usage for tenant %s: %v", id.TenantID, err)
		}
	}
}

// limitedBuffer records at most limit bytes and silently discards the rest,
// so metering never holds an unbounded body in memory.
type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.limit - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// flushWriter flushes after each write so streamed (SSE) MCP responses reach
// the client as they are produced.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
