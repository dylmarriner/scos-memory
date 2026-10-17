package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Plan entitlements. A plan's quotas and gated features are columns of
// cloud_plan_definitions (migration 000058), and they are enforced here on
// the hosted MCP path and on the dashboard panels a feature gates. The
// pricing page renders its quota and feature lines from the same columns
// (planLines), so a plan never advertises a limit or a feature that this
// file does not enforce.

// maxProxiedBody bounds a hosted MCP request body.
const maxProxiedBody = 16 << 20

// featureGraphify admits the graphify tools and dashboard panels.
const featureGraphify = "graphify"

// knownFeatures are the features a plan may carry; each is enforced below.
var knownFeatures = map[string]string{
	featureGraphify: "Graphify code knowledge graph",
}

// featureTools maps each gated MCP tool to the feature that admits it.
var featureTools = map[string]string{
	"scos_graphify_query":         featureGraphify,
	"scos_graphify_path":          featureGraphify,
	"scos_graphify_explain":       featureGraphify,
	"scos_graphify_ingest":        featureGraphify,
	"scos_graphify_ingest_status": featureGraphify,
}

// storageGrowingTools add memory content (directly, or through the turn
// journal whose completed turns are extracted into memory). Once a tenant's
// stored memory reaches its plan's storage quota these are refused; reads
// and forgets stay available so a tenant can always get back under its
// quota.
var storageGrowingTools = map[string]bool{
	"scos_remember":          true,
	"scos_correct":           true,
	"scos_correct_batch":     true,
	"scos_link_evidence":     true,
	"scos_graphify_ingest":   true,
	"scos_mg_session_create": true,
	"scos_mg_message_append": true,
	"scos_mg_chunk_append":   true,
	"scos_mg_response_start": true,
	"scos_mg_turn_complete":  true,
}

// planEntitlements is what a tenant's subscription currently permits. A nil
// limit is unlimited.
type planEntitlements struct {
	Plan               string
	SubscriptionStatus string
	MonthlyMCPRequests *int64
	StorageBytes       *int64
	Features           []string
}

