package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// staticRoles stands in for okf_governance_roles: it returns the active
// roles assigned to each principal and nothing for anyone else.
func staticRoles(assignments map[string][]string) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, principal string) ([]string, error) {
		return assignments[principal], nil
	}
}

// TestDecideProposalRejectsSelfApproval guards against a regression of the
// separation-of-duties bug the workflow-state-machine auditor confirmed
// (F6): reviewProposalAuthorized already rejects reviewer == p.Proposer at
// the 'review' step, but decideProposal (the step that actually flips a
// proposal to "approved" and gates mergeProposal) previously had no
// equivalent check. Here alice also holds a curator role, so only the
// identity check stands between her and approving her own change.
func TestDecideProposalRejectsSelfApproval(t *testing.T) {
	s := &okfServer{
		proposals: map[string]*OKFProposal{
			"p1": {
				ID:       "p1",
				Proposer: "alice",
				Reviewer: "bob",
				Status:   "under_review",
			},
		},
		governanceRoles: staticRoles(map[string][]string{
			"alice": {"knowledge_curator"},
			"bob":   {"knowledge_curator"},
		}),
	}

	_, err := s.decideProposal(context.Background(), "p1", true, "alice", "knowledge_curator", "self-approved")
	if err == nil {
		t.Fatal("expected decideProposal to reject a reviewer equal to the proposer, got nil error")
	}
	if proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("self-approval must be a 403-class refusal, got %v", err)
	}

	// The independent reviewer must still be able to approve.
	p, err := s.decideProposal(context.Background(), "p1", true, "bob", "knowledge_curator", "looks good")
	if err != nil {
		t.Fatalf("expected independent reviewer approval to succeed, got %v", err)
	}
	if p.Status != "approved" || p.Reviewer != "bob" {
		t.Fatalf("expected proposal approved by bob, got status=%q reviewer=%q", p.Status, p.Reviewer)
	}
}

// TestDecideProposalRequiresAnExplicitDecider covers the removed fallback:
// decideProposal used to substitute p.Reviewer when no reviewer was given,
// recording a decision under the identity of whoever claimed the review.
func TestDecideProposalRequiresAnExplicitDecider(t *testing.T) {
	s := &okfServer{
		proposals: map[string]*OKFProposal{
			"p2": {ID: "p2", Proposer: "carol", Reviewer: "dave", Status: "under_review"},
		},
		governanceRoles: staticRoles(map[string][]string{"dave": {"knowledge_curator"}}),
	}
	if _, err := s.decideProposal(context.Background(), "p2", true, "", "", ""); err == nil {
		t.Fatal("expected decideProposal to refuse a decision with no authenticated decider")
	}
	if s.proposals["p2"].Status != "under_review" {
		t.Fatalf("refused decision changed status to %q", s.proposals["p2"].Status)
	}
}

// TestResolveReviewRoleUsesOnlyServerSideAssignments proves the role a
// reviewer acts under comes from the role store: a requested role the
// principal does not hold is refused, an unrequested role is chosen from
// held roles, and system principals can never review.
func TestResolveReviewRoleUsesOnlyServerSideAssignments(t *testing.T) {
	s := &okfServer{governanceRoles: staticRoles(map[string][]string{
		"erin":                  {"auditor", "memory_steward"},
		"system:internal-thing": {"knowledge_curator"},
	})}
	ctx := context.Background()
	if role, err := s.resolveReviewRole(ctx, "erin", ""); err != nil || role != "memory_steward" {
		t.Fatalf("expected erin's highest-precedence held role memory_steward, got %q, %v", role, err)
	}
	if role, err := s.resolveReviewRole(ctx, "erin", "auditor"); err != nil || role != "auditor" {
		t.Fatalf("expected a held requested role to be honoured, got %q, %v", role, err)
	}
	if _, err := s.resolveReviewRole(ctx, "erin", "knowledge_curator"); proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("a requested role the principal does not hold must be refused, got %v", err)
	}
	if _, err := s.resolveReviewRole(ctx, "frank", ""); proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("a principal with no governance role must be refused, got %v", err)
	}
	if _, err := s.resolveReviewRole(ctx, "system:internal-thing", ""); proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("a system principal must never hold review authority, got %v", err)
	}
	if _, err := (&okfServer{}).resolveReviewRole(ctx, "erin", ""); err == nil {
		t.Fatal("a server without a role store must authorise nobody")
	}
}

const (
	testIssuerSecret = "okf-test-issuer-secret"
	testStaticToken  = "okf-test-static-service-token"
)

