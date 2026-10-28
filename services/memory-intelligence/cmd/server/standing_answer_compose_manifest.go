package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// standingAnswerComposeDraftFingerprint pins the entire human-review draft:
// the security boundary, exact displayed text, and every ordered source
// fingerprint. A digest is not an approval, a signature, or publication.
// The handler sorts source IDs before building this manifest.
func standingAnswerComposeDraftFingerprint(tenantID string, draft standingAnswerComposeResult) string {
	h := sha256.New()
	_, _ = h.Write([]byte("scos:p7:draft:v1\x00"))
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(draft.NamespaceID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(draft.Status))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatFloat(draft.Confidence, 'g', -1, 64)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(draft.DraftText))
	_, _ = h.Write([]byte{0})
	for _, source := range draft.Evidence {
		_, _ = h.Write([]byte(source.FactID))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(source.SourceFingerprint))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
