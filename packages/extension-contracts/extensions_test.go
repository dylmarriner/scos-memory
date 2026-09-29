package extensioncontracts

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestValidateManifest(t *testing.T) {
	m := &Manifest{PackageID: "org.scos.linter", Version: "1.0.0", Type: ExtTypeHook}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if err := ValidateManifest(&Manifest{Version: "1.0.0"}); err == nil {
		t.Fatal("expected error for empty package_id")
	}
}

// vectorManifest and the constants below are shared with the Python
// package's tests (scos_extension) so both implementations stay
// byte-compatible.
func vectorManifest() *Manifest {
	return &Manifest{
		PackageID: "org.scos.linter", Name: "Linter <é>", Version: "1.0.0", Type: ExtTypeHook,
		Publisher: "scos-team", RequiredPermissions: []string{"read:code", "write:memory"},
		Hooks:  []HookDef{{HookID: "lint", Stage: "pre_turn", Priority: 10, Timeout: 500}},
		Skills: []SkillDef{{SkillID: "fmt", Name: "Format & fix", Description: "a\nb", Parameters: nil}},
	}
}

const (
	vectorPublicKey = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	vectorSignature = "90IZ72ZkcttOxHT/MpX2NhoUrwglRbEfLpp8ustbbvZnmNTsqOh4Hnzu4nWJ/HhObYcVYJsLSVIJNX6izjfTDw=="
	vectorPayload   = `{"hooks":[{"hook_id":"lint","priority":10,"stage":"pre_turn","timeout_ms":500}],"name":"Linter <é>","package_id":"org.scos.linter","publisher":"scos-team","required_permissions":["read:code","write:memory"],"skills":[{"description":"a\nb","name":"Format & fix","parameters":[],"skill_id":"fmt"}],"type":"hook","version":"1.0.0"}`
)

func vectorKey(t *testing.T) ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestSigningPayloadIsCanonical(t *testing.T) {
	got, err := vectorManifest().SigningPayload()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != vectorPayload {
		t.Fatalf("payload drifted from the shared vector:\n got %s\nwant %s", got, vectorPayload)
	}
}

func TestSignAndVerify(t *testing.T) {
	priv := vectorKey(t)
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	m := vectorManifest()
	if err := m.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if pub != vectorPublicKey || m.Signature != vectorSignature {
		t.Fatalf("signature drifted from the shared vector: key %s sig %s", pub, m.Signature)
	}
	if !m.VerifySignature(pub) {
		t.Fatal("valid signature rejected")
	}

	tampered := *m
	tampered.RequiredPermissions = append([]string{"admin:*"}, m.RequiredPermissions...)
	if tampered.VerifySignature(pub) {
		t.Fatal("signature must cover required_permissions")
	}

	otherPub := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey))
	if m.VerifySignature(otherPub) {
		t.Fatal("signature verified under the wrong key")
	}
	for _, bad := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if m.VerifySignature(bad) {
			t.Fatalf("malformed public key %q accepted", bad)
		}
	}
}
