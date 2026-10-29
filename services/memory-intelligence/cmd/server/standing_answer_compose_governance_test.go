package main

import "testing"

// A reviewer must be able to detect changes in the source's trust and
// validity window even if its summary, version and confidence are unchanged.
func TestP7ComposeSourceFingerprintTracksGovernanceMetadata(t *testing.T) {
	tenant := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	namespace := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	ev := standingAnswerComposeEvidence{
		FactID:         "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
		Summary:        "A source requiring review",
		Confidence:     0.8,
		Sensitivity:    "internal",
		AuthorityLevel: 80,
		ValidFrom:      "2026-10-01T00:00:00Z",
		ValidUntil:     "2026-12-01T00:00:00Z",
		ExpiresAt:      "2026-12-15T00:00:00Z",
	}
	original := standingAnswerComposeFingerprint(ev, tenant, namespace)
	changes := []struct {
		name   string
		mutate func(*standingAnswerComposeEvidence)
	}{
		{"authority downgrade", func(e *standingAnswerComposeEvidence) { e.AuthorityLevel = 5 }},
		{"valid from", func(e *standingAnswerComposeEvidence) { e.ValidFrom = "2026-10-02T00:00:00Z" }},
		{"valid until", func(e *standingAnswerComposeEvidence) { e.ValidUntil = "2026-11-30T00:00:00Z" }},
		{"expiry", func(e *standingAnswerComposeEvidence) { e.ExpiresAt = "2026-12-14T00:00:00Z" }},
		{"unset expiry", func(e *standingAnswerComposeEvidence) { e.ExpiresAt = "" }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			altered := ev
			tc.mutate(&altered)
			if got := standingAnswerComposeFingerprint(altered, tenant, namespace); got == original {
				t.Fatalf("governance metadata drift did not change fingerprint (%s)", tc.name)
			}
		})
	}
}
