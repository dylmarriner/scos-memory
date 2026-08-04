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
		s.mu.Lock()
		p, err := s.mergeProposal(r.Context(), id)
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
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); e != nil {
			err = e
		} else if action == "review" {
			p, err = s.reviewProposalAuthorized(r.Context(), id, req.Reviewer, req.Role, req.Reason)
		} else {
			p, err = s.decideProposal(r.Context(), id, action == "approve", req.Reviewer, req.Role, req.Reason)
		}
	case "merge":
		p, err = s.mergeProposal(r.Context(), id)
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
func (s *okfServer) mergeProposal(ctx context.Context, id string) (*OKFProposal, error) {
	p, err := s.proposal(id, "approved")
	if err != nil {
		return nil, err
	}
	if os.Getenv("SCOS_OKF_PROTECTED_BRANCH") != "main" {
		return nil, errors.New("protected branch is not configured")
	}
	path, err := s.proposalPath(p.Path)
	if err != nil {
		return nil, err
	}
	if gitCommit(s.okfRoot) != p.BaseCommit {
		p.Diverged = true
		_ = s.persistProposal(ctx, p, p.Status, p.Proposer, p.ReviewRole, "authoritative commit diverged; explicit re-review required")
		return nil, errors.New("authoritative branch changed; divergence requires review")
	}
	current, _ := os.ReadFile(path)
	if sha256Hex(current) != p.BaseHash {
		p.Diverged = true
		if doc := s.documentForPath(p.Path); doc != nil {
			doc.Status = "diverged"
		}
		if proj := s.projections[p.Path]; proj != nil {
			proj.ImportStatus = "diverged"
		}
		_ = s.persistProposal(ctx, p, p.Status, p.Proposer, p.ReviewRole, "working tree diverged; explicit re-review required")
		return nil, errors.New("working tree diverged; explicit re-review required")
	}
	if err = os.WriteFile(path, []byte(p.Content), 0600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", s.okfRoot, "add", p.Path)
	if out, e := cmd.CombinedOutput(); e != nil {
		return nil, fmt.Errorf("stage proposal: %s", strings.TrimSpace(string(out)))
	}
	cmd = exec.CommandContext(ctx, "git", "-C", s.okfRoot, "-c", "user.name=SCOS OKF", "-c", "user.email=scos-okf@localhost", "commit", "-m", "OKF proposal "+p.ID)
	if out, e := cmd.CombinedOutput(); e != nil {
		return nil, fmt.Errorf("protected merge: %s", strings.TrimSpace(string(out)))
	}
	p.Status = "merged"
	p.MergedCommit = gitCommit(s.okfRoot)
	if err = s.persistProposal(ctx, p, "merged", p.Reviewer, p.ReviewRole, p.Reason); err != nil {
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
func (s *okfServer) requireRole(ctx context.Context, principal, role string) error {
	if role != "system_owner" && role != "knowledge_curator" && role != "auditor" && role != "memory_steward" {
		return errors.New("unsupported governance role")
	}
	if s.db == nil {
		return nil
	}
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM okf_governance_roles WHERE principal_id=$1 AND role=$2 AND active)`, principal, role).Scan(&ok)
	if err != nil {
		return fmt.Errorf("check governance role: %w", err)
	}
	if !ok {
		return errors.New("principal is not assigned the required governance role")
	}
	return nil
}
func (s *okfServer) persistProposal(ctx context.Context, p *OKFProposal, status, actor, role, reason string) error {
	if s.db == nil {
		return nil
	}
	var previous string
	_ = s.db.QueryRow(ctx, `SELECT status FROM okf_proposals WHERE id=$1`, p.ID).Scan(&previous)
	if _, err := s.db.Exec(ctx, `INSERT INTO okf_proposals (id,path,base_commit,base_hash,content,diff,status,proposer,reviewer,review_role,decision,reason,created_at,reviewed_at,merged_commit,indexed_at,diverged) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12,$13,NULLIF($14,''),NULLIF($15,''),NULLIF($16,''),$17) ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,reviewer=EXCLUDED.reviewer,review_role=EXCLUDED.review_role,decision=EXCLUDED.decision,reason=EXCLUDED.reason,reviewed_at=EXCLUDED.reviewed_at,merged_commit=EXCLUDED.merged_commit,indexed_at=EXCLUDED.indexed_at,diverged=EXCLUDED.diverged`, p.ID, p.Path, p.BaseCommit, p.BaseHash, p.Content, p.Diff, status, p.Proposer, p.Reviewer, p.ReviewRole, p.Decision, reason, p.CreatedAt, p.ReviewedAt, p.MergedCommit, p.IndexedAt, p.Diverged); err != nil {
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
