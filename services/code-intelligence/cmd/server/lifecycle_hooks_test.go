package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	"google.golang.org/protobuf/proto"
)

const hookTestSecret = "issuer-secret-for-hook-tests"

// fakeExtensionManager verifies the dispatch bearer the way
// extension-manager does (auth.AuthenticatedHandler plus signed claims) and
// records every body it accepts along with the verified claims.
func fakeExtensionManager(t *testing.T) (*hooksdk.Client, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 4)
	cfg := auth.Config{Token: "internal", IssuerSecret: hookTestSecret, Identity: "internal-service"}
	srv := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.CanonicalClaims(r.Context())
		if !ok {
			http.Error(w, "no claims", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["claims_tenant"], body["claims_principal"], body["claims_subject"] = claims.TenantID, claims.PrincipalID, claims.Subject
		got <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []hooksdk.DispatchResult{{ExtensionID: "ext-1", Status: "succeeded"}}})
	}), cfg))
	t.Cleanup(srv.Close)
	return hooksdk.NewClient("code-intelligence", srv.URL, hookTestSecret, srv.Client()), got
}

func awaitHook(t *testing.T, got <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case body := <-got:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("on_index_change was not dispatched")
		return nil
	}
}

func assertIndexChange(t *testing.T, body map[string]any, tenant, principal string, want map[string]any) {
	t.Helper()
	if body["event"] != hooksdk.OnIndexChange || body["tenant_id"] != tenant || body["principal_id"] != principal || body["claims_tenant"] != tenant {
		t.Fatalf("dispatch envelope = %+v", body)
	}
	payload, _ := body["payload"].(map[string]any)
	for k, v := range want {
		if payload[k] != v {
			t.Fatalf("payload[%s] = %v, want %v (payload %+v)", k, payload[k], v, payload)
		}
	}
}

func createPGRepo(t *testing.T, srv *server, tenant, path string) string {
	t.Helper()
	resp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{"canonical_path": path})
	var out struct {
		RepositoryID string `json:"repository_id"`
	}
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &out) != nil {
		t.Fatalf("create repo: %d: %s", resp.Code, resp.Body.String())
	}
	return out.RepositoryID
}

func TestHandlePGIndexDispatchesOnIndexChangeAfterActivation(t *testing.T) {
	_, db := testPGStore(t)
	hooks, got := fakeExtensionManager(t)
	srv := &server{pgStore: newPGCodeStore(db), hooks: hooks}
	tenant := testTenant(t, db)
	path := "/repo/" + t.Name()
	repoID := createPGRepo(t, srv, tenant, path)

	resp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": repoID, "branch": "main", "commit_id": "commit-1",
		"files": []map[string]any{
			{"path": "a.go", "content_hash": hashContent("a")},
			{"path": "b.go", "content_hash": hashContent("b")},
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		SnapshotID string `json:"snapshot_id"`
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &out)
	body := awaitHook(t, got)
	assertIndexChange(t, body, tenant, "p", map[string]any{
		"repository_id": repoID, "repository_path": path, "branch": "main",
		"commit_id": "commit-1", "snapshot_id": out.SnapshotID, "files_indexed": float64(2),
	})
	// The event must describe a committed, served snapshot.
	var status string
	if err := db.QueryRow(`SELECT status::text FROM code_repository_snapshots WHERE id = $1`, out.SnapshotID).Scan(&status); err != nil || status != "active" {
		t.Fatalf("snapshot status = %q (%v), want active", status, err)
	}
}

