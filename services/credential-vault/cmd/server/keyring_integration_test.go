//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/database"
)

// vaultIntegrationDBs opens the owner connection (DATABASE_URL, used only
// to seed and clean a throwaway tenant) and the deployed scos_app
// connection (SCOS_APP_TEST_DATABASE_URL) the server runs on, so every
// handler query executes under FORCE ROW LEVEL SECURITY exactly as in
// production.
func vaultIntegrationDBs(t *testing.T) (admin, app *sql.DB) {
	t.Helper()
	adminURL := os.Getenv("DATABASE_URL")
	appURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL and SCOS_APP_TEST_DATABASE_URL must be set for credential-vault integration tests")
	}
	for _, u := range []string{adminURL, appURL} {
		if err := database.RequireTLS(u); err != nil {
			t.Fatalf("integration database URL is not TLS-enabled: %v", err)
		}
	}
	var err error
	if admin, err = sql.Open("pgx", adminURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if app, err = sql.Open("pgx", appURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	return admin, app
}

func seedVaultTenant(t *testing.T, admin *sql.DB) (tenantID, principalID string) {
	t.Helper()
	ctx := context.Background()
	if err := admin.QueryRowContext(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id::text`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id::text`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM audit_entries WHERE tenant_id = $1`,
			`DELETE FROM credentials WHERE tenant_id = $1`,
			`DELETE FROM principals WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			if _, err := admin.ExecContext(context.Background(), stmt, tenantID); err != nil {
				t.Errorf("cleanup %q: %v", stmt, err)
			}
		}
	})
	return tenantID, principalID
}

func keyringFor(t *testing.T, current []byte, version string, previous map[string][]byte) *keyring {
	t.Helper()
	t.Setenv(envMasterKey, base64.StdEncoding.EncodeToString(current))
	t.Setenv(envKeyVersion, version)
	list := ""
	for v, key := range previous {
		if list != "" {
			list += ","
		}
		list += v + ":" + base64.StdEncoding.EncodeToString(key)
	}
	t.Setenv(envPreviousKeys, list)
	ring, err := loadKeyring()
	if err != nil {
		t.Fatalf("loadKeyring: %v", err)
	}
	return ring
}

func vaultMux(s *credentialVaultServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/credentials", s.handleCredentials)
	mux.HandleFunc("/v1/credentials/", s.handleCredentialByID)
	mux.Handle("/v1/admin/keys", auth.RequireScope(credentialsAdminScope, http.HandlerFunc(s.handleKeyStatus)))
	mux.Handle("/v1/admin/keys/rotate", auth.RequireScope(credentialsAdminScope, http.HandlerFunc(s.handleKeyRotate)))
	return mux
}

