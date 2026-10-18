package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// encString encrypts plaintext the way a Bitwarden client does: AES-256-CBC
// with PKCS#7 padding, then HMAC-SHA256 over iv||ciphertext.
func encString(t *testing.T, plaintext []byte, k vaultKey) string {
	t.Helper()
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	data := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(k.enc)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	m := hmac.New(sha256.New, k.mac)
	m.Write(iv)
	m.Write(data)
	b := base64.StdEncoding.EncodeToString
	return "2." + b(iv) + "|" + b(data) + "|" + b(m.Sum(nil))
}

func randomVaultKey(t *testing.T) (vaultKey, []byte) {
	t.Helper()
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	k, err := vaultKeyFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k, raw
}

// fakeVault is a Vaultwarden-shaped server holding one account.
type fakeVault struct {
	clientID, clientSecret string
	email                  string
	kdf                    kdfParams
	kdfInToken             bool
	sync                   map[string]any

	mu      sync.Mutex
	devices []string
	forms   []string
}

func newFakeVault(t *testing.T, password string, kdf kdfParams) (*fakeVault, vaultKey) {
	t.Helper()
	v := &fakeVault{clientID: "user.5f0c", clientSecret: "api-secret", email: "Backups@Example.NZ", kdf: kdf, kdfInToken: true}
	stretched, err := stretchedMasterKey(password, v.email, kdf)
	if err != nil {
		t.Fatal(err)
	}
	userKey, raw := randomVaultKey(t)
	v.sync = map[string]any{
		"profile": map[string]any{"email": v.email, "key": encString(t, raw, stretched), "organizations": []any{}},
		"ciphers": []any{},
	}
	return v, userKey
}

func (v *fakeVault) addCipher(c map[string]any) {
	v.sync["ciphers"] = append(v.sync["ciphers"].([]any), c)
}

