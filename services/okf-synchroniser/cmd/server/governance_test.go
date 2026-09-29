package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func grantAllRoles(context.Context, string, string) (bool, error) { return true, nil }

// grantRoles grants exactly the listed principal/role pairs ("bob:knowledge_curator").
func grantRoles(pairs ...string) func(context.Context, string, string) (bool, error) {
	allowed := map[string]bool{}
	for _, p := range pairs {
		allowed[p] = true
	}
	return func(_ context.Context, principal, role string) (bool, error) {
		return allowed[principal+":"+role], nil
	}
}

func asPrincipal(r *http.Request, principal string) *http.Request {
	return r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "t", PrincipalID: principal}))
}

func TestRequireRoleFailsClosedWithoutRoleSource(t *testing.T) {
	s := &okfServer{}
	if err := s.requireRole(context.Background(), "bob", "knowledge_curator"); err == nil {
		t.Fatal("with no role source, authorisation must be refused, not assumed")
	}
}

func TestProposerIsTheAuthenticatedCaller(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "policy.md"), []byte("# old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &okfServer{okfRoot: root, proposals: map[string]*OKFProposal{}}
	post := func(body string, principal string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/proposals", strings.NewReader(body))
		if principal != "" {
			r = asPrincipal(r, principal)
		}
		w := httptest.NewRecorder()
		s.handleProposals(w, r)
		return w
	}
	if w := post(`{"path":"policy.md","content":"# a\n","proposer":"mallory"}`, "alice"); w.Code != http.StatusForbidden {
		t.Fatalf("naming someone else as proposer must be refused, got %d", w.Code)
	}
	if w := post(`{"path":"policy.md","content":"# b\n"}`, "alice"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"proposer":"alice"`) {
		t.Fatalf("proposal must be attributed to the caller: %d %s", w.Code, w.Body.String())
	}
	// A bare service credential carries no principal: it may not claim a person.
	if w := post(`{"path":"policy.md","content":"# c\n","proposer":"alice"}`, ""); w.Code != http.StatusForbidden {
		t.Fatalf("service credential impersonating a person must be refused, got %d", w.Code)
	}
	if w := post(`{"path":"policy.md","content":"# d\n","proposer":"system:memory-intelligence-auto-promotion"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("service credential may propose as a system identity, got %d %s", w.Code, w.Body.String())
	}
}

func TestReviewIsAttributedToTheCallerNotTheBody(t *testing.T) {
	s := &okfServer{roleLookup: grantAllRoles, proposals: map[string]*OKFProposal{
		"p": {ID: "p", Path: "x.md", Proposer: "alice", Status: "diffed"},
	}}
	review := func(body, principal string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/proposals/p/review", strings.NewReader(body))
		if principal != "" {
			r = asPrincipal(r, principal)
		}
		w := httptest.NewRecorder()
		s.handleProposalAction(w, r)
		return w
	}
	if w := review(`{"reviewer":"carol"}`, "bob"); w.Code == http.StatusOK {
		t.Fatal("a body-named reviewer different from the caller must be refused")
	}
	if w := review(`{"reviewer":"bob"}`, ""); w.Code == http.StatusOK {
		t.Fatal("review without an authenticated principal must be refused")
	}
	if w := review(`{}`, "bob"); w.Code != http.StatusOK {
		t.Fatalf("caller review must succeed: %d %s", w.Code, w.Body.String())
	}
	if got := s.proposals["p"].Reviewer; got != "bob" {
		t.Fatalf("reviewer = %q, want the authenticated caller", got)
	}
}

// approvedProposal builds a git-backed OKF root with policy.md committed on
// main and an independently reviewed, approved proposal changing it.
func approvedProposal(t *testing.T, roles func(context.Context, string, string) (bool, error)) (*okfServer, *OKFProposal, string) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main")
	runGit(t, root, "config", "user.email", "t@example.invalid")
	runGit(t, root, "config", "user.name", "t")
	path := filepath.Join(root, "policy.md")
	if err := os.WriteFile(path, []byte("# old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "policy.md")
	runGit(t, root, "commit", "-q", "-m", "seed")
	s := &okfServer{okfRoot: root, proposals: map[string]*OKFProposal{}, documents: map[string]*OKFDocument{}, projections: map[string]*IndexProjection{}, roleLookup: roles}
	ctx := context.Background()
	p, err := s.createProposal(okfProposalRequest{Path: "policy.md", Content: "# new\n", Proposer: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.validateProposalContext(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.diffProposal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reviewProposalAuthorized(ctx, p.ID, "bob", "knowledge_curator", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decideProposal(ctx, p.ID, true, "bob", "knowledge_curator", "ok"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_OKF_PROTECTED_BRANCH", "main")
	return s, p, path
}

func TestMergeEnforcesSeparationRoleBranchAndCleanTree(t *testing.T) {
	s, p, path := approvedProposal(t, grantRoles("bob:knowledge_curator", "carol:knowledge_curator", "alice:knowledge_curator"))
	ctx := context.Background()
	if _, err := s.mergeProposal(ctx, p.ID, "alice"); err == nil {
		t.Fatal("the proposer must not merge their own change")
	}
	if _, err := s.mergeProposal(ctx, p.ID, "dave"); err == nil {
		t.Fatal("a merger without a curator/owner role must be refused")
	}
	t.Setenv("SCOS_OKF_PROTECTED_BRANCH", "release")
	if _, err := s.mergeProposal(ctx, p.ID, "carol"); err == nil || !strings.Contains(err.Error(), "protected branch") {
		t.Fatalf("merging while main is checked out but release is protected must fail, got %v", err)
	}
	t.Setenv("SCOS_OKF_PROTECTED_BRANCH", "main")
	stray := filepath.Join(s.okfRoot, "stray.md")
	if err := os.WriteFile(stray, []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mergeProposal(ctx, p.ID, "carol"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("a dirty working tree must block the merge, got %v", err)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	merged, err := s.mergeProposal(ctx, p.ID, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if merged.Status != "merged" || merged.MergedCommit == "" {
		t.Fatalf("unexpected merge result: %#v", merged)
	}
	if b, _ := os.ReadFile(path); string(b) != "# new\n" {
		t.Fatalf("merged content = %q", b)
	}
	if author := strings.TrimSpace(string(runGit(t, s.okfRoot, "log", "-1", "--format=%an"))); author != "carol" {
		t.Fatalf("commit author = %q, want the merger", author)
	}
}

func TestFailedCommitRestoresTheWorkingFile(t *testing.T) {
	s, p, path := approvedProposal(t, grantAllRoles)
	hook := filepath.Join(s.okfRoot, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mergeProposal(context.Background(), p.ID, "carol"); err == nil {
		t.Fatal("expected the rejected commit to fail the merge")
	}
	if b, _ := os.ReadFile(path); string(b) != "# old\n" {
		t.Fatalf("working file left as %q after a failed merge; must be restored", b)
	}
	if st := strings.TrimSpace(string(runGit(t, s.okfRoot, "status", "--porcelain"))); st != "" {
		t.Fatalf("working tree not clean after a failed merge: %q", st)
	}
	if p.Status != "approved" {
		t.Fatalf("status = %q, want approved (unchanged)", p.Status)
	}
}

func TestDivergedProposalReturnsToReviewThroughRebase(t *testing.T) {
	s, p, path := approvedProposal(t, grantAllRoles)
	ctx := context.Background()
	// Someone commits to the same file after approval.
	if err := os.WriteFile(path, []byte("# changed upstream\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, s.okfRoot, "commit", "-q", "-am", "upstream")
	if _, err := s.mergeProposal(ctx, p.ID, "carol"); err == nil {
		t.Fatal("merge against a moved base must fail")
	}
	if !p.Diverged || p.Status != "approved" {
		t.Fatalf("divergence not recorded: diverged=%v status=%q", p.Diverged, p.Status)
	}
	if _, err := s.rebaseProposal(ctx, p.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if p.Status != "diffed" || p.Diverged || p.Reviewer != "" || p.Decision != "" {
		t.Fatalf("rebase must discard the stale review: %#v", p)
	}
	if !strings.Contains(p.Diff, "changed upstream") {
		t.Fatalf("diff must be against the new authoritative content: %q", p.Diff)
	}
	if _, err := s.decideProposal(ctx, p.ID, true, "bob", "knowledge_curator", ""); err == nil {
		t.Fatal("approval without a fresh review must be refused")
	}
	if _, err := s.rebaseProposal(ctx, p.ID, "alice"); err == nil {
		t.Fatal("a proposal that has not diverged cannot be rebased")
	}
}

func TestSyncImportsOnlyCommittedContentWithPerFileCommits(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main")
	runGit(t, root, "config", "user.email", "t@example.invalid")
	runGit(t, root, "config", "user.name", "t")
	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a.md", "---\nowner: platform\nstatus: approved\n---\n# A\n")
	runGit(t, root, "add", "a.md")
	runGit(t, root, "commit", "-q", "-m", "a")
	aCommit := strings.TrimSpace(string(runGit(t, root, "rev-parse", "HEAD")))
	b := write("b.md", "# B\n")
	runGit(t, root, "add", "b.md")
	runGit(t, root, "commit", "-q", "-m", "b")

	s := &okfServer{okfRoot: root, statePath: filepath.Join(t.TempDir(), "p.json"),
		documents: map[string]*OKFDocument{}, projections: map[string]*IndexProjection{}}
	if res := s.synchronise(context.Background()); len(res.Errors) != 0 {
		t.Fatalf("sync errors: %v", res.Errors)
	}
	docA := s.documentForPath(a)
	if docA == nil || docA.CommitID != aCommit {
		t.Fatalf("a.md must carry the commit that last touched it (%s), got %#v", aCommit, docA)
	}
	if docA.Metadata["owner"] != "platform" || docA.Metadata["status"] != "approved" {
		t.Fatalf("front-matter metadata not extracted: %v", docA.Metadata)
	}

	// An uncommitted edit is not approved content: it must not replace the index.
	write("b.md", "# B edited but not committed\n")
	res := s.synchronise(context.Background())
	if !contains(res.Diverged, b) {
		t.Fatalf("uncommitted edit must be surfaced as diverged: %#v", res)
	}
	if doc := s.documentForPath(b); doc == nil || strings.Contains(doc.Content, "not committed") {
		t.Fatalf("uncommitted content was imported: %#v", doc)
	}

	// A committed deletion is reported as removed (and kept for review).
	runGit(t, root, "checkout", "-q", "--", "b.md")
	runGit(t, root, "rm", "-q", "b.md")
	runGit(t, root, "commit", "-q", "-m", "rm b")
	res = s.synchronise(context.Background())
	if !contains(res.Removed, b) || s.documentForPath(b) == nil {
		t.Fatalf("deletion must be reported as removed while the record is preserved: %#v", res)
	}
}

func TestSyncOutsideGitReportsMissingAuthority(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &okfServer{okfRoot: root, statePath: filepath.Join(t.TempDir(), "p.json"),
		documents: map[string]*OKFDocument{}, projections: map[string]*IndexProjection{}}
	res := s.synchronise(context.Background())
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0], "not a git repository") {
		t.Fatalf("a non-git OKF root must be reported, got %v", res.Errors)
	}
}

func TestExtractMetadata(t *testing.T) {
	if m := extractMetadata("x.md", []byte("# no front matter\nkey: value\n")); m != nil {
		t.Fatalf("body text is not front matter: %v", m)
	}
	m := extractMetadata("x.yaml", []byte("name: routing\nlist:\n  - a\n# comment\nversion: \"2\"\n"))
	if m["name"] != "routing" || m["version"] != "2" || len(m) != 2 {
		t.Fatalf("yaml top-level scalars: %v", m)
	}
}