// proposalHarness drives the real HTTP handlers behind the real
// authentication middleware, so identity reaches them only the way it does
// in production: through a signed bearer token.
type proposalHarness struct {
	t       *testing.T
	server  *okfServer
	handler http.Handler
}

func newProposalHarness(t *testing.T, roles map[string][]string) *proposalHarness {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "policy.md"), []byte("# old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &okfServer{
		documents:       map[string]*OKFDocument{},
		projections:     map[string]*IndexProjection{},
		proposals:       map[string]*OKFProposal{},
		okfRoot:         root,
		statePath:       filepath.Join(t.TempDir(), "projections.json"),
		governanceRoles: staticRoles(roles),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/proposals", s.handleProposals)
	mux.HandleFunc("/v1/proposals/", s.handleProposalAction)
	config := auth.Config{Token: testStaticToken, Identity: "okf-synchroniser", IssuerSecret: testIssuerSecret}
	return &proposalHarness{t: t, server: s, handler: auth.AuthenticatedHandler(mux, config)}
}

// tokenFor signs a claims token for principal; "" yields the shared static
// service token, which authenticates but carries no principal.
func (h *proposalHarness) tokenFor(principal string) string {
	h.t.Helper()
	if principal == "" {
		return testStaticToken
	}
	token, err := auth.SignClaims(auth.Claims{TenantID: "tenant-1", PrincipalID: principal, ExpiresAt: time.Now().Add(time.Hour)}, testIssuerSecret)
	if err != nil {
		h.t.Fatal(err)
	}
	return token
}

func (h *proposalHarness) post(principal, path string, body map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	req.Header.Set("Authorization", "Bearer "+h.tokenFor(principal))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *proposalHarness) expect(rec *httptest.ResponseRecorder, status int, what string) {
	h.t.Helper()
	if rec.Code != status {
		h.t.Fatalf("%s: got %d %q, want %d", what, rec.Code, strings.TrimSpace(rec.Body.String()), status)
	}
}

// createDiffed opens a proposal as principal and advances it through validate
// and diff, returning its id in the diffed state.
func (h *proposalHarness) createDiffed(principal string) string {
	h.t.Helper()
	rec := h.post(principal, "/v1/proposals", map[string]string{"path": "policy.md", "content": "# new\n"})
	h.expect(rec, http.StatusOK, "create proposal")
	var p OKFProposal
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		h.t.Fatal(err)
	}
	h.expect(h.post(principal, "/v1/proposals/"+p.ID+"/validate", nil), http.StatusOK, "validate")
	h.expect(h.post(principal, "/v1/proposals/"+p.ID+"/diff", nil), http.StatusOK, "diff")
	return p.ID
}

func TestProposalProposerComesFromAuthenticatedClaims(t *testing.T) {
	h := newProposalHarness(t, nil)
	id := h.createDiffed("alice")
	if got := h.server.proposals[id].Proposer; got != "alice" {
		t.Fatalf("proposer = %q, want the token principal alice", got)
	}

	// A body proposer naming someone else is refused, not recorded.
	rec := h.post("alice", "/v1/proposals", map[string]string{"path": "policy.md", "content": "# other\n", "proposer": "bob"})
	h.expect(rec, http.StatusForbidden, "claims caller asserting another proposer")
	// Agreeing with the token is accepted.
	h.expect(h.post("alice", "/v1/proposals", map[string]string{"path": "policy.md", "content": "# other\n", "proposer": "alice"}), http.StatusOK, "matching proposer")

	// The anonymous static token may not author a proposal as a person...
	h.expect(h.post("", "/v1/proposals", map[string]string{"path": "policy.md", "content": "# s\n", "proposer": "alice"}), http.StatusForbidden, "static token naming a human proposer")
	// ...but may as a system principal, which is how auto-promotion works.
	rec = h.post("", "/v1/proposals", map[string]string{"path": "policy.md", "content": "# s\n", "proposer": "system:memory-intelligence-auto-promotion"})
	h.expect(rec, http.StatusOK, "static token as system principal")
	var p OKFProposal
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Proposer != "system:memory-intelligence-auto-promotion" {
		t.Fatalf("system proposer = %q", p.Proposer)
	}
}

