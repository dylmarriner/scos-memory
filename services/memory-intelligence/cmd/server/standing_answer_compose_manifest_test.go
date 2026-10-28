package main

import "testing"

func TestP7ComposeDraftManifestBindsDisplayedTextEvidenceAndBoundary(t *testing.T) {
	draft := standingAnswerComposeResult{
		Status:      "draft_requires_review",
		Confidence:  0.8,
		NamespaceID: "4cbe32c6-891c-4155-a9ec-6aa39e66704a",
		DraftText:   "Source fact A:\n\"Alpha\"\n\nSource fact B:\n\"Beta\"",
		Evidence: []standingAnswerComposeEvidence{
			{FactID: "5cbe32c6-891c-4155-a9ec-6aa39e66704a", SourceFingerprint: "hash-a"},
			{FactID: "6cbe32c6-891c-4155-a9ec-6aa39e66704a", SourceFingerprint: "hash-b"},
		},
	}
	tenant := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	original := standingAnswerComposeDraftFingerprint(tenant, draft)
	if len(original) != 64 || original != standingAnswerComposeDraftFingerprint(tenant, draft) {
		t.Fatalf("unstable or invalid SHA-256 manifest: %q", original)
	}
	if standingAnswerComposeDraftFingerprint("8cbe32c6-891c-4155-a9ec-6aa39e66704a", draft) == original {
		t.Fatal("tenant boundary not bound")
	}
	draft.Status = "approved"
	if standingAnswerComposeDraftFingerprint(tenant, draft) == original {
		t.Fatal("review status not bound")
	}
	draft.Status = "draft_requires_review"
	draft.Confidence = 0.7
	if standingAnswerComposeDraftFingerprint(tenant, draft) == original {
		t.Fatal("overall confidence not bound")
	}
	draft.Confidence = 0.8
	draft.NamespaceID = "9cbe32c6-891c-4155-a9ec-6aa39e66704a"
	if standingAnswerComposeDraftFingerprint(tenant, draft) == original {
		t.Fatal("namespace boundary not bound")
	}
	draft.NamespaceID = "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	draft.DraftText += "\nApproved"
	if standingAnswerComposeDraftFingerprint(tenant, draft) == original {
		t.Fatal("displayed text not bound")
	}
	draft.DraftText = "Source fact A:\n\"Alpha\"\n\nSource fact B:\n\"Beta\""
	draft.Evidence[0].SourceFingerprint = "changed-hash"
	if standingAnswerComposeDraftFingerprint(tenant, draft) == original {
		t.Fatal("source fingerprint not bound")
	}
	draft.Evidence[0].SourceFingerprint = "hash-a"
	draft.Evidence = draft.Evidence[:1]
	if standingAnswerComposeDraftFingerprint(tenant, draft) == original {
		t.Fatal("evidence set not bound")
	}
}
