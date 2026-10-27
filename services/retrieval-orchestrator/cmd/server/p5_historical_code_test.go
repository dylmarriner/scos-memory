package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestP5HistoricalCodeClientBindsImmutableSnapshotAndIdentity(t *testing.T) {
	const snapshot = "a3bc12f0-3456-4123-8abc-123456789abc"
	const commit = "deadbeef0123456789"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/code/query" || r.Header.Get("X-SCOS-Tenant-ID") != "tenant-1" || r.Header.Get("X-SCOS-Principal-ID") != "principal-1" {
			t.Fatalf("historical request lost endpoint or trusted identity")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["as_of_commit"] != commit || body["snapshot_id"] != snapshot || body["branch"] != "main" || body["repository_id"] != "repo-1" {
			t.Fatalf("historical selectors missing: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"historical": true, "snapshot_id": snapshot, "commit_id": commit, "branch": "main", "retrieval_mode": "historical_exact",
			"results": []map[string]any{{"SymbolVersionID": "sv-old", "SnapshotID": snapshot, "CommitID": commit, "Branch": "main", "Path": "old.go"}},
		})
	}))
	defer upstream.Close()
	client := &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}
	out, err := client.QueryHistorical(context.Background(), "tenant-1", "principal-1", "repo-1", "main", "old.go", 10, commit, snapshot)
	if err != nil || out == nil || !out.Historical || out.CommitID != commit || out.SnapshotID != snapshot || len(out.Results) != 1 {
		t.Fatalf("historical provenance missing: %+v, %v", out, err)
	}
}

func TestP5HistoricalCodeClientRejectsFalseOrMixedProvenance(t *testing.T) {
	const snapshot = "a3bc12f0-3456-4123-8abc-123456789abc"
	cases := []struct {
		name     string
		response string
	}{
		{"current fallback", `{"retrieval_mode":"hybrid","results":[]}`},
		{"wrong commit", `{"historical":true,"snapshot_id":"` + snapshot + `","commit_id":"other","branch":"main","results":[]}`},
		{"wrong branch", `{"historical":true,"snapshot_id":"` + snapshot + `","commit_id":"old","branch":"dev","results":[]}`},
		{"mixed snapshot", `{"historical":true,"snapshot_id":"` + snapshot + `","commit_id":"old","branch":"main","results":[{"SymbolVersionID":"sv-1","SnapshotID":"other","CommitID":"old","Branch":"main"}]}`},
		{"missing result provenance", `{"historical":true,"snapshot_id":"` + snapshot + `","commit_id":"old","branch":"main","results":[{"SymbolVersionID":"sv-1"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.response)) }))
			defer upstream.Close()
			client := &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}
			if out, err := client.QueryHistorical(context.Background(), "tenant", "principal", "repo", "main", "old.go", 10, "old", snapshot); err == nil {
				t.Fatalf("accepted untrusted historical result: %+v", out)
			}
		})
	}
}

func TestP5HistoricalRetrievalFailsClosedBeforeDatabaseAccess(t *testing.T) {
	srv := &server{}
	cases := []struct {
		name, payload string
		want          int
	}{
		{"missing repository", `{"tenant_id":"t","principal_id":"p","query":"memory","branch":"main","as_of_commit":"old"}`, http.StatusBadRequest},
		{"missing branch", `{"tenant_id":"t","principal_id":"p","query":"memory","repository_id":"repo","as_of_commit":"old"}`, http.StatusBadRequest},
		{"no code service", `{"tenant_id":"t","principal_id":"p","query":"remember my preference","repository_id":"repo","branch":"main","as_of_commit":"old"}`, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewBufferString(tc.payload)))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestP5HistoricalCodeSelectorsAreAudited(t *testing.T) {
	req := RetrieveRequest{RepositoryID: "repo-1", Branch: "main", AsOfCommit: "old", SnapshotID: "snapshot"}
	got := appliedRestrictions(req)
	if got["as_of_commit"] != "old" || got["snapshot_id"] != "snapshot" || got["repository_id"] != "repo-1" || got["branch"] != "main" || got["code_temporal_scope"] != "immutable_snapshot" {
		t.Fatalf("missing immutable code selectors in retrieval audit: %#v", got)
	}
	if strings.Contains(strings.TrimSpace(toJSONForTest(appliedRestrictions(RetrieveRequest{}))), "as_of_commit") {
		t.Fatal("ordinary retrieval falsely claims a historical snapshot")
	}
}

func toJSONForTest(value any) string { b, _ := json.Marshal(value); return string(b) }

func TestP5HistoricalRetrievalPreservesSafeNotFoundAndAmbiguity(t *testing.T) {
	const secret = "postgres://hidden_operator:password@private-db:5432/private"
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "private error: "+secret, status)
			}))
			defer upstream.Close()
			srv := &server{codeIntelligence: &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"tenant_id":"t","principal_id":"p","query":"remember the previous code","repository_id":"repo","branch":"main","as_of_commit":"old"}`))
			srv.handleRetrieve(rec, req)
			want := status
			if status == http.StatusServiceUnavailable {
				want = http.StatusBadGateway
			}
			if rec.Code != want || strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("status %d want %d; leaked=%t", rec.Code, want, strings.Contains(rec.Body.String(), secret))
			}
		})
	}
}

