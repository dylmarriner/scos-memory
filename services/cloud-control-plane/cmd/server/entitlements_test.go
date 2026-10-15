package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func int64p(v int64) *int64 { return &v }

func TestPlanLinesAreRenderedFromEnforcedLimits(t *testing.T) {
	got := planLines(int64p(10000), int64p(1_000_000_000), nil)
	want := []string{"10,000 MCP requests/month", "1 GB memory storage"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("free plan lines = %v, want %v", got, want)
	}
	got = planLines(int64p(5_000_000), int64p(1_000_000_000_000), []string{"graphify"})
	want = []string{"5,000,000 MCP requests/month", "1 TB memory storage", "Graphify code knowledge graph"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("expert plan lines = %v, want %v", got, want)
	}
	got = planLines(nil, nil, []string{"graphify"})
	if got[0] != "Unlimited MCP requests" || got[1] != "Unlimited memory storage" {
		t.Fatalf("unlimited plan lines = %v", got)
	}
	if s := formatStorage(1_500_000_000); s != "1.5 GB" {
		t.Fatalf("formatStorage = %q", s)
	}
}

func TestCheckToolEntitlementsGatesFeatures(t *testing.T) {
	free := planEntitlements{Plan: "free", SubscriptionStatus: "active", MonthlyMCPRequests: int64p(10000), StorageBytes: int64p(1000)}
	measured := false
	measure := func(context.Context) (int64, error) { measured = true; return 0, nil }
	err := checkToolEntitlements(context.Background(), free, []string{"scos_recall", "scos_graphify_query"}, measure)
	var qe *quotaError
	if !errors.As(err, &qe) || qe.status != http.StatusForbidden || qe.code != "feature_not_in_plan" {
		t.Fatalf("graphify on the free plan must be refused, got %v", err)
	}
	if err := checkToolEntitlements(context.Background(), free, []string{"scos_recall", "scos_memory_summary"}, measure); err != nil {
		t.Fatalf("ungated reads must pass: %v", err)
	}
	if measured {
		t.Fatal("storage must not be measured when no tool adds memory")
	}
	pro := free
	pro.Plan, pro.Features = "pro", []string{"graphify"}
	if err := checkToolEntitlements(context.Background(), pro, []string{"scos_graphify_query"}, measure); err != nil {
		t.Fatalf("graphify on a plan that includes it must pass: %v", err)
	}
}

func TestCheckToolEntitlementsEnforcesStorageQuota(t *testing.T) {
	plan := planEntitlements{Plan: "free", SubscriptionStatus: "active", StorageBytes: int64p(1000)}
	stored := int64(999)
	measure := func(context.Context) (int64, error) { return stored, nil }
	if err := checkToolEntitlements(context.Background(), plan, []string{"scos_remember"}, measure); err != nil {
		t.Fatalf("a write under quota must pass: %v", err)
	}
	stored = 1000
	err := checkToolEntitlements(context.Background(), plan, []string{"scos_remember"}, measure)
	var qe *quotaError
	if !errors.As(err, &qe) || qe.status != http.StatusInsufficientStorage {
		t.Fatalf("a write at the storage quota must be refused, got %v", err)
	}
	// Forgetting is how a tenant gets back under quota: never refused.
	if err := checkToolEntitlements(context.Background(), plan, []string{"scos_forget", "scos_recall"}, measure); err != nil {
		t.Fatalf("forget/read at quota must pass: %v", err)
	}
	// Unlimited storage never measures.
	plan.StorageBytes = nil
	failing := func(context.Context) (int64, error) { return 0, errors.New("must not be called") }
	if err := checkToolEntitlements(context.Background(), plan, []string{"scos_remember"}, failing); err != nil {
		t.Fatalf("unlimited storage must not measure: %v", err)
	}
	// A measurement failure is not a pass.
	plan.StorageBytes = int64p(1000)
	if err := checkToolEntitlements(context.Background(), plan, []string{"scos_remember"}, failing); err == nil {
		t.Fatal("a failed storage measurement must not admit the write")
	}
}

func TestHostedScopesCarryOperatorScopesForCreatorsOnly(t *testing.T) {
	for _, role := range []string{"user", "", "admin"} {
		for _, s := range hostedScopes(role) {
			if s == "backups:admin" || s == "recovery:admin" {
				t.Fatalf("role %q received operator scope %s", role, s)
			}
		}
	}
	got := strings.Join(hostedScopes("creator"), ",")
	if !strings.Contains(got, "backups:admin") || !strings.Contains(got, "recovery:admin") || !strings.Contains(got, "memory:read") {
		t.Fatalf("creator scopes = %s", got)
	}
}

func TestCalledToolsAndRefusalAddressing(t *testing.T) {
	body := []byte(`[{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"scos_graphify_path"}},{"jsonrpc":"2.0","id":8,"method":"tools/list"}]`)
	if got := calledTools(body); len(got) != 1 || got[0] != "scos_graphify_path" {
		t.Fatalf("calledTools = %v", got)
	}
	rec := httptest.NewRecorder()
	writeMCPRefusal(rec, body, &quotaError{status: http.StatusTooManyRequests, code: "request_quota_reached", msg: "quota"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("refusal = %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var out struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Data map[string]string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || string(out.ID) != "7" || out.Error.Data["reason"] != "request_quota_reached" {
		t.Fatalf("refusal body = %s (%v)", rec.Body.String(), err)
	}
	next := beginningOfNextMonth(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC))
	if !next.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("quota reset = %v", next)
	}
}

func TestValidLimits(t *testing.T) {
	if err := validLimits(&planLimitsInput{MonthlyMCPRequests: int64p(-1)}); err == nil {
		t.Fatal("negative request quota accepted")
	}
	if err := validLimits(&planLimitsInput{Features: []string{"advanced_retrieval"}}); err == nil {
		t.Fatal("a feature nothing enforces was accepted")
	}
	if err := validLimits(&planLimitsInput{Features: []string{"graphify", "graphify"}}); err == nil {
		t.Fatal("duplicate feature accepted")
	}
	if err := validLimits(&planLimitsInput{MonthlyMCPRequests: nil, StorageBytes: int64p(0), Features: []string{"graphify"}}); err != nil {
		t.Fatalf("valid limits refused: %v", err)
	}
}
