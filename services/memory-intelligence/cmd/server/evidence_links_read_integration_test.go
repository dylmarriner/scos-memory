//go:build integration

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

func TestP6EvidenceLinkReadbackIsScopedAndPaginated(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.remember("P6 link readback preserves immutable symbol-version evidence.", 0.9)
	otherFact := f.otherTenantFact()

	repo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/p6/readback/"+fact)
	snap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','p6-test','active') RETURNING id::text", repo, f.tenant)

	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		symbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", repo, fmt.Sprintf("p6-link-%d", i), snap)
		version := f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,$3,$3,'file','abcd') RETURNING id::text", symbol, snap, fmt.Sprintf("p6/file-%d.go", i))
		code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", fmt.Sprintf(`{"fact_id":%q,"code_symbol_version_id":%q}`, fact, version))
		if code != http.StatusOK {
			t.Fatalf("create link %d = %d %v", i, code, out)
		}
		want[version] = true
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 4; page++ {
		path := "/v1/evidence-links?fact_id=" + url.QueryEscape(fact) + "&limit=1"
		if cursor != "" {
			path += "&after=" + url.QueryEscape(cursor)
		}
		code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("page %d = %d %v", page, code, out)
		}
		links, ok := out["links"].([]any)
		if !ok || len(links) != 1 {
			t.Fatalf("page %d invalid links %v", page, out)
		}
		link := links[0].(map[string]any)
		version, _ := link["code_symbol_version_id"].(string)
		if !want[version] || seen[version] || link["repository_id"] != repo || link["snapshot_id"] != snap ||
			link["commit_id"] != "p6-test" || link["link_type"] != "evidences" || link["source"] != "explicit" {
			t.Fatalf("page %d lost provenance or duplicated evidence: %v", page, link)
		}
		seen[version] = true
		next, _ := out["next_cursor"].(string)
		if page < 2 && next == "" {
			t.Fatalf("page %d ended before 3 links: %v", page, out)
		}
		if page == 2 {
			if next != "" {
				t.Fatalf("last page has extra cursor: %v", out)
			}
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatalf("got %d of 3 links", len(seen))
	}

	for _, tc := range []struct{ name, tenant, principal, fact string }{
		{"other tenant fact", f.tenant, f.owner, otherFact},
		{"other tenant principal", f.otherTenant, f.other, fact},
		{"non-member principal", f.tenant, f.scalar("INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human','p6-no-access-'||gen_random_uuid(),'Nonmember') RETURNING id::text", f.tenant), fact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/evidence-links?fact_id=" + tc.fact
			code, out, _ := f.callAs(tc.tenant, tc.principal, f.srv.handleEvidenceLinks, http.MethodGet, path, "")
			if code != http.StatusNotFound {
				t.Fatalf("cross-scope read = %d %v, want 404", code, out)
			}
		})
	}

	empty := f.remember("A fact with no code links.", 0.8)
	code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodGet, "/v1/evidence-links?fact_id="+empty, "")
	if code != http.StatusOK {
		t.Fatalf("empty fact read = %d %v", code, out)
	}
	if links, ok := out["links"].([]any); !ok || len(links) != 0 {
		t.Fatalf("empty fact must return []: %v", out)
	}
}