func TestHandlePGIndexSCIPDispatchesOnIndexChange(t *testing.T) {
	_, db := testPGStore(t)
	hooks, got := fakeExtensionManager(t)
	srv := &server{pgStore: newPGCodeStore(db), hooks: hooks}
	tenant := testTenant(t, db)
	path := "/repo/" + t.Name()
	repoID := createPGRepo(t, srv, tenant, path)
	index := sampleSCIPIndex()
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	resp := pgRequest(t, srv, http.MethodPost, "/v2/code/index/scip", tenant, map[string]any{
		"repository_id": repoID, "branch": "main", "commit_id": "commit-scip",
		"index_base64": base64.StdEncoding.EncodeToString(data),
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("index scip: %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		SnapshotID string `json:"snapshot_id"`
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &out)
	assertIndexChange(t, awaitHook(t, got), tenant, "p", map[string]any{
		"repository_id": repoID, "repository_path": path, "branch": "main",
		"commit_id": "commit-scip", "snapshot_id": out.SnapshotID, "files_indexed": float64(len(index.Documents)),
	})
}

// The v1 in-memory index goes through the real auth middleware with a
// signed caller token, so the dispatch must forward the caller's own claims.
func TestHandleIndexV1DispatchesOnIndexChangeWithCallerClaims(t *testing.T) {
	hooks, got := fakeExtensionManager(t)
	store := newCodeStore()
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo/one", Branch: "main"}
	srv := &server{store: store, hooks: hooks}
	token, err := auth.SignClaims(auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1", Subject: "user-7", ExpiresAt: time.Now().Add(time.Minute)}, hookTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/code/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"abc","files":["main.go","util.go"]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.AuthenticatedHandler(http.HandlerFunc(srv.handleIndex), auth.Config{IssuerSecret: hookTestSecret, Identity: "x"}).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", w.Code, w.Body.String())
	}
	body := awaitHook(t, got)
	assertIndexChange(t, body, "tenant-1", "principal-1", map[string]any{
		"repository_id": "repo-1", "repository_path": "/repo/one", "branch": "main",
		"commit_id": "abc", "snapshot_id": "repo-1@1", "files_indexed": float64(2),
	})
	if body["claims_subject"] != "user-7" {
		t.Fatalf("caller claims not forwarded: %+v", body)
	}
}

// logLines is a log sink safe to read while Notify's goroutine writes.
type logLines chan string

func (l logLines) Write(p []byte) (int, error) { l <- string(p); return len(p), nil }

func TestIndexSucceedsAndLogsWhenExtensionManagerFails(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "extension manager down", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	lines := make(logLines, 16)
	original := log.Writer()
	log.SetOutput(lines)
	defer log.SetOutput(original)

	store := newCodeStore()
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo/one"}
	srv := &server{store: store, hooks: hooksdk.NewClient("code-intelligence", failing.URL, hookTestSecret, failing.Client())}
	r := httptest.NewRequest(http.MethodPost, "/v1/code/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"abc","files":["main.go"]}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	w := httptest.NewRecorder()
	srv.handleIndex(w, r)
	if w.Code != http.StatusOK || store.generations["repo-1"].Status != "active" {
		t.Fatalf("index must commit regardless of hook delivery: %d %s", w.Code, w.Body.String())
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, "on_index_change not delivered") && strings.Contains(line, "extension manager down") {
				return
			}
		case <-deadline:
			t.Fatal("failed dispatch was not logged")
		}
	}
}

func expectNoMoreHooks(t *testing.T, got <-chan map[string]any) {
	t.Helper()
	select {
	case body := <-got:
		t.Fatalf("retry raised a second on_index_change: %+v", body)
	case <-time.After(300 * time.Millisecond):
	}
}

// A retried delivery resolves to the same, already-active snapshot and
// changes nothing, so exactly one event is sent for the pair.
func TestRetriedPGIndexSendsExactlyOneIndexChange(t *testing.T) {
	_, db := testPGStore(t)
	hooks, got := fakeExtensionManager(t)
	srv := &server{pgStore: newPGCodeStore(db), hooks: hooks}
	tenant := testTenant(t, db)
	repoID := createPGRepo(t, srv, tenant, "/repo/"+t.Name())
	req := map[string]any{
		"repository_id": repoID, "branch": "main", "commit_id": "commit-1",
		"files": []map[string]any{{"path": "a.go", "content_hash": hashContent("a")}},
	}
	var snapshots []string
	for i := 0; i < 2; i++ {
		resp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("index attempt %d: %d: %s", i, resp.Code, resp.Body.String())
		}
		var out struct {
			SnapshotID string `json:"snapshot_id"`
		}
		_ = json.Unmarshal(resp.Body.Bytes(), &out)
		snapshots = append(snapshots, out.SnapshotID)
	}
	if snapshots[0] != snapshots[1] {
		t.Fatalf("retry did not resolve to the same snapshot: %v", snapshots)
	}
	assertIndexChange(t, awaitHook(t, got), tenant, "p", map[string]any{"snapshot_id": snapshots[0]})
	expectNoMoreHooks(t, got)
}

func TestRetriedSCIPIndexSendsExactlyOneIndexChange(t *testing.T) {
	_, db := testPGStore(t)
	hooks, got := fakeExtensionManager(t)
	srv := &server{pgStore: newPGCodeStore(db), hooks: hooks}
	tenant := testTenant(t, db)
	repoID := createPGRepo(t, srv, tenant, "/repo/"+t.Name())
	data, err := proto.Marshal(sampleSCIPIndex())
	if err != nil {
		t.Fatal(err)
	}
	req := map[string]any{"repository_id": repoID, "branch": "main", "commit_id": "commit-1", "index_base64": base64.StdEncoding.EncodeToString(data)}
	var counts []float64
	for i := 0; i < 2; i++ {
		resp := pgRequest(t, srv, http.MethodPost, "/v2/code/index/scip", tenant, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("scip attempt %d: %d: %s", i, resp.Code, resp.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(resp.Body.Bytes(), &out)
		counts = append(counts, out["symbol_count"].(float64))
	}
	if counts[0] != counts[1] || counts[0] == 0 {
		t.Fatalf("retry must report the recorded symbol count: %v", counts)
	}
	awaitHook(t, got)
	expectNoMoreHooks(t, got)
}
