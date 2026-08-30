package main

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
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

func TestMasterKeyRequiresConfiguration(t *testing.T) {
	os.Unsetenv("SCOS_CREDENTIAL_VAULT_MASTER_KEY")
	if _, err := masterKey(); err == nil {
		t.Fatal("expected masterKey to fail when unconfigured")
	}
}

func TestMasterKeyRejectsWrongLength(t *testing.T) {
	os.Setenv("SCOS_CREDENTIAL_VAULT_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	defer os.Unsetenv("SCOS_CREDENTIAL_VAULT_MASTER_KEY")
	if _, err := masterKey(); err == nil {
		t.Fatal("expected masterKey to reject a key that does not decode to 32 bytes")
	}
}

func TestMasterKeyAcceptsValidKey(t *testing.T) {
	key := testKey(t)
	os.Setenv("SCOS_CREDENTIAL_VAULT_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	defer os.Unsetenv("SCOS_CREDENTIAL_VAULT_MASTER_KEY")
	got, err := masterKey()
	if err != nil {
		t.Fatalf("masterKey: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(got))
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
