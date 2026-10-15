package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey(t)
	ciphertext, nonce, err := encryptValue(key, "sk-live-super-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(ciphertext) == "sk-live-super-secret" {
		t.Fatal("ciphertext must not equal plaintext")
	}
	plaintext, err := decryptValue(key, ciphertext, nonce)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plaintext != "sk-live-super-secret" {
		t.Fatalf("expected round-tripped plaintext, got %q", plaintext)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	key := testKey(t)
	wrongKey := testKey(t)
	ciphertext, nonce, err := encryptValue(key, "top-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := decryptValue(wrongKey, ciphertext, nonce); err == nil {
		t.Fatal("expected decryption under the wrong key to fail")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	key := testKey(t)
	ciphertext, nonce, err := encryptValue(key, "top-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext[0] ^= 0xFF
	if _, err := decryptValue(key, ciphertext, nonce); err == nil {
		t.Fatal("expected decryption of tampered ciphertext to fail")
	}
}

func setKeyringEnv(t *testing.T, current []byte, version, previous string) {
	t.Helper()
	t.Setenv(envMasterKey, base64.StdEncoding.EncodeToString(current))
	t.Setenv(envKeyVersion, version)
	t.Setenv(envPreviousKeys, previous)
}

func TestLoadKeyringRequiresConfiguration(t *testing.T) {
	t.Setenv(envMasterKey, "")
	if _, err := loadKeyring(); err == nil {
		t.Fatal("expected loadKeyring to fail when the master key is unconfigured")
	}
}

func TestLoadKeyringRejectsWrongLength(t *testing.T) {
	t.Setenv(envMasterKey, base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := loadKeyring(); err == nil {
		t.Fatal("expected loadKeyring to reject a key that does not decode to 32 bytes")
	}
}

func TestLoadKeyringDefaultsToVersionOne(t *testing.T) {
	setKeyringEnv(t, testKey(t), "", "")
	ring, err := loadKeyring()
	if err != nil {
		t.Fatalf("loadKeyring: %v", err)
	}
	if ring.currentVersion() != defaultKeyVersion {
		t.Fatalf("expected default key version %d, got %d", defaultKeyVersion, ring.currentVersion())
	}
}

func TestLoadKeyringRejectsInvalidConfiguration(t *testing.T) {
	current := base64.StdEncoding.EncodeToString(testKey(t))
	cases := map[string]struct{ version, previous string }{
		"non-numeric version":         {"two", ""},
		"zero version":                {"0", ""},
		"previous without version":    {"2", current},
		"previous duplicates current": {"2", "2:" + current},
		"previous newer than current": {"2", "3:" + current},
		"previous repeated":           {"3", "1:" + current + ",1:" + current},
		"previous wrong length":       {"2", "1:" + base64.StdEncoding.EncodeToString([]byte("short"))},
		"previous not base64":         {"2", "1:not base64!"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envMasterKey, current)
			t.Setenv(envKeyVersion, tc.version)
			t.Setenv(envPreviousKeys, tc.previous)
			if _, err := loadKeyring(); err == nil {
				t.Fatalf("expected loadKeyring to reject version=%q previous=%q", tc.version, tc.previous)
			}
		})
	}
}

// TestKeyringRotationKeepsOldCiphertextsReadable is the regression guard for
// the "rotate by restart" defect: before key versioning, swapping
// SCOS_CREDENTIAL_VAULT_MASTER_KEY made every stored value undecryptable
// because key_version was never consulted. A value sealed under version 1
// must stay readable after the vault restarts on version 2 with version 1
// retained as a previous key, and new seals must record version 2.
func TestKeyringRotationKeepsOldCiphertextsReadable(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)

	setKeyringEnv(t, oldKey, "1", "")
	before, err := loadKeyring()
	if err != nil {
		t.Fatalf("load v1 keyring: %v", err)
	}
	ciphertext, nonce, version, err := before.seal("sk-live-before-rotation")
	if err != nil {
		t.Fatalf("seal under v1: %v", err)
	}
	if version != 1 {
		t.Fatalf("expected seal under key version 1, got %d", version)
	}

	setKeyringEnv(t, newKey, "2", "1:"+base64.StdEncoding.EncodeToString(oldKey))
	after, err := loadKeyring()
	if err != nil {
		t.Fatalf("load v2 keyring: %v", err)
	}
	plaintext, err := after.open(version, ciphertext, nonce)
	if err != nil {
		t.Fatalf("open v1 ciphertext after rotation: %v", err)
	}
	if plaintext != "sk-live-before-rotation" {
		t.Fatalf("unexpected plaintext after rotation: %q", plaintext)
	}

	resealed, resealedNonce, newVersion, err := after.seal(plaintext)
	if err != nil {
		t.Fatalf("re-seal under v2: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("expected new seals to record key version 2, got %d", newVersion)
	}
	if _, err := decryptValue(oldKey, resealed, resealedNonce); err == nil {
		t.Fatal("a value re-sealed under version 2 must not open with the retired version 1 key")
	}
	if got, err := after.open(newVersion, resealed, resealedNonce); err != nil || got != plaintext {
		t.Fatalf("open v2 ciphertext: %q, %v", got, err)
	}

	// Once version 1 is dropped from the keyring, a value still sealed under
	// it is reported as a missing key version rather than a generic
	// decryption failure, so the operator knows which key to restore.
	setKeyringEnv(t, newKey, "2", "")
	dropped, err := loadKeyring()
	if err != nil {
		t.Fatalf("load v2-only keyring: %v", err)
	}
	_, err = dropped.open(version, ciphertext, nonce)
	var missing unknownKeyVersionError
	if !errors.As(err, &missing) || missing.version != 1 {
		t.Fatalf("expected unknownKeyVersionError for version 1, got %v", err)
	}
}

func TestKeyringVersionsAreSorted(t *testing.T) {
	setKeyringEnv(t, testKey(t), "5", "3:"+base64.StdEncoding.EncodeToString(testKey(t))+", 1:"+base64.StdEncoding.EncodeToString(testKey(t)))
	ring, err := loadKeyring()
	if err != nil {
		t.Fatalf("loadKeyring: %v", err)
	}
	got := ring.versions()
	if len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 5 {
		t.Fatalf("expected versions [1 3 5], got %v", got)
	}
}

func TestReadinessRequiresKeyring(t *testing.T) {
	s := &credentialVaultServer{db: &sql.DB{}}
	if err := s.readinessCheck(); err == nil {
		t.Fatal("expected readiness check to fail without a configured keyring")
	}
}

func TestKeyAdminRoutesRequireAdminScope(t *testing.T) {
	s := &credentialVaultServer{}
	for _, path := range []string{"/v1/admin/keys", "/v1/admin/keys/rotate"} {
		for _, claims := range []*auth.Claims{nil, {TenantID: "tenant-1", PrincipalID: "agent-1", Scopes: []string{"memory:read", "memory:write"}}} {
			mux := http.NewServeMux()
			mux.Handle("/v1/admin/keys", auth.RequireScope(credentialsAdminScope, http.HandlerFunc(s.handleKeyStatus)))
			mux.Handle("/v1/admin/keys/rotate", auth.RequireScope(credentialsAdminScope, http.HandlerFunc(s.handleKeyRotate)))
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
			req.Header.Set("X-SCOS-Principal-ID", "agent-1")
			if claims != nil {
				req = req.WithContext(auth.ContextWithClaims(req.Context(), *claims))
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s with claims %+v: expected 403 without %s, got %d", path, claims, credentialsAdminScope, rec.Code)
			}
		}
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	s := &credentialVaultServer{}
	if err := s.readinessCheck(); err == nil {
		t.Fatal("expected readiness check to fail without a configured postgres dependency")
	}
}

func TestResolveIdentityRequiresHeadersWithoutClaims(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/credentials", nil)
	_, status, _ := resolveIdentity(req)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal headers, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/credentials", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	req.Header.Set("X-SCOS-Principal-ID", "agent-1")
	id, status, _ := resolveIdentity(req)
	if status != 0 {
		t.Fatalf("expected identity to resolve, got status %d", status)
	}
	if id.TenantID != "tenant-1" || id.PrincipalID != "agent-1" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestHandleCredentialByIDRoutesMethodNotAllowed(t *testing.T) {
	s := &credentialVaultServer{}
	req := httptest.NewRequest(http.MethodPatch, "/v1/credentials/some-id", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	req.Header.Set("X-SCOS-Principal-ID", "agent-1")
	rec := httptest.NewRecorder()
	s.handleCredentialByID(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for unsupported method, got %d", rec.Code)
	}
}

func TestHandleCredentialByIDRequiresIdentity(t *testing.T) {
	s := &credentialVaultServer{}
	req := httptest.NewRequest(http.MethodGet, "/v1/credentials/some-id", nil)
	rec := httptest.NewRecorder()
	s.handleCredentialByID(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without identity, got %d", rec.Code)
	}
}
