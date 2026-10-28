package main

import "testing"

// Classification is part of the evidence reviewed, even when both classes
// remain readable. An old approval precondition must not survive its change.
func TestP7ComposeSourceFingerprintTracksSensitivity(t *testing.T) {
	tenant := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	namespace := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	ev := standingAnswerComposeEvidence{
		FactID:      "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
		Summary:     "Classification-sensitive evidence",
		Confidence:  0.8,
		Sensitivity: "public",
	}
	first := standingAnswerComposeFingerprint(ev, tenant, namespace)
	ev.Sensitivity = "internal"
	if second := standingAnswerComposeFingerprint(ev, tenant, namespace); second == first {
		t.Fatalf("source sensitivity changed without invalidating review fingerprint: %s", first)
	}
}
