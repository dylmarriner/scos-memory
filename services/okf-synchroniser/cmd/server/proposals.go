package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	secretscan "github.com/dylmarriner/scos-memory/packages/secret-scan"
)

var proposalStatuses = map[string]bool{
	"proposed": true, "validated": true, "diffed": true, "under_review": true,
	"approved": true, "rejected": true, "merged": true, "indexed": true,
}

type OKFProposal struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	BaseCommit   string `json:"base_commit"`
	BaseHash     string `json:"base_hash"`
	Content      string `json:"content"`
	Diff         string `json:"diff"`
	Status       string `json:"status"`
	Proposer     string `json:"proposer"`
	Reviewer     string `json:"reviewer,omitempty"`
	ReviewRole   string `json:"review_role,omitempty"`
	Decision     string `json:"decision,omitempty"`
	Reason       string `json:"reason,omitempty"`
	CreatedAt    string `json:"created_at"`
	ReviewedAt   string `json:"reviewed_at,omitempty"`
	MergedCommit string `json:"merged_commit,omitempty"`
	MergedBy     string `json:"merged_by,omitempty"`
	MergeRole    string `json:"merge_role,omitempty"`
	IndexedAt    string `json:"indexed_at,omitempty"`
	Diverged     bool   `json:"diverged,omitempty"`
}

// okfProposalRequest is the create-proposal body. Proposer is not an
// identity input: handleProposals overwrites it with the principal resolved
// by resolveProposer before createProposal sees it, and a body value that
// disagrees with the authenticated caller is refused with 403 rather than
// silently ignored, so a client that still sends one learns it was wrong.
type okfProposalRequest struct{ Path, Content, Proposer string }

// systemPrincipalPrefix namespaces the proposers an anonymous internal
// service (one presenting only the shared static token, which carries no
// claims) may attribute a proposal to. No system principal can hold a
// governance role (see resolveReviewRole), so a static-token holder can
// open proposals for curators to review but can never review, approve or
// reject one, nor author one in a human principal's name.
const (
	systemPrincipalPrefix = "system:"
	defaultSystemProposer = "system:internal-service"
)

// reviewRoles are the governance roles that may take and decide an OKF
// review, in the order one is chosen when the reviewer does not name one.
// knowledge_curator leads because it was the historical default.
var reviewRoles = []string{"knowledge_curator", "memory_steward", "system_owner", "auditor"}

// mergeRoles are the governance roles that may perform the protected-branch
// merge of an approved proposal, in the order one is chosen. Merging writes
// authoritative OKF content, so it is narrower than review: the roles that
// own curated knowledge and the system itself. An auditor or memory steward
// may review and decide, but may not change the authoritative branch.
var mergeRoles = []string{"knowledge_curator", "system_owner"}

// proposalAuthzError is a lifecycle refusal about who is asking rather than
// what state the proposal is in, so the HTTP layer answers with its status
// (401/403) instead of the generic 422 used for lifecycle violations.
type proposalAuthzError struct {
	status int
	msg    string
}

func (e *proposalAuthzError) Error() string { return e.msg }

func forbidden(msg string) error {
	return &proposalAuthzError{status: http.StatusForbidden, msg: msg}
}

func proposalErrorStatus(err error) int {
	var authzErr *proposalAuthzError
	if errors.As(err, &authzErr) {
		return authzErr.status
	}
	return http.StatusUnprocessableEntity
}

// resolveProposer derives the author of a new proposal from the
// authenticated request. A claims-bearing token is the proposer, full stop;
// an asserted body proposer must equal its principal_id. The shared static
// service token is authenticated but anonymous, so it may only attribute
// the proposal to a system: principal (memory-intelligence's auto-promotion
// does this) and defaults to defaultSystemProposer when it names none.
func resolveProposer(r *http.Request, asserted string) (string, error) {
	asserted = strings.TrimSpace(asserted)
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if asserted != "" && asserted != claims.PrincipalID {
			return "", forbidden("proposer does not match the authenticated principal")
		}
		return claims.PrincipalID, nil
	}
	if auth.Identity(r.Context()) == "" {
		return "", &proposalAuthzError{status: http.StatusUnauthorized, msg: "authentication required"}
	}
	if asserted == "" {
		return defaultSystemProposer, nil
	}
	if !strings.HasPrefix(asserted, systemPrincipalPrefix) {
		return "", forbidden("a token without principal claims may only propose as a " + systemPrincipalPrefix + " principal")
	}
	return asserted, nil
}

