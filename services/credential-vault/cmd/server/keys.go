package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// credentialsAdminScope gates the keyring administration routes. It follows
// the "<area>:admin" convention of backup-manager's backups:admin and
// recovery-orchestrator's recovery:admin; auth.RequireScope also honours the
// global "admin" scope, and refuses every request without signed claims
// (including the shared static service token).
const credentialsAdminScope = "credentials:admin"

// keyStatusResponse reports, for the caller's tenant, how many stored
// credentials are sealed under each key version. A retired key can be
// removed from SCOS_CREDENTIAL_VAULT_PREVIOUS_KEYS only once no tenant has a
// credential left under its version.
type keyStatusResponse struct {
	CurrentKeyVersion     int            `json:"current_key_version"`
	ConfiguredKeyVersions []int          `json:"configured_key_versions"`
	CredentialsByVersion  map[string]int `json:"credentials_by_key_version"`
	PendingRotation       int            `json:"pending_rotation"`
	UnreadableKeyVersions []int          `json:"unreadable_key_versions"`
}

type keyRotateResponse struct {
	CurrentKeyVersion int            `json:"current_key_version"`
	Rotated           int            `json:"rotated"`
	RotatedFrom       map[string]int `json:"rotated_from_key_versions"`
	PendingRotation   int            `json:"pending_rotation"`
}

func (s *credentialVaultServer) handleKeyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentity(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	counts, err := credentialCountsByKeyVersion(ctx, tx, identity.TenantID)
	if err != nil {
		http.Error(w, "count credentials by key version: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := keyStatusResponse{
		CurrentKeyVersion:     s.keys.currentVersion(),
		ConfiguredKeyVersions: s.keys.versions(),
		CredentialsByVersion:  map[string]int{},
		UnreadableKeyVersions: []int{},
	}
	for version, n := range counts {
		resp.CredentialsByVersion[strconv.Itoa(version)] = n
		if version != s.keys.currentVersion() {
			resp.PendingRotation += n
		}
		if !s.keys.has(version) {
			resp.UnreadableKeyVersions = append(resp.UnreadableKeyVersions, version)
		}
	}
	sort.Ints(resp.UnreadableKeyVersions)
	writeJSON(w, resp)
}

// handleKeyRotate re-seals every credential in the caller's tenant that is
// not already under the current key version: each value is opened with the
// key matching its recorded key_version and sealed again under the current
// key, in one transaction holding row locks so a concurrent value update
// cannot be overwritten with a stale plaintext. If any stored version has no
// configured key, nothing is re-sealed -- a partial rotation would let an
// operator believe the retired key is safe to drop.
func (s *credentialVaultServer) handleKeyRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentity(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}

	current := s.keys.currentVersion()
	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, key_version, ciphertext, nonce
		FROM credentials
		WHERE tenant_id = $1 AND key_version <> $2
		ORDER BY id
		FOR UPDATE
	`, identity.TenantID, current)
	if err != nil {
		http.Error(w, "load credentials for rotation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type sealed struct {
		id         string
		keyVersion int
		ciphertext []byte
		nonce      []byte
	}
	var pending []sealed
	for rows.Next() {
		var c sealed
		if err := rows.Scan(&c.id, &c.keyVersion, &c.ciphertext, &c.nonce); err != nil {
			rows.Close()
			http.Error(w, "scan credential: "+err.Error(), http.StatusInternalServerError)
			return
		}
		pending = append(pending, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		http.Error(w, "load credentials for rotation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows.Close()

	missing := map[int]bool{}
	for _, c := range pending {
		if !s.keys.has(c.keyVersion) {
			missing[c.keyVersion] = true
		}
	}
	if len(missing) > 0 {
		versions := make([]int, 0, len(missing))
		for v := range missing {
			versions = append(versions, v)
		}
		sort.Ints(versions)
		reason := fmt.Sprintf("rotation to key version %d refused: no key configured for stored key versions %v", current, versions)
		// Roll back the row locks before recording the refusal in a fresh
		// transaction, so the audit entry is durable even though no
		// credential changed.
		_ = tx.Rollback()
		if err := s.recordKeyringAudit(ctx, identity, "error", reason); err != nil {
			http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Error(w, reason, http.StatusConflict)
		return
	}

	resp := keyRotateResponse{CurrentKeyVersion: current, RotatedFrom: map[string]int{}}
	for _, c := range pending {
		plaintext, err := s.keys.open(c.keyVersion, c.ciphertext, c.nonce)
		if err != nil {
			_ = tx.Rollback()
			reason := fmt.Sprintf("rotation to key version %d aborted: credential %s could not be opened under key version %d", current, c.id, c.keyVersion)
			if auditErr := s.recordKeyringAudit(ctx, identity, "error", reason); auditErr != nil {
				http.Error(w, "record audit entry: "+auditErr.Error(), http.StatusInternalServerError)
				return
			}
			http.Error(w, reason, http.StatusInternalServerError)
			return
		}
		ciphertext, nonce, version, err := s.keys.seal(plaintext)
		if err != nil {
			http.Error(w, "re-seal credential: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE credentials SET ciphertext = $1, nonce = $2, key_version = $3
			WHERE id = $4 AND tenant_id = $5 AND key_version = $6
		`, ciphertext, nonce, version, c.id, identity.TenantID, c.keyVersion); err != nil {
			http.Error(w, "re-seal credential: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := insertAuditEntry(ctx, tx, identity, "credential_key_rotated", c.id, "allowed",
			fmt.Sprintf("re-sealed from key version %d to key version %d", c.keyVersion, version)); err != nil {
			http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Rotated++
		resp.RotatedFrom[strconv.Itoa(c.keyVersion)]++
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_keys_rotated", keyringResourceID(current), "allowed",
		fmt.Sprintf("re-sealed %d credential(s) to key version %d", resp.Rotated, current)); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	counts, err := credentialCountsByKeyVersion(ctx, tx, identity.TenantID)
	if err != nil {
		http.Error(w, "count credentials by key version: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for version, n := range counts {
		if version != current {
			resp.PendingRotation += n
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// recordKeyringAudit writes a standalone keyring audit entry in its own
// transaction, for outcomes where the rotation transaction itself is rolled
// back.
func (s *credentialVaultServer) recordKeyringAudit(ctx context.Context, identity requestIdentity, decision, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		return err
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_keys_rotated", keyringResourceID(s.keys.currentVersion()), decision, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func keyringResourceID(version int) string {
	return "keyring:v" + strconv.Itoa(version)
}

func credentialCountsByKeyVersion(ctx context.Context, tx *sql.Tx, tenantID string) (map[int]int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key_version, count(*) FROM credentials WHERE tenant_id = $1 GROUP BY key_version`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var version, n int
		if err := rows.Scan(&version, &n); err != nil {
			return nil, err
		}
		out[version] = n
	}
	return out, rows.Err()
}
