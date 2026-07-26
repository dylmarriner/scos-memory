package extensioncontracts

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestValidateManifest(t *testing.T) {
	m := &Manifest{
		PackageID: "org.scos.linter",
		Version:   "1.0.0",
		Type:      ExtTypeHook,
	}

	if err := ValidateManifest(m); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	invalid := &Manifest{
		PackageID: "",
		Version:   "1.0.0",
	}
	if err := ValidateManifest(invalid); err == nil {
		t.Fatal("expected error for empty package_id")
	}
}

func TestVerifySignature(t *testing.T) {
	pubKey := "pub-key-secret"
	raw := "org.scos.linter:1.0.0:scos-team:" + pubKey
	h := sha256.Sum256([]byte(raw))
	sig := hex.EncodeToString(h[:])

	m := &Manifest{
		PackageID: "org.scos.linter",
		Version:   "1.0.0",
		Publisher: "scos-team",
		Signature: sig,
	}

	if !m.VerifySignature(pubKey) {
		t.Fatal("expected signature verification to succeed")
	}
}