func TestProposalSelfApprovalIsRefusedOverHTTP(t *testing.T) {
	// alice is herself a curator, so only separation of duties stops her.
	h := newProposalHarness(t, map[string][]string{"alice": {"knowledge_curator"}, "bob": {"knowledge_curator"}})
	id := h.createDiffed("alice")

	h.expect(h.post("alice", "/v1/proposals/"+id+"/review", map[string]string{"reason": "mine"}), http.StatusForbidden, "proposer reviewing own proposal")
	h.expect(h.post("bob", "/v1/proposals/"+id+"/review", map[string]string{"reason": "taking it"}), http.StatusOK, "independent review")
	h.expect(h.post("alice", "/v1/proposals/"+id+"/approve", map[string]string{"reason": "ship it"}), http.StatusForbidden, "proposer approving own proposal")
	h.expect(h.post("alice", "/v1/proposals/"+id+"/reject", map[string]string{"reason": "nope"}), http.StatusForbidden, "proposer rejecting own proposal")
	if got := h.server.proposals[id].Status; got != "under_review" {
		t.Fatalf("refused self-decisions changed status to %q", got)
	}

	h.expect(h.post("bob", "/v1/proposals/"+id+"/approve", map[string]string{"reason": "reviewed"}), http.StatusOK, "independent approval")
	p := h.server.proposals[id]
	if p.Status != "approved" || p.Reviewer != "bob" || p.ReviewRole != "knowledge_curator" {
		t.Fatalf("approval not recorded under bob's held role: %#v", p)
	}
}

func TestProposalUnauthorisedPrincipalCannotReview(t *testing.T) {
	h := newProposalHarness(t, map[string][]string{"bob": {"knowledge_curator"}})
	id := h.createDiffed("alice")

	// mallory holds no governance role; asserting one in the body must not
	// grant it.
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/review", nil), http.StatusForbidden, "roleless review")
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/review", map[string]string{"role": "knowledge_curator"}), http.StatusForbidden, "roleless review claiming a role")
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/review", map[string]string{"role": "system_owner"}), http.StatusForbidden, "roleless review claiming system_owner")
	// The static service token carries no principal and can never review.
	h.expect(h.post("", "/v1/proposals/"+id+"/review", map[string]string{"reviewer": "bob", "role": "knowledge_curator"}), http.StatusForbidden, "static token review")

	h.expect(h.post("bob", "/v1/proposals/"+id+"/review", nil), http.StatusOK, "curator review")
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/approve", map[string]string{"role": "knowledge_curator"}), http.StatusForbidden, "roleless approval")
	if got := h.server.proposals[id].Status; got != "under_review" {
		t.Fatalf("unauthorised decision changed status to %q", got)
	}
}

func TestProposalBodyReviewerCannotImpersonate(t *testing.T) {
	h := newProposalHarness(t, map[string][]string{"bob": {"knowledge_curator"}, "mallory": {"auditor"}})
	id := h.createDiffed("alice")

	// mallory names bob as reviewer: refused, and nothing is recorded under
	// bob's name.
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/review", map[string]string{"reviewer": "bob", "role": "knowledge_curator"}), http.StatusForbidden, "body reviewer impersonation at review")
	if p := h.server.proposals[id]; p.Status != "diffed" || p.Reviewer != "" {
		t.Fatalf("impersonated review was recorded: %#v", p)
	}

	h.expect(h.post("bob", "/v1/proposals/"+id+"/review", nil), http.StatusOK, "bob reviews")
	// alice (the proposer, holding no role) tries to approve by naming bob.
	h.expect(h.post("alice", "/v1/proposals/"+id+"/approve", map[string]string{"reviewer": "bob"}), http.StatusForbidden, "body reviewer impersonation at approve")
	// mallory holds auditor but may not act under bob's curator role.
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/approve", map[string]string{"role": "knowledge_curator"}), http.StatusForbidden, "acting under a role not held")
	if got := h.server.proposals[id].Status; got != "under_review" {
		t.Fatalf("impersonated decision changed status to %q", got)
	}
}

