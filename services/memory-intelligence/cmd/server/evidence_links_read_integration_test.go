//go:build integration

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

// A symbol version must belong to the same repository as its snapshot.
// The legacy schema has separate FKs but does not enforce this relationship.
func TestP6RejectsCrossTenantSymbolRepositoryMismatch(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.remember("P6 code provenance cannot borrow another tenant's symbol.", 0.9)
	localRepo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/p6/local/"+fact)
	localSnap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','local','active') RETURNING id::text", localRepo, f.tenant)
	foreignRepo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.otherTenant, "/p6/foreign/"+fact)
	foreignSnap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','foreign','active') RETURNING id::text", foreignRepo, f.otherTenant)
	foreignSymbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", foreignRepo, "foreign-"+fact, foreignSnap)
	mismatchedVersion := f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'Foreign.secret','private/foreign.go','file','abcd') RETURNING id::text", foreignSymbol, localSnap)
	code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", fmt.Sprintf(`{"fact_id":%q,"code_symbol_version_id":%q}`, fact, mismatchedVersion))
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant symbol with local snapshot accepted: %d %v", code, out)
	}
	// Direct SQL writers must not bypass the HTTP tenant/repository guard.
	_, err := f.admin.Exec("INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", f.tenant, fact, mismatchedVersion)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("direct SQL cross-repository link should be rejected with 23514, got %v", err)
	}
}

// A code symbol's repository may be corrupted after a legitimate link was
// written. Readback must fail closed instead of trusting the snapshot alone.
func TestP6ReadbackExcludesDriftedSymbolRepository(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.remember("P6 code readback must reject provenance drift.", 0.9)
	localRepo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/p6/drift/local/"+fact)
	localSnap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','local','active') RETURNING id::text", localRepo, f.tenant)
	localSymbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", localRepo, "local-"+fact, localSnap)
	localVersion := f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'Local.file','local/file.go','file','abcd') RETURNING id::text", localSymbol, localSnap)
	code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", fmt.Sprintf(`{"fact_id":%q,"code_symbol_version_id":%q}`, fact, localVersion))
	if code != http.StatusOK {
		t.Fatalf("valid evidence link = %d %v", code, out)
	}
	foreignRepo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.otherTenant, "/p6/drift/foreign/"+fact)
	foreignSnap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','foreign','active') RETURNING id::text", foreignRepo, f.otherTenant)
	foreignSymbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", foreignRepo, "foreign-"+fact, foreignSnap)
	if _, err := f.admin.Exec("UPDATE code_symbol_versions SET symbol_id=$1 WHERE id=$2", foreignSymbol, localVersion); err != nil {
		t.Fatal(err)
	}
	code, out, _ = f.call(f.srv.handleEvidenceLinks, http.MethodGet, "/v1/evidence-links?fact_id="+fact, "")
	if code != http.StatusOK {
		t.Fatalf("read after symbol identity drift = %d %v", code, out)
	}
	links, ok := out["links"].([]any)
	if !ok || len(links) != 0 {
		t.Fatalf("cross-tenant symbol identity leaked in readback: %v", out)
	}
}

// A snapshot's tenant must be bound to its repository by the database, not
// merely by HTTP handlers or joins. This also prevents later tenant drift.
func TestP6SnapshotTenantRepositoryInvariant(t *testing.T) {
	f := newBehaviourFixture(t)
	repo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/p6/tenant-invariant/"+f.tenant)
	var pgErr *pgconn.PgError
	_, err := f.admin.Exec(`INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id)
		VALUES($1,$2,'main','forged')`, repo, f.otherTenant)
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("cross-tenant snapshot must violate composite FK (23503), got %v", err)
	}
	valid := f.scalar(`INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id)
		VALUES($1,$2,'main','valid') RETURNING id::text`, repo, f.tenant)
	_, err = f.admin.Exec("UPDATE code_repository_snapshots SET tenant_id=$1 WHERE id=$2", f.otherTenant, valid)
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("snapshot tenant drift must violate composite FK (23503), got %v", err)
	}
	_, err = f.admin.Exec("UPDATE code_repositories SET tenant_id=$1 WHERE id=$2", f.otherTenant, repo)
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("repository tenant drift must violate composite FK (23503), got %v", err)
	}
	if _, err := f.admin.Exec("DELETE FROM code_repositories WHERE id=$1", repo); err != nil {
		t.Fatalf("repository deletion must retain snapshot cascade: %v", err)
	}
	var remains bool
	if err := f.admin.QueryRow("SELECT EXISTS(SELECT 1 FROM code_repository_snapshots WHERE id=$1)", valid).Scan(&remains); err != nil || remains {
		t.Fatalf("deleted repository left snapshot behind: remains=%v err=%v", remains, err)
	}
}
