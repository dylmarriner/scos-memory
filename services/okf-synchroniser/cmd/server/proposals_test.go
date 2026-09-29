package main

import (
	"context"
	"testing"
)

// TestDecideProposalRejectsSelfApproval guards against a regression of the
// separation-of-duties bug the workflow-state-machine auditor confirmed
// (F6): reviewProposalAuthorized already rejects reviewer == p.Proposer at
// the 'review' step, but decideProposal (the step that actually flips a
// proposal to "approved" and gates mergeProposal) previously had no
// equivalent check -- it accepted whatever req.Reviewer the caller
// supplied, validated only by requireRole (role membership, not identity
// against the proposer). A proposer who also held a governance role could
// therefore call the "approve" action naming themself as reviewer and
// self-approve their own OKF document change. roleLookup grants every role,
// isolating this test to the identity check under test.
func TestDecideProposalRejectsSelfApproval(t *testing.T) {
	s := &okfServer{
		roleLookup: grantAllRoles,
		proposals: map[string]*OKFProposal{
			"p1": {
				ID:       "p1",
				Proposer: "alice",
				Reviewer: "bob",
				Status:   "under_review",
			},
		},
	}

	// alice (the proposer) attempts to approve her own proposal by naming
	// herself explicitly as the reviewer, overriding the independent
	// reviewer (bob) who actually claimed the review step.
	_, err := s.decideProposal(context.Background(), "p1", true, "alice", "knowledge_curator", "self-approved")
	if err == nil {
		t.Fatal("expected decideProposal to reject a reviewer equal to the proposer, got nil error")
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

// TestDecideProposalDefaultsToRecordedReviewerAndStillRejectsProposer covers
// the empty-reviewer path (decideProposal falls back to p.Reviewer when
// req.Reviewer is empty): if the recorded reviewer was somehow the proposer
// themself, the fallback must not silently approve.
func TestDecideProposalDefaultsToRecordedReviewerAndStillRejectsProposer(t *testing.T) {
	s := &okfServer{
		roleLookup: grantAllRoles,
		proposals: map[string]*OKFProposal{
			"p2": {
				ID:       "p2",
				Proposer: "carol",
				Reviewer: "carol", // should never happen via reviewProposalAuthorized, but decideProposal must not trust it blindly
				Status:   "under_review",
			},
		},
	}
	_, err := s.decideProposal(context.Background(), "p2", true, "", "knowledge_curator", "")
	if err == nil {
		t.Fatal("expected decideProposal to reject when the recorded reviewer equals the proposer")
	}
}