// TestSynchroniseDetectsIndexedContentDivergence models documents hydrated
// by loadConceptsFromDB, where doc.ContentHash and the projection's
// ContentHash come from the same column and so always agree. Divergence
// must be judged on the indexed content itself.
func TestSynchroniseDetectsIndexedContentDivergence(t *testing.T) {
	newServer := func(t *testing.T, indexed string) (*okfServer, string) {
		root := t.TempDir()
		path := filepath.Join(root, "doc.md")
		if err := os.WriteFile(path, []byte("repository"), 0o600); err != nil {
			t.Fatal(err)
		}
		recorded := sha256Hex([]byte("repository"))
		s := &okfServer{
			documents:   map[string]*OKFDocument{"id": {ID: "id", Path: path, ContentHash: recorded, Content: indexed, Status: "current"}},
			projections: map[string]*IndexProjection{path: {Path: path, ContentHash: recorded, ImportStatus: "synced"}},
			okfRoot:     root,
			statePath:   filepath.Join(t.TempDir(), "projections.json"),
		}
		return s, path
	}

	t.Run("differs", func(t *testing.T) {
		s, path := newServer(t, "edited in the index")
		result := s.synchronise(context.Background())
		if len(result.Diverged) != 1 || result.Diverged[0] != path {
			t.Fatalf("expected %s reported diverged, got %#v", path, result)
		}
		if s.documents["id"].Status != "diverged" || s.projections[path].ImportStatus != "diverged" {
			t.Fatalf("divergence not marked: doc=%#v proj=%#v", s.documents["id"], s.projections[path])
		}
		if s.documents["id"].Content != "edited in the index" {
			t.Fatal("divergent indexed content must be preserved for review, not overwritten")
		}
	})

	t.Run("equal", func(t *testing.T) {
		s, path := newServer(t, "repository")
		result := s.synchronise(context.Background())
		if len(result.Diverged) != 0 {
			t.Fatalf("expected no divergence when indexed content matches, got %#v", result)
		}
		if s.documents["id"].Status != "current" || s.projections[path].ImportStatus != "synced" {
			t.Fatalf("matching document mis-marked: doc=%#v proj=%#v", s.documents["id"], s.projections[path])
		}
	})
}

