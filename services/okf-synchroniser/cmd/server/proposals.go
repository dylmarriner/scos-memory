package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	secretscan "github.com/dylmarriner/scos-memory/packages/secret-scan"
)

// authenticatedPrincipal is the principal proven by the caller's signed
// claims token. Governance decisions are attributed to it -- never to a
// name supplied in the request body, which any token holder could set.
func authenticatedPrincipal(r *http.Request) (string, bool) {
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok || strings.TrimSpace(claims.PrincipalID) == "" {
		return "", false
	}
	return claims.PrincipalID, true
}

// actingPrincipal requires a claims-bearing caller and rejects a body-named
// actor that differs from it.
func actingPrincipal(r *http.Request, named string) (string, error) {
	principal, ok := authenticatedPrincipal(r)
	if !ok {
		return "", errors.New("this governance action requires an authenticated principal (claims token)")
	}
	if named != "" && named != principal {
		return "", errors.New("the acting principal must be the authenticated caller")
	}
	return principal, nil
}

// proposerFor attributes a new proposal. A person proposes as themself; a
// service credential carries no principal and may only propose under a
// "system:" identity (e.g. memory-intelligence's §26.1 auto-promotion), so
// it can never impersonate a human proposer or reviewer.
func proposerFor(r *http.Request, named string) (string, error) {
	if principal, ok := authenticatedPrincipal(r); ok {
		if named != "" && named != principal {
			return "", errors.New("the proposer must be the authenticated caller")
		}
		return principal, nil
	}
	if !strings.HasPrefix(named, "system:") {
		return "", errors.New("a service credential may only propose under a system: identity")
	}
	return named, nil
}

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
	IndexedAt    string `json:"indexed_at,omitempty"`
	Diverged     bool   `json:"diverged,omitempty"`
}

type okfProposalRequest struct{ Path, Content, Proposer, Reviewer string }