func (e planEntitlements) has(feature string) bool {
	for _, f := range e.Features {
		if f == feature {
			return true
		}
	}
	return false
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// errNoSubscription is returned when a tenant has no subscription row.
var errNoSubscription = errors.New("tenant has no subscription")

func loadEntitlements(ctx context.Context, q queryRower, tenantID string) (planEntitlements, error) {
	var e planEntitlements
	var requests, storage sql.NullInt64
	var features []byte
	err := q.QueryRowContext(ctx, `SELECT cs.plan, cs.status, p.monthly_mcp_requests, p.storage_bytes, p.features
		FROM cloud_subscriptions cs JOIN cloud_plan_definitions p ON p.id = cs.plan
		WHERE cs.tenant_id = $1`, tenantID).Scan(&e.Plan, &e.SubscriptionStatus, &requests, &storage, &features)
	if errors.Is(err, sql.ErrNoRows) {
		return e, errNoSubscription
	}
	if err != nil {
		return e, err
	}
	if requests.Valid {
		v := requests.Int64
		e.MonthlyMCPRequests = &v
	}
	if storage.Valid {
		v := storage.Int64
		e.StorageBytes = &v
	}
	if err := json.Unmarshal(features, &e.Features); err != nil {
		return e, fmt.Errorf("decode plan features: %w", err)
	}
	return e, nil
}

// planLines renders the quota and feature lines a plan advertises from the
// limits it enforces.
func planLines(requests, storage *int64, features []string) []string {
	var lines []string
	if requests == nil {
		lines = append(lines, "Unlimited MCP requests")
	} else {
		lines = append(lines, groupThousands(*requests)+" MCP requests/month")
	}
	if storage == nil {
		lines = append(lines, "Unlimited memory storage")
	} else {
		lines = append(lines, formatStorage(*storage)+" memory storage")
	}
	for _, f := range features {
		if label, ok := knownFeatures[f]; ok {
			lines = append(lines, label)
		}
	}
	return lines
}

func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatStorage renders a byte quota in decimal units (1 GB = 10^9 bytes),
// exact to one decimal place.
func formatStorage(n int64) string {
	units := []struct {
		size int64
		name string
	}{{1_000_000_000_000, "TB"}, {1_000_000_000, "GB"}, {1_000_000, "MB"}, {1_000, "KB"}}
	for _, u := range units {
		if n >= u.size {
			if n%u.size == 0 {
				return strconv.FormatInt(n/u.size, 10) + " " + u.name
			}
			return strconv.FormatFloat(float64(n)/float64(u.size), 'f', 1, 64) + " " + u.name
		}
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// quotaError is a refusal on plan grounds; status is the HTTP status.
type quotaError struct {
	status int
	code   string
	msg    string
}

func (e *quotaError) Error() string { return e.msg }

// checkToolEntitlements refuses a request that calls a tool the plan does not
// include, or that adds memory content once the storage quota is reached.
// measureStorage is only invoked when a storage-growing tool is called under
// a finite storage quota.
func checkToolEntitlements(ctx context.Context, e planEntitlements, tools []string, measureStorage func(context.Context) (int64, error)) error {
	growing := false
	for _, name := range tools {
		if feature, gated := featureTools[name]; gated && !e.has(feature) {
			return &quotaError{status: http.StatusForbidden, code: "feature_not_in_plan",
				msg: fmt.Sprintf("%s requires the %s feature, which the %s plan does not include", name, feature, e.Plan)}
		}
		if storageGrowingTools[name] {
			growing = true
		}
	}
	if !growing || e.StorageBytes == nil {
		return nil
	}
	stored, err := measureStorage(ctx)
	if err != nil {
		return fmt.Errorf("measure stored memory: %w", err)
	}
	if stored >= *e.StorageBytes {
		return &quotaError{status: http.StatusInsufficientStorage, code: "storage_quota_reached",
			msg: fmt.Sprintf("memory storage quota reached: %d of %d bytes stored on the %s plan; forget memories or upgrade to add more", stored, *e.StorageBytes, e.Plan)}
	}
	return nil
}

// calledTools lists the tool names of every tools/call in a JSON-RPC body
// (a single message or a batch).
func calledTools(body []byte) []string {
	var names []string
	for _, msg := range decodeRPC(body) {
		if msg.Method == "tools/call" {
			names = append(names, msg.Params.Name)
		}
	}
	return names
}

// firstRPCID is the id of the first message in body, for addressing a
// JSON-RPC error to the request it refuses.
func firstRPCID(body []byte) json.RawMessage {
	for _, msg := range decodeRPC(body) {
		if len(msg.ID) > 0 {
			return msg.ID
		}
	}
	return json.RawMessage("null")
}

// beginningOfNextMonth is when a monthly request quota resets.
func beginningOfNextMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// admitMCPRequest counts one hosted MCP request against the tenant's
// monthly quota. The check and the increment run under a per-tenant
// transaction-scoped advisory lock, so concurrent requests cannot both pass
// a quota with one request left. A refused request is not counted.
func (s *server) admitMCPRequest(ctx context.Context, tenantID string, limit *int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cloud-mcp-requests:' || $1, 0))`, tenantID); err != nil {
		return err
	}
	if limit != nil {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(api_requests),0) FROM cloud_usage_daily
			WHERE tenant_id=$1 AND usage_date >= date_trunc('month', current_date)::date`, tenantID).Scan(&used); err != nil {
			return err
		}
		if used >= *limit {
			return &quotaError{status: http.StatusTooManyRequests, code: "request_quota_reached",
				msg: fmt.Sprintf("monthly MCP request quota reached: %d of %d requests used this month", used, *limit)}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cloud_usage_daily(tenant_id,usage_date,api_requests) VALUES($1,current_date,1)
		ON CONFLICT (tenant_id,usage_date) DO UPDATE SET api_requests=cloud_usage_daily.api_requests+1`, tenantID); err != nil {
		return err
	}
	return tx.Commit()
}

// writeMCPRefusal answers a refused hosted MCP request with a JSON-RPC error
// addressed to the request, under the HTTP status that names the refusal.
func writeMCPRefusal(w http.ResponseWriter, body []byte, err *quotaError) {
	w.Header().Set("Content-Type", "application/json")
	if err.status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(beginningOfNextMonth(time.Now())).Seconds())+1))
	}
	w.WriteHeader(err.status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      firstRPCID(body),
		"error":   map[string]any{"code": -32003, "message": err.msg, "data": map[string]string{"reason": err.code}},
	})
}

// requireFeature gates a dashboard panel on the signed-in account's plan.
func (s *server) requireFeature(w http.ResponseWriter, r *http.Request, feature string) bool {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	e, err := loadEntitlements(r.Context(), s.db, id.TenantID)
	if errors.Is(err, errNoSubscription) {
		http.Error(w, "no subscription for this account", http.StatusForbidden)
		return false
	}
	if err != nil {
		http.Error(w, "billing unavailable", http.StatusServiceUnavailable)
		return false
	}
	if e.SubscriptionStatus != "active" {
		http.Error(w, "subscription is not active", http.StatusPaymentRequired)
		return false
	}
	if !e.has(feature) {
		http.Error(w, fmt.Sprintf("the %s plan does not include %s", e.Plan, knownFeatures[feature]), http.StatusForbidden)
		return false
	}
	return true
}

// operatorScopes are the fabric operator scopes a hosted MCP call carries
// when, and only when, the API key belongs to an account holding the
// creator platform role: the backup-manager and recovery-orchestrator
// admin routes demand them, and no customer account may hold them.
var operatorScopes = []string{"backups:admin", "recovery:admin", "extensions:admin"}

func hostedScopes(platformRole string) []string {
	scopes := []string{"memory:read", "memory:write"}
	if platformRole == "creator" {
		scopes = append(scopes, operatorScopes...)
	}
	return scopes
}