// TestMergeMarksRootedDocumentDiverged covers merge-time divergence: the
// document and projection are keyed by the okfRoot-joined path, while the
// proposal stores the path relative to okfRoot, so the lookups previously
// never matched and the indexed document was never marked diverged.
func TestMergeMarksRootedDocumentDiverged(t *testing.T) {
	t.Setenv("SCOS_OKF_PROTECTED_BRANCH", "main")
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	path := filepath.Join(root, "policy.md")
	if err := os.WriteFile(path, []byte("# old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "policy.md")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	s := &okfServer{documents: map[string]*OKFDocument{}, projections: map[string]*IndexProjection{}, proposals: map[string]*OKFProposal{}, okfRoot: root, statePath: filepath.Join(t.TempDir(), "projections.json"),
		governanceRoles: staticRoles(map[string][]string{"carol": {"knowledge_curator"}})}
	s.synchronise(context.Background())
	p, err := s.createProposal(okfProposalRequest{Path: "policy.md", Content: "# new\n", Proposer: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	p.Status = "approved"
	// The working tree moves on under the approved proposal.
	if err := os.WriteFile(path, []byte("# edited outside the workflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mergeProposal(context.Background(), p.ID, "carol"); err == nil {
		t.Fatal("merge over a diverged working tree must fail")
	}
	if !p.Diverged {
		t.Fatal("proposal not flagged diverged")
	}
	if doc := s.documentForPath(path); doc == nil || doc.Status != "diverged" {
		t.Fatalf("indexed document not marked diverged: %#v", doc)
	}
	if proj := s.projections[path]; proj == nil || proj.ImportStatus != "diverged" {
		t.Fatalf("projection not marked diverged: %#v", proj)
	}
}

// approvedOverHTTP takes a proposal by alice through independent review and
// approval by bob, in a harness whose OKF root is a git repository so the
// protected-branch merge can actually commit.
func approvedOverHTTP(t *testing.T, roles map[string][]string) (*proposalHarness, string) {
	t.Helper()
	t.Setenv("SCOS_OKF_PROTECTED_BRANCH", "main")
	h := newProposalHarness(t, roles)
	root := h.server.okfRoot
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "add", "policy.md")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	id := h.createDiffed("alice")
	h.expect(h.post("bob", "/v1/proposals/"+id+"/review", nil), http.StatusOK, "review")
	h.expect(h.post("bob", "/v1/proposals/"+id+"/approve", map[string]string{"reason": "reviewed"}), http.StatusOK, "approve")
	return h, id
}

// TestMergeRequiresAMergeRole: an approved proposal is not mergeable by any
// token holder. The anonymous static token, a principal with no role, and
// a principal holding only review roles (auditor, memory_steward) are all
// refused, and the proposal stays approved with nothing committed.
func TestMergeRequiresAMergeRole(t *testing.T) {
	h, id := approvedOverHTTP(t, map[string][]string{
		"bob":     {"knowledge_curator"},
		"mallory": {"auditor", "memory_steward"},
	})
	before := gitCommit(h.server.okfRoot)
	h.expect(h.post("", "/v1/proposals/"+id+"/merge", nil), http.StatusForbidden, "static token merge")
	h.expect(h.post("eve", "/v1/proposals/"+id+"/merge", nil), http.StatusForbidden, "roleless merge")
	h.expect(h.post("mallory", "/v1/proposals/"+id+"/merge", nil), http.StatusForbidden, "review-only role merge")
	if p := h.server.proposals[id]; p.Status != "approved" || p.MergedBy != "" {
		t.Fatalf("refused merge changed the proposal: %#v", p)
	}
	if after := gitCommit(h.server.okfRoot); after != before {
		t.Fatalf("refused merge committed: %s -> %s", before, after)
	}
}

// TestMergeIsAttributedToTheMerger: the merge transition names the principal
// who merged and the role they merged under, not the reviewer.
func TestMergeIsAttributedToTheMerger(t *testing.T) {
	h, id := approvedOverHTTP(t, map[string][]string{
		"bob":   {"knowledge_curator"},
		"olive": {"system_owner"},
	})
	rec := h.post("olive", "/v1/proposals/"+id+"/merge", nil)
	h.expect(rec, http.StatusOK, "system owner merge")
	var p OKFProposal
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "indexed" || p.MergedBy != "olive" || p.MergeRole != "system_owner" || p.Reviewer != "bob" {
		t.Fatalf("merge not attributed to olive as system_owner: %#v", p)
	}
	if p.MergedCommit == "" || p.MergedCommit != gitCommit(h.server.okfRoot) {
		t.Fatalf("merge did not commit: %#v", p)
	}
}

// TestProtectedMergeCommitsOnlyTheProposalOntoTheProtectedBranch: the merge
// refuses a store checked out on another branch or carrying other staged
// changes, and a merge that succeeds is one commit on the protected branch
// whose parent is the reviewed base and which changes only the proposal's
// path.
func TestProtectedMergeCommitsOnlyTheProposalOntoTheProtectedBranch(t *testing.T) {
	h, id := approvedOverHTTP(t, map[string][]string{
		"bob":   {"knowledge_curator"},
		"olive": {"system_owner"},
	})
	root := h.server.okfRoot
	base := gitCommit(root)

	runGit(t, root, "checkout", "-q", "-b", "side")
	h.expect(h.post("olive", "/v1/proposals/"+id+"/merge", nil), http.StatusUnprocessableEntity, "merge off the protected branch")
	runGit(t, root, "checkout", "-q", "main")

	if err := os.WriteFile(filepath.Join(root, "other.md"), []byte("# unreviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "other.md")
	h.expect(h.post("olive", "/v1/proposals/"+id+"/merge", nil), http.StatusUnprocessableEntity, "merge with other staged changes")
	runGit(t, root, "reset", "-q", "--", "other.md")
	if gitCommit(root) != base {
		t.Fatal("a refused merge moved the protected branch")
	}

	h.expect(h.post("olive", "/v1/proposals/"+id+"/merge", nil), http.StatusOK, "merge")
	parent, err := gitIn(root, "rev-parse", "HEAD^")
	if err != nil || parent != base {
		t.Fatalf("merge commit parent = %q (%v), want the reviewed base %s", parent, err, base)
	}
	changed, err := gitIn(root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil || changed != "policy.md" {
		t.Fatalf("merge commit changed %q (%v), want only policy.md", changed, err)
	}
	if branch, _ := gitIn(root, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Fatalf("store left on %q", branch)
	}
}

func TestProposalDiffIsAUnifiedDiff(t *testing.T) {
	old := "# Policy\n\none\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	updated := "# Policy\n\none\ntwo\nthree\nFOUR\nfive\nsix\nseven\n"
	diff, err := makeProposalDiff("policy.md", old, updated)
	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/policy.md\n+++ b/policy.md\n@@ -3,7 +3,7 @@\n one\n two\n three\n-four\n+FOUR\n five\n six\n seven\n"
	if diff != want {
		t.Fatalf("diff =\n%s\nwant\n%s", diff, want)
	}
	if d, err := makeProposalDiff("policy.md", old, old); err != nil || d != "" {
		t.Fatalf("equal content diff = %q, %v", d, err)
	}
	created, err := makeProposalDiff("new.md", "", "# New\n")
	if err != nil || !strings.Contains(created, "@@ -0,0 +1 @@\n+# New\n") {
		t.Fatalf("new document diff = %q, %v", created, err)
	}
}