func (s *okfServer) handleProposals(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req okfProposalRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid proposal", 400)
			return
		}
		proposer, err := proposerFor(r, req.Proposer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
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
		merger, err := actingPrincipal(r, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		s.mu.Lock()
		p, err := s.mergeProposal(r.Context(), id, merger)
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), 422)
			return
		}
		result := s.synchronise(r.Context())
		if contains(result.Diverged, p.Path) {
			http.Error(w, "post-merge indexing detected divergence", 422)
			return
		}
		s.mu.Lock()
		p.Status = "indexed"
		p.IndexedAt = time.Now().UTC().Format(time.RFC3339)
		err = s.persistProposal(r.Context(), p, "indexed", p.Reviewer, p.ReviewRole, "post-merge index")
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
		var req struct{ Reviewer, Role, Reason string }
		var reviewer string
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); e != nil {
			err = e
		} else if reviewer, err = actingPrincipal(r, req.Reviewer); err != nil {
			// attributed to the authenticated caller only
		} else if action == "review" {
			p, err = s.reviewProposalAuthorized(r.Context(), id, reviewer, req.Role, req.Reason)
		} else {
			p, err = s.decideProposal(r.Context(), id, action == "approve", reviewer, req.Role, req.Reason)
		}
	case "rebase":
		var actor string
		if actor, err = actingPrincipal(r, ""); err == nil {
			p, err = s.rebaseProposal(r.Context(), id, actor)
		}
	default:
		err = errors.New("unknown proposal action")
	}
	if err != nil {
		http.Error(w, err.Error(), 422)
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
	p := &OKFProposal{ID: newProposalID(), Path: req.Path, BaseCommit: gitCommit(s.okfRoot), BaseHash: sha256Hex(base), Content: req.Content, Diff: makeProposalDiff(string(base), req.Content), Status: "proposed", Proposer: req.Proposer, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
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
	p.Diff = makeProposalDiff(string(readFileOrEmpty(mustProposalPath(s, p.Path))), p.Content)
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
		return nil, errors.New("independent authorised review is required")
	}
	if role == "" {
		role = "knowledge_curator"
	}
	if err = s.requireRole(ctx, reviewer, role); err != nil {
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
	if reviewer == "" {
		reviewer = p.Reviewer
	}
	if role == "" {
		role = p.ReviewRole
	}
	// reviewProposalAuthorized already rejects reviewer == p.Proposer at the
	// 'review' step, but that only binds p.Reviewer; a caller of the
	// 'decide' step could previously pass an explicit req.Reviewer equal to
	// p.Proposer and, provided that principal also held a governance role,
	// self-approve a proposal they authored -- defeating the
	// separation-of-duties invariant the review step exists to establish,
	// since decideProposal (unlike reviewProposalAuthorized) never checked
	// identity against the proposer, only role membership via requireRole.
	if strings.TrimSpace(reviewer) == "" || reviewer == p.Proposer {
		return nil, errors.New("independent authorised review is required")
	}
	if err = s.requireRole(ctx, reviewer, role); err != nil {
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

// mergeProposal commits an approved proposal to the protected branch as
// merger, who must be an authorised curator or owner other than the
// proposer (separation of duties: nobody merges their own change).
func (s *okfServer) mergeProposal(ctx context.Context, id, merger string) (*OKFProposal, error) {
	p, err := s.proposal(id, "approved")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(merger) == "" || merger == p.Proposer {
		return nil, errors.New("an authorised merger other than the proposer is required")
	}
	if err := s.requireAnyRole(ctx, merger, "system_owner", "knowledge_curator"); err != nil {
		return nil, err
	}
	protected := strings.TrimSpace(os.Getenv("SCOS_OKF_PROTECTED_BRANCH"))
	if protected == "" {
		return nil, errors.New("protected branch is not configured (SCOS_OKF_PROTECTED_BRANCH)")
	}
	// The configured name is only a claim; the merge lands on whatever is
	// checked out, so that is what must match.
	branch, err := s.git(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve checked-out branch: %w", err)
	}
	if branch != protected {
		return nil, fmt.Errorf("OKF checkout is on %q, not the protected branch %q", branch, protected)
	}
	// Uncommitted edits would be swept into, or shadow, the governed commit.
	if dirty, err := s.git(ctx, "status", "--porcelain"); err != nil {
		return nil, fmt.Errorf("check working tree: %w", err)
	} else if dirty != "" {
		return nil, errors.New("OKF working tree has uncommitted changes; the protected branch must be clean to merge")
	}
	path, err := s.proposalPath(p.Path)
	if err != nil {
		return nil, err
	}
	if gitCommit(s.okfRoot) != p.BaseCommit {
		return nil, s.markProposalDiverged(ctx, p, "authoritative commit diverged; rebase and re-review required")
	}
	current, readErr := os.ReadFile(path)
	if sha256Hex(current) != p.BaseHash {
		if doc := s.documentForPath(p.Path); doc != nil {
			doc.Status = "diverged"
		}
		if proj := s.projections[p.Path]; proj != nil {
			proj.ImportStatus = "diverged"
		}
		return nil, s.markProposalDiverged(ctx, p, "working tree diverged; rebase and re-review required")
	}
	existed := readErr == nil
	if err = os.WriteFile(path, []byte(p.Content), 0600); err != nil {
		return nil, err
	}
	// Any failure from here restores the file, so a failed merge never
	// leaves ungoverned content for the next sync to import.
	restore := func(cause error) error {
		_, _ = s.git(ctx, "reset", "-q", "--", p.Path)
		var rerr error
		if existed {
			rerr = os.WriteFile(path, current, 0600)
		} else {
			rerr = os.Remove(path)
		}
		if rerr != nil {
			return fmt.Errorf("%w (restoring %s also failed: %v)", cause, p.Path, rerr)
		}
		return cause
	}
	if _, err := s.git(ctx, "add", "--", p.Path); err != nil {
		return nil, restore(fmt.Errorf("stage proposal: %w", err))
	}
	author := fmt.Sprintf("--author=%s <%s@okf.scos.invalid>", merger, merger)
	if _, err := s.git(ctx, "-c", "user.name=SCOS OKF", "-c", "user.email=scos-okf@okf.scos.invalid",
		"commit", author, "-m", "OKF proposal "+p.ID, "-m", "Proposed-by: "+p.Proposer+"\nReviewed-by: "+p.Reviewer+"\nMerged-by: "+merger); err != nil {
		return nil, restore(fmt.Errorf("protected merge: %w", err))
	}
	p.Status = "merged"
	p.MergedCommit = gitCommit(s.okfRoot)
	if err = s.persistProposal(ctx, p, "merged", merger, p.ReviewRole, p.Reason); err != nil {
		return nil, err
	}
	return p, nil
}

// markProposalDiverged records that the proposal's base moved and returns
// the error the caller reports; the proposal leaves the approved state only
// through rebaseProposal, which forces a fresh review.
func (s *okfServer) markProposalDiverged(ctx context.Context, p *OKFProposal, reason string) error {
	p.Diverged = true
	if err := s.persistProposal(ctx, p, p.Status, p.Proposer, p.ReviewRole, reason); err != nil {
		return fmt.Errorf("%s; recording divergence failed: %w", reason, err)
	}
	return errors.New(reason)
}

// rebaseProposal re-bases a diverged proposal on the current authoritative
// content and returns it to "diffed": the earlier review approved a diff
// against content that no longer exists, so it is discarded and the change
// needs an independent review again (§26: divergence is marked and reviewed,
// never silently resolved).
func (s *okfServer) rebaseProposal(ctx context.Context, id, actor string) (*OKFProposal, error) {
	p := s.proposals[id]
	if p == nil {
		return nil, errors.New("proposal not found")
	}
	if !p.Diverged {
		return nil, errors.New("proposal has not diverged; nothing to rebase")
	}
	if p.Status == "merged" || p.Status == "indexed" || p.Status == "rejected" {
		return nil, fmt.Errorf("a %s proposal cannot be rebased", p.Status)
	}
	path, err := s.proposalPath(p.Path)
	if err != nil {
		return nil, err
	}
	base, _ := os.ReadFile(path)
	diff := makeProposalDiff(string(base), p.Content)
	if diff == "" {
		return nil, errors.New("the authoritative content already matches this proposal; nothing to re-review")
	}
	p.BaseCommit = gitCommit(s.okfRoot)
	p.BaseHash = sha256Hex(base)
	p.Diff = diff
	p.Diverged = false
	p.Status = "diffed"
	p.Reviewer, p.ReviewRole, p.Decision, p.ReviewedAt = "", "", "", ""
	if err := s.persistProposal(ctx, p, "diffed", actor, "", "rebased onto "+p.BaseCommit+"; independent re-review required"); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *okfServer) git(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", s.okfRoot}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", errors.New(text)
	}
	return text, nil
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
func (s *okfServer) requireRole(ctx context.Context, principal, role string) error {
	if role != "system_owner" && role != "knowledge_curator" && role != "auditor" && role != "memory_steward" {
		return errors.New("unsupported governance role")
	}
	var ok bool
	var err error
	switch {
	case s.roleLookup != nil:
		ok, err = s.roleLookup(ctx, principal, role)
	case s.db != nil:
		err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM okf_governance_roles WHERE principal_id=$1 AND role=$2 AND active)`, principal, role).Scan(&ok)
	default:
		// No role source means no authorisation evidence: fail closed.
		return errors.New("governance roles are unavailable; refusing unverified authorisation")
	}
	if err != nil {
		return fmt.Errorf("check governance role: %w", err)
	}
	if !ok {
		return errors.New("principal is not assigned the required governance role")
	}
	return nil
}

func (s *okfServer) requireAnyRole(ctx context.Context, principal string, roles ...string) error {
	var last error
	for _, role := range roles {
		if last = s.requireRole(ctx, principal, role); last == nil {
			return nil
		}
	}
	return fmt.Errorf("one of the %s roles is required: %w", strings.Join(roles, "/"), last)
}

func (s *okfServer) persistProposal(ctx context.Context, p *OKFProposal, status, actor, role, reason string) error {
	if s.db == nil {
		return nil
	}
	var previous string
	if err := s.db.QueryRow(ctx, `SELECT status FROM okf_proposals WHERE id=$1`, p.ID).Scan(&previous); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read previous proposal status: %w", err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO okf_proposals (id,path,base_commit,base_hash,content,diff,status,proposer,reviewer,review_role,decision,reason,created_at,reviewed_at,merged_commit,indexed_at,diverged) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12,$13,NULLIF($14,'')::timestamptz,NULLIF($15,''),NULLIF($16,'')::timestamptz,$17) ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,reviewer=EXCLUDED.reviewer,review_role=EXCLUDED.review_role,decision=EXCLUDED.decision,reason=EXCLUDED.reason,reviewed_at=EXCLUDED.reviewed_at,merged_commit=EXCLUDED.merged_commit,indexed_at=EXCLUDED.indexed_at,diverged=EXCLUDED.diverged`, p.ID, p.Path, p.BaseCommit, p.BaseHash, p.Content, p.Diff, status, p.Proposer, p.Reviewer, p.ReviewRole, p.Decision, reason, p.CreatedAt, p.ReviewedAt, p.MergedCommit, p.IndexedAt, p.Diverged); err != nil {
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
func makeProposalDiff(old, new string) string {
	if old == new {
		return ""
	}
	return "--- authoritative\n+++ proposal\n-" + strings.ReplaceAll(old, "\n", "\n-") + "\n+" + strings.ReplaceAll(new, "\n", "\n+")
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