// resolveReviewer derives the reviewing principal from the authenticated
// request's claims. Review decisions are attributable acts, so the
// anonymous static service token is refused outright, and an asserted body
// reviewer that is not the caller is refused with 403 -- it is never used
// to choose whose authority the decision is recorded under.
func resolveReviewer(r *http.Request, asserted string) (string, error) {
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		return "", forbidden("review decisions require a bearer token carrying a principal_id claim")
	}
	if asserted = strings.TrimSpace(asserted); asserted != "" && asserted != claims.PrincipalID {
		return "", forbidden("reviewer does not match the authenticated principal")
	}
	return claims.PrincipalID, nil
}

func (s *okfServer) handleProposals(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req okfProposalRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid proposal", 400)
			return
		}
		proposer, err := resolveProposer(r, req.Proposer)
		if err != nil {
			http.Error(w, err.Error(), proposalErrorStatus(err))
			return
		}
		req.Proposer = proposer
		s.mu.Lock()
		p, err := s.createProposal(req)
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), 422)
			return
		}
		writeJSON(w, p)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.mu.RLock()
	out := make([]*OKFProposal, 0, len(s.proposals))
	for _, p := range s.proposals {
		out = append(out, p)
	}
	s.mu.RUnlock()
	writeJSON(w, out)
}

func (s *okfServer) handleProposalAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/proposals/"), "/")
	if len(parts) < 2 || parts[0] == "" {
		http.Error(w, "proposal action required", 400)
		return
	}
	id, action := parts[0], parts[1]
	if action == "merge" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The merger is the authenticated caller, never a body field and
		// never the reviewer recorded on the proposal: approval and merge
		// are separate privileged acts and each is attributed to whoever
		// performed it.
		merger, err := resolveReviewer(r, "")
		if err != nil {
			http.Error(w, err.Error(), proposalErrorStatus(err))
			return
		}
		s.mu.Lock()
		p, err := s.mergeProposal(r.Context(), id, merger)
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), proposalErrorStatus(err))
			return
		}
		result := s.synchronise(r.Context())
		// synchronise reports okfRoot-joined paths (walkOKFDir), while
		// p.Path is relative to okfRoot, so compare like with like.
		if contains(result.Diverged, mustProposalPath(s, p.Path)) {
			http.Error(w, "post-merge indexing detected divergence", 422)
			return
		}
		s.mu.Lock()
		p.Status = "indexed"
		p.IndexedAt = time.Now().UTC().Format(time.RFC3339)
		err = s.persistProposal(r.Context(), p, "indexed", p.MergedBy, p.MergeRole, "post-merge index")
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, p)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var p *OKFProposal
	var err error
	switch action {
	case "validate":
		p, err = s.validateProposalContext(r.Context(), id)
	case "diff":
		p, err = s.diffProposal(r.Context(), id)
	case "review", "approve", "reject":
		// Reviewer is an assertion checked against the token, never an
		// input; Role only selects among roles the caller already holds in
		// okf_governance_roles (see resolveReviewRole).
		var req struct{ Reviewer, Role, Reason string }
		var reviewer string
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); e != nil {
			err = e
		} else if reviewer, err = resolveReviewer(r, req.Reviewer); err != nil {
			break
		} else if action == "review" {
			p, err = s.reviewProposalAuthorized(r.Context(), id, reviewer, req.Role, req.Reason)
		} else {
			p, err = s.decideProposal(r.Context(), id, action == "approve", reviewer, req.Role, req.Reason)
		}
	default:
		err = errors.New("unknown proposal action")
	}
	if err != nil {
		http.Error(w, err.Error(), proposalErrorStatus(err))
		return
	}
	writeJSON(w, p)
}

