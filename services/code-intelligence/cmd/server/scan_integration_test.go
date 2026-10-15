package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// TestHandlePGScanIndexesAWorkspaceRepositoryInTheBackground drives the
// scan endpoint against a real store: a repository in the workspace is read,
// indexed in the background and queryable once ready; rescanning an
// unchanged tree reuses its snapshot; an edit produces a new one.
func TestHandlePGScanIndexesAWorkspaceRepositoryInTheBackground(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	workspace := t.TempDir()
	repo := filepath.Join(workspace, "svc")
	writeFile(t, filepath.Join(repo, "auth", "token.go"), "package auth\n\n// SignToken signs a token.\nfunc SignToken() string { return verify() }\n\nfunc verify() string { return \"ok\" }\n")
	writeFile(t, filepath.Join(repo, "web", "client.ts"), "export function fetchUser(id: string) { return id }\n")
	writeFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(repo, ".git", "refs", "heads", "main"), "abcdefabcdefabcdefabcdefabcdefabcdefabcd\n")
	t.Setenv("SCOS_WORKSPACE_ROOT", workspace)
	t.Setenv("SCOS_WORKSPACE_HOST_PATH", "/home/dev/code")

	call := func(handler http.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		} else {
			reader = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(method, path, reader)
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p"}))
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	reg := call(srv.handlePGRepos, http.MethodPost, "/v2/code/repos", map[string]any{"canonical_path": "/home/dev/code/svc"})
	if reg.Code != http.StatusOK {
		t.Fatalf("register: %d %s", reg.Code, reg.Body.String())
	}
	var repoResp struct {
		RepositoryID string `json:"repository_id"`
	}
	_ = json.Unmarshal(reg.Body.Bytes(), &repoResp)

	scan := func() map[string]any {
		t.Helper()
		w := call(srv.handlePGScan, http.MethodPost, "/v2/code/scan", map[string]any{"repository_id": repoResp.RepositoryID})
		if w.Code != http.StatusAccepted && w.Code != http.StatusOK {
			t.Fatalf("scan: %d %s", w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	waitReady := func(snapshotID string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			w := call(srv.handlePGSnapshot, http.MethodGet, "/v2/code/snapshots/"+snapshotID, nil)
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			switch out["status"] {
			case "ready", "active":
				return out
			case "failed":
				t.Fatalf("snapshot failed: %v", out["error"])
			}
			if time.Now().After(deadline) {
				t.Fatalf("snapshot not ready: %v", out)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	first := scan()
	if first["status"] != "indexing" || first["branch"] != "main" || first["git_commit"] != "abcdefabcdefabcdefabcdefabcdefabcdefabcd" || first["file_count"] != float64(2) {
		t.Fatalf("first scan = %v", first)
	}
	ready := waitReady(first["snapshot_id"].(string))
	if ready["symbol_count"].(float64) < 5 {
		t.Fatalf("expected file and declaration symbols, got %v", ready)
	}

	q := call(srv.handlePGQuery, http.MethodPost, "/v2/code/query", map[string]any{"repository_id": repoResp.RepositoryID, "query": "SignToken"})
	if q.Code != http.StatusOK || !bytes.Contains(q.Body.Bytes(), []byte("SignToken")) {
		t.Fatalf("query without a branch must use the indexed branch and find SignToken: %d %s", q.Code, q.Body.String())
	}
	var queried struct {
		Results []struct {
			FullyQualifiedName string
		} `json:"results"`
		RetrievalMode string `json:"retrieval_mode"`
	}
	_ = json.Unmarshal(q.Body.Bytes(), &queried)
	if queried.RetrievalMode != "exact" || len(queried.Results) == 0 || queried.Results[0].FullyQualifiedName != "auth.SignToken" {
		t.Fatalf("a bare declaration name must anchor on that declaration first: %s", q.Body.String())
	}

	again := scan()
	if again["unchanged"] != true || again["snapshot_id"] != first["snapshot_id"] {
		t.Fatalf("rescanning an unchanged tree must reuse its snapshot: %v", again)
	}

	writeFile(t, filepath.Join(repo, "auth", "token.go"), "package auth\n\nfunc SignToken() string { return \"v2\" }\n")
	edited := scan()
	if edited["snapshot_id"] == first["snapshot_id"] || edited["status"] != "indexing" {
		t.Fatalf("an edited tree must be indexed as a new snapshot: %v", edited)
	}
	waitReady(edited["snapshot_id"].(string))

	outside := call(srv.handlePGRepos, http.MethodPost, "/v2/code/repos", map[string]any{"canonical_path": "/etc"})
	var outsideRepo struct {
		RepositoryID string `json:"repository_id"`
	}
	_ = json.Unmarshal(outside.Body.Bytes(), &outsideRepo)
	if w := call(srv.handlePGScan, http.MethodPost, "/v2/code/scan", map[string]any{"repository_id": outsideRepo.RepositoryID}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a repository outside the workspace must be refused, got %d %s", w.Code, w.Body.String())
	}
	_ = os.Remove(filepath.Join(repo, "web", "client.ts"))
}
