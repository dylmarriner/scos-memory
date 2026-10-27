//go:build integration

package main

import (
	"fmt"
	"net/http"
	"testing"
)

func TestP6CodeEvidenceReverseReadIsScopedAndPaginated(t *testing.T) {
	f := newBehaviourFixture(t)
	first := f.remember("Reverse P6 read protects memory dependency one.", 0.9)
	second := f.remember("Reverse P6 read protects memory dependency two.", 0.9)
	foreign := f.otherTenantFact()
	repo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/p6/reverse/"+first)
	snap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','reverse-commit','active') RETURNING id::text", repo, f.tenant)
	sym := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", repo, "p6-reverse-"+first, snap)
	version := f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'P6.reverse','source/reverse.go','file','abcd') RETURNING id::text", sym, snap)
	for _, fact := range []string{first, second} {
		code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links",
			fmt.Sprintf(`{"fact_id":%q,"code_symbol_version_id":%q}`, fact, version))
		if code != http.StatusOK {
			t.Fatalf("link fact %s failed: %d %v", fact, code, out)
		}
	}
	seen := map[string]bool{}
	after := ""
	for page := 0; page < 2; page++ {
		path := "/v1/evidence-links/by-code?code_symbol_version_id=" + version + "&limit=1"
		if after != "" {
			path += "&after=" + after
		}
		code, out, _ := f.call(f.srv.handleCodeEvidenceLinksRead, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("page %d: %d %v", page, code, out)
		}
		if out["code_symbol_version_id"] != version {
			t.Fatalf("wrong version: %v", out)
		}
		links, ok := out["links"].([]any)
		if !ok || len(links) != 1 {
			t.Fatalf("invalid reverse page: %v", out)
		}
		link := links[0].(map[string]any)
		fact, _ := link["fact_id"].(string)
		if (fact != first && fact != second) || seen[fact] || link["repository_id"] != repo ||
			link["snapshot_id"] != snap || link["commit_id"] != "reverse-commit" ||
			link["path"] != "source/reverse.go" {
			t.Fatalf("wrong or duplicate historical evidence: %v", link)
		}
		seen[fact] = true
		if page == 0 {
			after, _ = out["next_cursor"].(string)
			if after == "" {
				t.Fatal("first page missing cursor")
			}
		} else if out["next_cursor"] != nil {
			t.Fatalf("final page unexpectedly has cursor: %v", out)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("read %d of 2 facts", len(seen))
	}
	inaccessible := f.scalar("INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human','p6-reverse-no-access-'||gen_random_uuid(),'Nonmember') RETURNING id::text", f.tenant)
	path := "/v1/evidence-links/by-code?code_symbol_version_id=" + version
	for _, tc := range []struct{ tenant, principal string }{
		{f.tenant, inaccessible},
		{f.otherTenant, f.other},
	} {
		code, out, _ := f.callAs(tc.tenant, tc.principal, f.srv.handleCodeEvidenceLinksRead, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("scope reads should not leak existence: %d %v", code, out)
		}
		links, ok := out["links"].([]any)
		if !ok || len(links) != 0 {
			t.Fatalf("unscoped memory links leaked: %v", out)
		}
	}
	// A foreign tenant fact may never be falsely returned because a caller
	// requests its UUID, nor should the endpoint accept combined selectors.
	code, out, _ := f.call(f.srv.handleCodeEvidenceLinksRead, http.MethodGet, path+"&fact_id="+foreign, "")
	if code != http.StatusBadRequest {
		t.Fatalf("selector mixing accepted: %d %v", code, out)
	}
}