func (s *okfServer) proposalPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if clean == "." || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("proposal path must remain inside OKF")
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if ext != ".md" && ext != ".yaml" && ext != ".yml" {
		return "", errors.New("proposal must target markdown or yaml")
	}
	return filepath.Join(s.okfRoot, clean), nil
}

func (s *okfServer) createProposal(req okfProposalRequest) (*OKFProposal, error) {
	if strings.TrimSpace(req.Proposer) == "" || req.Content == "" {
		return nil, errors.New("proposer and content are required")
	}
	// Write-time secret/PII gate (P0-B), fail-closed here unlike
	// memory-intelligence's redact-and-flag: OKF content is meant to become
	// broadly readable/shared curated knowledge, so a leaked secret here is
	// worse, not better, than in raw conversational memory -- reject the
	// proposal rather than silently store a redacted "authoritative" doc.
	if findings := secretscan.Scan(req.Content); len(findings) > 0 {
		return nil, fmt.Errorf("proposal content contains %d likely secret/PII match(es) (e.g. %s); remove before submitting", len(findings), findings[0].Kind)
	}
	path, err := s.proposalPath(req.Path)
	if err != nil {
		return nil, err
	}
	base, _ := os.ReadFile(path)
	diff, err := makeProposalDiff(req.Path, string(base), req.Content)
	if err != nil {
		return nil, err
	}
	p := &OKFProposal{ID: newProposalID(), Path: req.Path, BaseCommit: gitCommit(s.okfRoot), BaseHash: sha256Hex(base), Content: req.Content, Diff: diff, Status: "proposed", Proposer: req.Proposer, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if p.Diff == "" {
		return nil, errors.New("proposal does not change the authoritative content")
	}
	if s.proposals == nil {
		s.proposals = map[string]*OKFProposal{}
	}
	s.proposals[p.ID] = p
	if err := s.persistProposal(context.Background(), p, "proposed", req.Proposer, "", ""); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *okfServer) validateProposal(id string) (*OKFProposal, error) {
	return s.validateProposalContext(context.Background(), id)
}

func (s *okfServer) validateProposalContext(ctx context.Context, id string) (*OKFProposal, error) {
	p, err := s.proposal(id, "proposed")
	if err != nil {
		return nil, err
	}
	if _, err = s.proposalPath(p.Path); err != nil {
		return nil, err
	}
	if p.Content == "" {
		return nil, errors.New("proposal content is empty")
	}
	p.Status = "validated"
	if err = s.persistProposal(ctx, p, "validated", p.Proposer, "", ""); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *okfServer) diffProposal(ctx context.Context, id string) (*OKFProposal, error) {
	p, err := s.proposal(id, "validated")
	if err != nil {
		return nil, err
	}
	diff, err := makeProposalDiff(p.Path, string(readFileOrEmpty(mustProposalPath(s, p.Path))), p.Content)
	if err != nil {
		return nil, err
	}
	p.Diff = diff
	if p.Diff == "" {
		return nil, errors.New("proposal has no diff")
	}
	p.Status = "diffed"
	if err = s.persistProposal(ctx, p, "diffed", p.Proposer, "", ""); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *okfServer) reviewProposal(id, reviewer string) (*OKFProposal, error) {
	// Compatibility for callers of the original in-process API: the explicit
	// HTTP lifecycle still requires /diff, while this helper performs that
	// deterministic step before review.
	if p := s.proposals[id]; p != nil && p.Status == "validated" {
		if _, err := s.diffProposal(context.Background(), id); err != nil {
			return nil, err
		}
	}
	p, err := s.reviewProposalAuthorized(context.Background(), id, reviewer, "", "")
	// Preserve the legacy in-process method's response shape. HTTP callers use
	// the normative under_review state and the durable transition above.
	if err == nil {
		p.Status = "reviewed"
	}
	return p, err
}
func (s *okfServer) reviewProposalAuthorized(ctx context.Context, id, reviewer, role, reason string) (*OKFProposal, error) {
	p, err := s.proposal(id, "diffed")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reviewer) == "" || reviewer == p.Proposer {
		return nil, forbidden("independent authorised review is required")
	}
	if role, err = s.resolveReviewRole(ctx, reviewer, role); err != nil {
		return nil, err
	}
	p.Status = "under_review"
	p.Reviewer = reviewer
	p.ReviewRole = role
	p.Reason = reason
	p.ReviewedAt = time.Now().UTC().Format(time.RFC3339)
	if err = s.persistProposal(ctx, p, "under_review", reviewer, role, reason); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *okfServer) decideProposal(ctx context.Context, id string, approved bool, reviewer, role, reason string) (*OKFProposal, error) {
	p, err := s.proposal(id, "under_review")
	if err != nil {
		return nil, err
	}
	// The decider is whoever the HTTP layer authenticated (resolveReviewer);
	// there is deliberately no fallback to p.Reviewer, because recording a
	// decision under the identity of the principal who merely claimed the
	// review would let any token holder decide in that curator's name.
	// reviewProposalAuthorized rejects reviewer == p.Proposer at the 'review'
	// step, but that only binds p.Reviewer, so the decision step repeats the
	// separation-of-duties check against its own caller.
	if strings.TrimSpace(reviewer) == "" || reviewer == p.Proposer {
		return nil, forbidden("independent authorised review is required")
	}
	if role, err = s.resolveReviewRole(ctx, reviewer, role); err != nil {
		return nil, err
	}
	p.Reviewer = reviewer
	p.ReviewRole = role
	p.Reason = reason
	p.Decision = map[bool]string{true: "approved", false: "rejected"}[approved]
	p.Status = p.Decision
	if err = s.persistProposal(ctx, p, p.Status, reviewer, role, reason); err != nil {
		return nil, err
	}
	return p, nil
}