func vaultCall(t *testing.T, mux http.Handler, claims auth.Claims, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestKeyRotationReSealsWithoutLosingReadability drives the real handlers
// against PostgreSQL under the deployed scos_app role: a credential stored
// under key version 1 stays readable after the vault restarts on key
// version 2 (with version 1 retained as a previous key), the admin rotation
// re-seals it to version 2 and audits that, new writes record version 2,
// and once version 1 is retired the rotated value is still readable.
func TestKeyRotationReSealsWithoutLosingReadability(t *testing.T) {
	admin, app := vaultIntegrationDBs(t)
	tenantID, principalID := seedVaultTenant(t, admin)
	user := auth.Claims{TenantID: tenantID, PrincipalID: principalID, Scopes: []string{"memory:read", "memory:write"}}
	operator := auth.Claims{TenantID: tenantID, PrincipalID: principalID, Scopes: []string{credentialsAdminScope}}

	v1Key, v2Key := testKey(t), testKey(t)

	// ── vault running on key version 1 ───────────────────────────────────
	v1 := vaultMux(&credentialVaultServer{db: app, keys: keyringFor(t, v1Key, "1", nil)})
	rec := vaultCall(t, v1, user, http.MethodPost, "/v1/credentials", map[string]any{"name": "openai", "value": "sk-before-rotation"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create under v1: %d %s", rec.Code, rec.Body.String())
	}
	var created credentialMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.KeyVersion != 1 {
		t.Fatalf("expected credential sealed under key version 1, got %d", created.KeyVersion)
	}

	// ── restart on key version 2, version 1 retained ─────────────────────
	v2 := vaultMux(&credentialVaultServer{db: app, keys: keyringFor(t, v2Key, "2", map[string][]byte{"1": v1Key})})

	rec = vaultCall(t, v2, user, http.MethodGet, "/v1/credentials/"+created.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read v1 credential after restart on v2: %d %s", rec.Code, rec.Body.String())
	}
	var read credentialValueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.Value != "sk-before-rotation" || read.KeyVersion != 1 {
		t.Fatalf("expected v1 value readable before rotation, got value=%q key_version=%d", read.Value, read.KeyVersion)
	}

	// A caller without credentials:admin cannot rotate.
	if rec := vaultCall(t, v2, user, http.MethodPost, "/v1/admin/keys/rotate", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for rotation without %s, got %d", credentialsAdminScope, rec.Code)
	}

	rec = vaultCall(t, v2, operator, http.MethodGet, "/v1/admin/keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("key status: %d %s", rec.Code, rec.Body.String())
	}
	var status keyStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.CurrentKeyVersion != 2 || status.PendingRotation != 1 || status.CredentialsByVersion["1"] != 1 {
		t.Fatalf("unexpected pre-rotation key status: %+v", status)
	}

	rec = vaultCall(t, v2, operator, http.MethodPost, "/v1/admin/keys/rotate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var rotated keyRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Rotated != 1 || rotated.RotatedFrom["1"] != 1 || rotated.PendingRotation != 0 || rotated.CurrentKeyVersion != 2 {
		t.Fatalf("unexpected rotation result: %+v", rotated)
	}

	var storedVersion int
	var storedCiphertext, storedNonce []byte
	if err := admin.QueryRow(`SELECT key_version, ciphertext, nonce FROM credentials WHERE id = $1`, created.ID).Scan(&storedVersion, &storedCiphertext, &storedNonce); err != nil {
		t.Fatal(err)
	}
	if storedVersion != 2 {
		t.Fatalf("expected stored key_version 2 after rotation, got %d", storedVersion)
	}
	if _, err := decryptValue(v1Key, storedCiphertext, storedNonce); err == nil {
		t.Fatal("rotated ciphertext must no longer open under the retired v1 key")
	}

	var rotationAudits int
	if err := admin.QueryRow(`SELECT count(*) FROM audit_entries WHERE tenant_id = $1 AND action = 'credential_key_rotated' AND resource_id = $2 AND decision = 'allowed' AND actor_id = $3`,
		tenantID, created.ID, principalID).Scan(&rotationAudits); err != nil {
		t.Fatal(err)
	}
	if rotationAudits != 1 {
		t.Fatalf("expected one credential_key_rotated audit entry, got %d", rotationAudits)
	}

	// A second rotation is a no-op.
	rec = vaultCall(t, v2, operator, http.MethodPost, "/v1/admin/keys/rotate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second rotate: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Rotated != 0 {
		t.Fatalf("expected idempotent second rotation, got %+v", rotated)
	}

	// New writes record the current version.
	rec = vaultCall(t, v2, user, http.MethodPost, "/v1/credentials", map[string]any{"name": "github", "value": "ghp-after-rotation"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create under v2: %d %s", rec.Code, rec.Body.String())
	}
	var second credentialMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.KeyVersion != 2 {
		t.Fatalf("expected new credential sealed under key version 2, got %d", second.KeyVersion)
	}

	// ── retire version 1 entirely ────────────────────────────────────────
	v2Only := vaultMux(&credentialVaultServer{db: app, keys: keyringFor(t, v2Key, "2", nil)})
	for id, want := range map[string]string{created.ID: "sk-before-rotation", second.ID: "ghp-after-rotation"} {
		rec = vaultCall(t, v2Only, user, http.MethodGet, "/v1/credentials/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("read %s after retiring v1: %d %s", id, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &read); err != nil {
			t.Fatal(err)
		}
		if read.Value != want || read.KeyVersion != 2 {
			t.Fatalf("read %s after retiring v1: value=%q key_version=%d", id, read.Value, read.KeyVersion)
		}
	}
}

// TestKeyRotationRefusesWhenAStoredVersionHasNoKey proves a rotation never
// half-completes: if any credential is sealed under a version this process
// has no key for, nothing is re-sealed, the response names the missing
// version, and the refusal is audited.
func TestKeyRotationRefusesWhenAStoredVersionHasNoKey(t *testing.T) {
	admin, app := vaultIntegrationDBs(t)
	tenantID, principalID := seedVaultTenant(t, admin)
	user := auth.Claims{TenantID: tenantID, PrincipalID: principalID}
	operator := auth.Claims{TenantID: tenantID, PrincipalID: principalID, Scopes: []string{"admin"}}

	v1Key, v2Key, v3Key := testKey(t), testKey(t), testKey(t)
	v1 := vaultMux(&credentialVaultServer{db: app, keys: keyringFor(t, v1Key, "1", nil)})
	rec := vaultCall(t, v1, user, http.MethodPost, "/v1/credentials", map[string]any{"name": "legacy", "value": "legacy-value"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create under v1: %d %s", rec.Code, rec.Body.String())
	}
	var legacy credentialMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil {
		t.Fatal(err)
	}

	// Rotated straight to v3 while forgetting to keep v1.
	v3 := vaultMux(&credentialVaultServer{db: app, keys: keyringFor(t, v3Key, "3", map[string][]byte{"2": v2Key})})
	rec = vaultCall(t, v3, operator, http.MethodPost, "/v1/admin/keys/rotate", nil)
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("[1]")) {
		t.Fatalf("expected 409 naming missing key version 1, got %d %s", rec.Code, rec.Body.String())
	}
	var storedVersion int
	if err := admin.QueryRow(`SELECT key_version FROM credentials WHERE id = $1`, legacy.ID).Scan(&storedVersion); err != nil {
		t.Fatal(err)
	}
	if storedVersion != 1 {
		t.Fatalf("refused rotation must not touch the credential, key_version now %d", storedVersion)
	}
	var refusals int
	if err := admin.QueryRow(`SELECT count(*) FROM audit_entries WHERE tenant_id = $1 AND action = 'credential_keys_rotated' AND decision = 'error' AND actor_id = $2`, tenantID, principalID).Scan(&refusals); err != nil {
		t.Fatal(err)
	}
	if refusals != 1 {
		t.Fatalf("expected the refused rotation to be audited once, got %d", refusals)
	}

	// Reading the unrotated value now fails with the missing version named,
	// and the failed read is audited rather than silently dropped.
	rec = vaultCall(t, v3, user, http.MethodGet, "/v1/credentials/"+legacy.ID, nil)
	if rec.Code != http.StatusInternalServerError || !bytes.Contains(rec.Body.Bytes(), []byte("key version 1")) {
		t.Fatalf("expected read to fail naming key version 1, got %d %s", rec.Code, rec.Body.String())
	}
	var readErrors int
	if err := admin.QueryRow(`SELECT count(*) FROM audit_entries WHERE tenant_id = $1 AND action = 'credential_read' AND resource_id = $2 AND decision = 'error'`, tenantID, legacy.ID).Scan(&readErrors); err != nil {
		t.Fatal(err)
	}
	if readErrors != 1 {
		t.Fatalf("expected the failed read to be audited once, got %d", readErrors)
	}
}