func (v *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Bitwarden-Client-Version") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/identity/connect/token":
		if err := r.ParseForm(); err != nil || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		v.mu.Lock()
		v.forms = append(v.forms, r.PostForm.Encode())
		v.devices = append(v.devices, r.PostForm.Get("deviceIdentifier"))
		v.mu.Unlock()
		if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != v.clientID || r.PostForm.Get("client_secret") != v.clientSecret {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "invalid client credentials"})
			return
		}
		resp := map[string]any{"access_token": "at-1", "token_type": "Bearer"}
		if v.kdfInToken {
			resp["Kdf"], resp["KdfIterations"], resp["KdfMemory"], resp["KdfParallelism"] = v.kdf.Kdf, v.kdf.KdfIterations, v.kdf.KdfMemory, v.kdf.KdfParallelism
		}
		json.NewEncoder(w).Encode(resp)
	case "/identity/accounts/prelogin":
		json.NewEncoder(w).Encode(v.kdf)
	case "/api/sync":
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(v.sync)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// useFakeVault serves v over TLS and configures the drill to read item from it.
func useFakeVault(t *testing.T, v *fakeVault, password, itemID string) {
	t.Helper()
	srv := httptest.NewTLSServer(v)
	t.Cleanup(srv.Close)
	prev := vaultHTTPClient
	vaultHTTPClient = srv.Client()
	t.Cleanup(func() { vaultHTTPClient = prev })
	dir := t.TempDir()
	secretFile, passwordFile := filepath.Join(dir, "client-secret"), filepath.Join(dir, "password")
	if err := os.WriteFile(secretFile, []byte(v.clientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_BW_SERVER_URL", srv.URL)
	t.Setenv("SCOS_BW_CLIENT_ID", v.clientID)
	t.Setenv("SCOS_BW_CLIENT_SECRET_FILE", secretFile)
	t.Setenv("SCOS_BW_PASSWORD_FILE", passwordFile)
	t.Setenv("SCOS_BACKUP_KEY_SECRET_ID", itemID)
}

func TestVaultDrillReadsAPersonalLoginItemAndOpensABackup(t *testing.T) {
	ctx := context.Background()
	s, _, _ := objectStoreTestServer(t, "Enabled")
	fakePostgresTools(t)
	b := takeBackup(t, s)
	backupKey := strings.Repeat("k", 32)
	const password, itemID = "correct horse battery staple", "8b1f6a52-0c55-4d55-9b0a-7a1c2d3e4f50"
	v, userKey := newFakeVault(t, password, kdfParams{Kdf: 0, KdfIterations: 600000})
	v.addCipher(map[string]any{
		"id": itemID, "type": 1, "name": encString(t, []byte("SCOS backup key"), userKey),
		"login":        map[string]any{"username": encString(t, []byte("scos"), userKey), "password": encString(t, []byte(backupKey), userKey)},
		"revisionDate": "2026-10-18T01:02:03Z",
	})
	useFakeVault(t, v, password, itemID)

	d, err := s.secretRecoveryDrill(ctx)
	if err != nil || d.Status != "verified" || d.BackupID != b.ID || d.SecretKey != "SCOS backup key" || d.RevisedAt != "2026-10-18T01:02:03Z" {
		t.Fatalf("drill = %+v, %v", d, err)
	}
	if c := s.secretRecoveryCoverage(time.Now()); !c.Covered || !strings.Contains(c.Evidence, itemID) {
		t.Fatalf("coverage = %+v", c)
	}
	if _, err := s.secretRecoveryDrill(ctx); err != nil {
		t.Fatal(err)
	}
	if len(v.devices) != 2 || v.devices[0] == "" || v.devices[0] != v.devices[1] {
		t.Fatalf("every drill must sign in as the same device: %v", v.devices)
	}
	for _, f := range v.forms {
		if strings.Contains(f, "correct+horse") {
			t.Fatalf("the master password must never leave the host: %s", f)
		}
	}

	// The wrong master password cannot unwrap the account key.
	if err := os.WriteFile(os.Getenv("SCOS_BW_PASSWORD_FILE"), []byte("wrong password"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.secretRecoveryDrill(ctx); d.Status != "failed" || !strings.Contains(d.Detail, "does not unlock") {
		t.Fatalf("wrong password = %+v", d)
	}
	if c := s.secretRecoveryCoverage(time.Now()); c.Covered {
		t.Fatalf("coverage after a failed drill = %+v", c)
	}
}

func TestVaultDrillReadsAnOrganisationItemWithItsOwnKeyUnderArgon2(t *testing.T) {
	ctx := context.Background()
	s, _, _ := objectStoreTestServer(t, "Enabled")
	fakePostgresTools(t)
	b := takeBackup(t, s)
	backupKey := strings.Repeat("k", 32)
	const password, itemID, orgID = "p", "item-org", "org-1"
	v, userKey := newFakeVault(t, password, kdfParams{Kdf: 1, KdfIterations: 3, KdfMemory: 64, KdfParallelism: 4})
	v.kdfInToken = false // older servers only answer prelogin

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	profile := v.sync["profile"].(map[string]any)
	profile["privateKey"] = encString(t, der, userKey)
	orgKey, orgRaw := randomVaultKey(t)
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &priv.PublicKey, orgRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile["organizations"] = []any{map[string]any{"id": orgID, "key": "4." + base64.StdEncoding.EncodeToString(wrapped)}}
	itemKey, itemRaw := randomVaultKey(t)
	v.addCipher(map[string]any{
		"id": itemID, "organizationId": orgID, "type": 2, "key": encString(t, itemRaw, orgKey),
		"name": encString(t, []byte("backup key note"), itemKey), "notes": encString(t, []byte(backupKey), itemKey),
		"revisionDate": "2026-10-18T00:00:00Z",
	})
	useFakeVault(t, v, password, itemID)

	if d, err := s.secretRecoveryDrill(ctx); err != nil || d.Status != "verified" || d.BackupID != b.ID || d.SecretKey != "backup key note" {
		t.Fatalf("drill = %+v, %v", d, err)
	}
}

func TestVaultDrillFailures(t *testing.T) {
	ctx := context.Background()
	s, _, _ := objectStoreTestServer(t, "Enabled")
	const password, itemID = "p", "item-1"
	v, userKey := newFakeVault(t, password, kdfParams{Kdf: 0, KdfIterations: 5000})
	deleted := "2026-10-01T00:00:00Z"
	v.addCipher(map[string]any{"id": "trashed", "type": 1, "name": encString(t, []byte("old"), userKey), "deletedDate": deleted})
	tampered := encString(t, []byte(strings.Repeat("k", 32)), userKey)
	tampered = tampered[:len(tampered)-6] + "AAAAA="
	v.addCipher(map[string]any{"id": itemID, "type": 1, "name": encString(t, []byte("n"), userKey), "login": map[string]any{"password": tampered}})
	useFakeVault(t, v, password, itemID)

	cases := []struct {
		setup func()
		want  string
	}{
		{func() {}, "fails its MAC"},
		{func() { t.Setenv("SCOS_BACKUP_KEY_SECRET_ID", "trashed") }, "in the trash"},
		{func() { t.Setenv("SCOS_BACKUP_KEY_SECRET_ID", "missing") }, "no item missing"},
		{func() { t.Setenv("SCOS_BW_CLIENT_ID", "user.other") }, "invalid client credentials"},
	}
	for _, c := range cases {
		c.setup()
		if d, _ := s.secretRecoveryDrill(ctx); d.Status != "failed" || !strings.Contains(d.Detail, c.want) {
			t.Fatalf("want %q, got %+v", c.want, d)
		}
	}

	t.Setenv("SCOS_BW_SERVER_URL", "http://vault.example")
	if _, _, err := s.secretSource(); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Fatalf("plain http to a remote vault = %v", err)
	}
	t.Setenv("SCOS_BW_SERVER_URL", "https://vault.example")
	t.Setenv("SCOS_BW_PASSWORD_FILE", "")
	if _, _, err := s.secretSource(); err == nil || !strings.Contains(err.Error(), "SCOS_BW_PASSWORD_FILE") {
		t.Fatalf("missing password file = %v", err)
	}
	if c := s.secretRecoveryCoverage(time.Now()); c.Covered || !strings.Contains(c.Reason, "SCOS_BW_PASSWORD_FILE") {
		t.Fatalf("coverage = %+v", c)
	}
}

func TestDecryptEncStringRefusesUnauthenticatedTypes(t *testing.T) {
	k, _ := randomVaultKey(t)
	if _, err := decryptEncString("0.aXY=|ZGF0YQ==", k); err == nil {
		t.Fatal("a type 0 value has no MAC and must be refused")
	}
	other, _ := randomVaultKey(t)
	if _, err := decryptEncString(encString(t, []byte("x"), k), other); err == nil || !strings.Contains(err.Error(), "MAC") {
		t.Fatalf("wrong key = %v", err)
	}
}

// Known answers from Bitwarden's SDK (bitwarden-crypto master_key.rs): the
// server-side password hash, PBKDF2-SHA256(master key, password, 1), for a
// master key derived with each KDF. The email variants check normalisation.
func TestMasterKeyMatchesBitwardenKnownAnswers(t *testing.T) {
	hash := func(t *testing.T, password, email string, p kdfParams) string {
		t.Helper()
		k, err := masterKey(password, email, p)
		if err != nil {
			t.Fatal(err)
		}
		h, err := pbkdf2.Key(sha256.New, string(k), []byte(password), 1, 32)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(h)
	}
	for _, email := range []string{"test@bitwarden.com", "TEST@bitwarden.com", " test@bitwarden.com"} {
		if got := hash(t, "asdfasdf", email, kdfParams{Kdf: 0, KdfIterations: 100000}); got != "wmyadRMyBZOH7P/a/ucTCbSghKgdzDpPqUnu/DAVtSw=" {
			t.Fatalf("PBKDF2 %q = %s", email, got)
		}
	}
	if got := hash(t, "asdfasdf", "test_salt", kdfParams{Kdf: 1, KdfIterations: 4, KdfMemory: 32, KdfParallelism: 2}); got != "PR6UjYmjmppTYcdyTiNbAhPJuQQOmynKbdEl1oyi/iQ=" {
		t.Fatalf("Argon2id = %s", got)
	}
	for _, p := range []kdfParams{{Kdf: 0, KdfIterations: 4999}, {Kdf: 1, KdfIterations: 1, KdfMemory: 16, KdfParallelism: 1}, {Kdf: 1, KdfIterations: 2, KdfMemory: 15, KdfParallelism: 1}} {
		if _, err := masterKey("p", "e", p); err == nil {
			t.Fatalf("%+v is below the minimums and must be refused", p)
		}
	}
}