// mergeProposal performs the protected-branch merge of an approved proposal
// as merger, who must hold a governance role in mergeRoles. Every transition
// it records -- the merge itself and any divergence it finds -- names the
// merger and the role they merged under, not the reviewer or proposer.
func (s *okfServer) mergeProposal(ctx context.Context, id, merger string) (*OKFProposal, error) {
	p, err := s.proposal(id, "approved")
	if err != nil {
		return nil, err
	}
	role, err := s.resolveMergeRole(ctx, merger)
	if err != nil {
		return nil, err
	}
	branch := protectedBranch()
	if head, err := gitIn(s.okfRoot, "symbolic-ref", "--short", "HEAD"); err != nil || head != branch {
		return nil, fmt.Errorf("the OKF store is not on the protected branch %s; refusing to merge", branch)
	}
	path, err := s.proposalPath(p.Path)
	if err != nil {
		return nil, err
	}
	if gitCommit(s.okfRoot) != p.BaseCommit {
		p.Diverged = true
		_ = s.persistProposal(ctx, p, p.Status, merger, role, "authoritative commit diverged; explicit re-review required")
		return nil, errors.New("authoritative branch changed; divergence requires review")
	}
	current, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if sha256Hex(current) != p.BaseHash {
		p.Diverged = true
		// Documents and projections are keyed by the okfRoot-joined path.
		if doc := s.documentForPath(path); doc != nil {
			doc.Status = "diverged"
		}
		if proj := s.projections[path]; proj != nil {
			proj.ImportStatus = "diverged"
		}
		_ = s.persistProposal(ctx, p, p.Status, merger, role, "working tree diverged; explicit re-review required")
		return nil, errors.New("working tree diverged; explicit re-review required")
	}
	// The merge commits the index, so anything else staged in the store
	// would ride along unreviewed.
	if staged, e := gitIn(s.okfRoot, "diff", "--cached", "--name-only"); e != nil || staged != "" {
		return nil, fmt.Errorf("the OKF store has staged changes outside this proposal (%s); refusing to merge", staged)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err = os.WriteFile(path, []byte(p.Content), 0600); err != nil {
		return nil, err
	}
	// Only the proposal's own path is committed, and the commit lands on the
	// protected branch only if that branch still points at the reviewed base
	// (a compare-and-swap on the ref); anything else restores the file.
	restore := func() {
		if readErr != nil {
			_ = os.Remove(path)
		} else {
			_ = os.WriteFile(path, current, 0600)
		}
		_, _ = gitIn(s.okfRoot, "reset", "-q", "--", p.Path)
	}
	if out, e := gitIn(s.okfRoot, "add", "--", p.Path); e != nil {
		restore()
		return nil, fmt.Errorf("stage proposal: %s", out)
	}
	tree, e := gitIn(s.okfRoot, "write-tree")
	if e != nil {
		restore()
		return nil, fmt.Errorf("protected merge: write tree: %s", tree)
	}
	commit, e := gitIn(s.okfRoot, "-c", "user.name=SCOS OKF", "-c", "user.email=scos-okf@localhost",
		"commit-tree", tree, "-p", p.BaseCommit, "-m", fmt.Sprintf("OKF proposal %s: %s\n\nProposed-by: %s\nReviewed-by: %s (%s)\nMerged-by: %s (%s)", p.ID, p.Path, p.Proposer, p.Reviewer, p.ReviewRole, merger, role))
	if e != nil {
		restore()
		return nil, fmt.Errorf("protected merge: commit: %s", commit)
	}
	if out, e := gitIn(s.okfRoot, "update-ref", "-m", "OKF proposal "+p.ID, "refs/heads/"+branch, commit, p.BaseCommit); e != nil {
		restore()
		p.Diverged = true
		_ = s.persistProposal(ctx, p, p.Status, merger, role, "authoritative commit moved during merge; explicit re-review required")
		return nil, fmt.Errorf("protected merge: %s moved during the merge: %s", branch, out)
	}
	p.Status = "merged"
	p.MergedCommit = gitCommit(s.okfRoot)
	p.MergedBy = merger
	p.MergeRole = role
	if err = s.persistProposal(ctx, p, "merged", merger, role, "protected-branch merge of approved proposal"); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *okfServer) proposal(id, status string) (*OKFProposal, error) {
	p := s.proposals[id]
	if p == nil {
		return nil, errors.New("proposal not found")
	}
	if p.Status != status {
		return nil, fmt.Errorf("proposal is not %s", status)
	}
	return p, nil
}

// resolveReviewRole establishes, from okf_governance_roles alone, the role a
// reviewer acts under. A requested role is only a selector: it must be a
// review role the principal actively holds, otherwise the request is
// refused (403) rather than recorded under an authority the caller lacks.
// With no request the first held role in reviewRoles order is used.
func (s *okfServer) resolveReviewRole(ctx context.Context, principal, requested string) (string, error) {
	if strings.HasPrefix(principal, systemPrincipalPrefix) {
		return "", forbidden("system principals cannot hold a governance role")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" && !contains(reviewRoles, requested) {
		return "", forbidden("unsupported governance role")
	}
	held, err := s.activeGovernanceRoles(ctx, principal)
	if err != nil {
		return "", err
	}
	if requested != "" {
		if !contains(held, requested) {
			return "", forbidden("principal is not assigned the required governance role")
		}
		return requested, nil
	}
	for _, role := range reviewRoles {
		if contains(held, role) {
			return role, nil
		}
	}
	return "", forbidden("principal is not assigned a governance role that may review OKF proposals")
}

// resolveMergeRole establishes, from okf_governance_roles alone, the role a
// principal merges under: the first role in mergeRoles they actively hold.
// System principals and principals holding only review roles are refused.
func (s *okfServer) resolveMergeRole(ctx context.Context, principal string) (string, error) {
	if strings.TrimSpace(principal) == "" {
		return "", forbidden("merging requires an authenticated principal")
	}
	if strings.HasPrefix(principal, systemPrincipalPrefix) {
		return "", forbidden("system principals cannot hold a governance role")
	}
	held, err := s.activeGovernanceRoles(ctx, principal)
	if err != nil {
		return "", err
	}
	for _, role := range mergeRoles {
		if contains(held, role) {
			return role, nil
		}
	}
	return "", forbidden("principal is not assigned a governance role that may merge OKF proposals (" + strings.Join(mergeRoles, ", ") + ")")
}

// activeGovernanceRoles reads the principal's active roles. It fails closed:
// a server with neither a role source nor a database authorises nobody.
func (s *okfServer) activeGovernanceRoles(ctx context.Context, principal string) ([]string, error) {
	if s.governanceRoles != nil {
		return s.governanceRoles(ctx, principal)
	}
	if s.db == nil {
		return nil, errors.New("governance role store is not configured")
	}
	rows, err := s.db.Query(ctx, `SELECT role FROM okf_governance_roles WHERE principal_id=$1 AND active`, principal)
	if err != nil {
		return nil, fmt.Errorf("check governance role: %w", err)
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("check governance role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check governance role: %w", err)
	}
	return roles, nil
}
func (s *okfServer) persistProposal(ctx context.Context, p *OKFProposal, status, actor, role, reason string) error {
	if s.db == nil {
		return nil
	}
	var previous string
	_ = s.db.QueryRow(ctx, `SELECT status FROM okf_proposals WHERE id=$1`, p.ID).Scan(&previous)
	if _, err := s.db.Exec(ctx, `INSERT INTO okf_proposals (id,path,base_commit,base_hash,content,diff,status,proposer,reviewer,review_role,decision,reason,created_at,reviewed_at,merged_commit,indexed_at,diverged,merged_by,merge_role) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12,$13::timestamptz,NULLIF($14,'')::timestamptz,NULLIF($15,''),NULLIF($16,'')::timestamptz,$17,NULLIF($18,''),NULLIF($19,'')) ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,reviewer=EXCLUDED.reviewer,review_role=EXCLUDED.review_role,decision=EXCLUDED.decision,reason=EXCLUDED.reason,reviewed_at=EXCLUDED.reviewed_at,merged_commit=EXCLUDED.merged_commit,indexed_at=EXCLUDED.indexed_at,diverged=EXCLUDED.diverged,merged_by=EXCLUDED.merged_by,merge_role=EXCLUDED.merge_role`, p.ID, p.Path, p.BaseCommit, p.BaseHash, p.Content, p.Diff, status, p.Proposer, p.Reviewer, p.ReviewRole, p.Decision, reason, p.CreatedAt, p.ReviewedAt, p.MergedCommit, p.IndexedAt, p.Diverged, p.MergedBy, p.MergeRole); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO okf_proposal_transitions(proposal_id,from_status,to_status,actor,governance_role,reason) VALUES($1,NULLIF($2,''),$3,$4,NULLIF($5,''),$6)`, p.ID, previous, status, actor, role, reason)
	return err
}
func newProposalID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "okf-proposal-" + hex.EncodeToString(b)
}
// defaultProtectedBranch is the branch the OKF store is initialised on and
// the only branch approved proposals are merged into, unless
// SCOS_OKF_PROTECTED_BRANCH names another.
const defaultProtectedBranch = "main"

func protectedBranch() string {
	if b := strings.TrimSpace(os.Getenv("SCOS_OKF_PROTECTED_BRANCH")); b != "" {
		return b
	}
	return defaultProtectedBranch
}

// makeProposalDiff is the unified diff (three lines of context) from the
// authoritative content of path to the proposed content, as git computes
// it; "" when they are equal.
func makeProposalDiff(path, old, new string) (string, error) {
	if old == new {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "okf-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte(old), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte(new), 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "diff", "--no-index", "--no-color", "--no-ext-diff", "-U3", "--", "a", "b")
	cmd.Dir = dir
	out, err := cmd.Output()
	// git diff --no-index exits 1 when the files differ.
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", fmt.Errorf("diff: %w", err)
	}
	// Drop git's own header lines and name both sides by the document path.
	lines := strings.SplitAfter(string(out), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "@@") {
			return "--- a/" + path + "\n+++ b/" + path + "\n" + strings.Join(lines[i:], ""), nil
		}
	}
	return "", errors.New("diff: git produced no hunks for differing content")
}
func readFileOrEmpty(path string) []byte             { b, _ := os.ReadFile(path); return b }
func mustProposalPath(s *okfServer, p string) string { x, _ := s.proposalPath(p); return x }
func contains(xs []string, w string) bool {
	for _, x := range xs {
		if x == w {
			return true
		}
	}
	return false
}