func TestP5HistoricalCodeClientSupportsCommitOrSnapshotSelection(t *testing.T) {
	const snapshot = "a3bc12f0-3456-4123-8abc-123456789abc"
	const commit = "old-commit"
	for _, tc := range []struct{ name, requestedCommit, requestedSnapshot string }{
		{"commit only", commit, ""},
		{"snapshot only", "", snapshot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["as_of_commit"] != nil && body["as_of_commit"] != tc.requestedCommit {
					t.Errorf("wrong commit selector: %v", body)
				}
				if body["snapshot_id"] != nil && body["snapshot_id"] != tc.requestedSnapshot {
					t.Errorf("wrong snapshot selector: %v", body)
				}
				if tc.requestedCommit == "" && body["as_of_commit"] != nil {
					t.Error("forwarded unrequested commit")
				}
				if tc.requestedSnapshot == "" && body["snapshot_id"] != nil {
					t.Error("forwarded unrequested snapshot")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"historical": true, "snapshot_id": snapshot, "commit_id": commit, "branch": "main", "results": []any{},
				})
			}))
			defer upstream.Close()
			client := &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}
			got, err := client.QueryHistorical(context.Background(), "tenant", "principal", "repo", "main", "code", 5, tc.requestedCommit, tc.requestedSnapshot)
			if err != nil || got == nil || !got.Historical {
				t.Fatalf("selector failed: %+v %v", got, err)
			}
		})
	}
	client := &codeIntelligenceClient{}
	if _, err := client.QueryHistorical(context.Background(), "tenant", "principal", "repo", "main", "code", 5, "", ""); err == nil {
		t.Fatal("historical query without immutable selector must fail")
	}
}

func TestP5HistoricalCodeClientAcceptsCanonicalUUIDCase(t *testing.T) {
	const canonical = "a3bc12f0-3456-4123-8abc-123456789abc"
	const mixedCase = "A3BC12F0-3456-4123-8ABC-123456789ABC"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["snapshot_id"] != mixedCase {
			t.Errorf("selector was silently changed before lookup: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"historical": true, "snapshot_id": canonical, "commit_id": "old", "branch": "main", "results": []map[string]any{
				{"SymbolVersionID": "sv-1", "SnapshotID": canonical, "CommitID": "old", "Branch": "main"},
			},
		})
	}))
	defer upstream.Close()
	client := &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}
	got, err := client.QueryHistorical(context.Background(), "tenant", "principal", "repo", "main", "code", 5, "old", mixedCase)
	if err != nil || got == nil || got.SnapshotID != canonical {
		t.Fatalf("case-equivalent UUID rejected: %+v %v", got, err)
	}
}

func TestP5HistoricalFailureLogsOnlyStatusOrProvenanceCategory(t *testing.T) {
	const secret = "postgres://sensitive_operator:password@private-host:5432/private-db"
	for _, tc := range []struct {
		name         string
		status       int
		body, marker string
	}{
		{"downstream HTTP", http.StatusServiceUnavailable, secret, "HTTP 503"},
		{"provenance mismatch", http.StatusOK, `{"historical":false,"results":[],"private":"` + secret + `"}`, "provenance mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			srv := &server{codeIntelligence: &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}}
			var logs bytes.Buffer
			prior := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(prior)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"tenant_id":"t","principal_id":"p","query":"remember the old code","repository_id":"repo","branch":"main","as_of_commit":"old"}`))
			srv.handleRetrieve(rec, req)
			if rec.Code != http.StatusBadGateway || !strings.Contains(logs.String(), tc.marker) || strings.Contains(logs.String(), secret) || strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("unsafe or unhelpful failure: status=%d logs=%q", rec.Code, logs.String())
			}
		})
	}
}
