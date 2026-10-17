package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Secret-manager recovery (GOVERNANCE.md §31 "secret-manager recovery
// procedures"): every encrypted backup is worthless without the backup key,
// so the key is kept in Bitwarden Secrets Manager as well as in this
// deployment's environment, and a drill proves it can be recovered from
// there alone: it reads the secret back with bws, the Bitwarden Secrets
// Manager CLI, and decrypts the newest encrypted backup with the key it
// read -- not with the key the service is running with.
//
//	SCOS_BWS_ACCESS_TOKEN_FILE   file holding a machine-account access token
//	                             (or SCOS_BWS_ACCESS_TOKEN)
//	SCOS_BACKUP_KEY_SECRET_ID    the secret's id in Bitwarden
//	SCOS_BWS_SERVER_URL          a self-hosted or EU server; default US cloud
//	SCOS_SECRET_DRILL_INTERVAL   how often the scheduler drills, default 24h

// SecretRecoveryDrill is the record of one drill.
type SecretRecoveryDrill struct {
	Status      string `json:"status"` // verified | failed
	SecretID    string `json:"secret_id"`
	SecretKey   string `json:"secret_key,omitempty"`
	RevisedAt   string `json:"revised_at,omitempty"`
	BackupID    string `json:"backup_id,omitempty"`
	CompletedAt string `json:"completed_at"`
	Detail      string `json:"detail"`
}

// bwsSecret is the part of `bws secret get --output json` the drill reads.
type bwsSecret struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	Value        string `json:"value"`
	RevisionDate string `json:"revisionDate"`
}

func secretDrillConfig() (secretID, token string, err error) {
	secretID = strings.TrimSpace(os.Getenv("SCOS_BACKUP_KEY_SECRET_ID"))
	token = strings.TrimSpace(os.Getenv("SCOS_BWS_ACCESS_TOKEN"))
	if f := strings.TrimSpace(os.Getenv("SCOS_BWS_ACCESS_TOKEN_FILE")); f != "" {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			return secretID, "", fmt.Errorf("SCOS_BWS_ACCESS_TOKEN_FILE: %w", rerr)
		}
		token = strings.TrimSpace(string(b))
	}
	switch {
	case secretID == "" && token == "":
		return "", "", errors.New("no secret manager is configured (SCOS_BACKUP_KEY_SECRET_ID and SCOS_BWS_ACCESS_TOKEN_FILE)")
	case secretID == "":
		return "", "", errors.New("SCOS_BACKUP_KEY_SECRET_ID names no Bitwarden secret")
	case token == "":
		return secretID, "", errors.New("no Bitwarden Secrets Manager access token (SCOS_BWS_ACCESS_TOKEN_FILE)")
	}
	return secretID, token, nil
}

func (s *backupServer) secretDrillStatePath() string {
	return filepath.Join(s.backupDir, "secret-recovery-drill.json")
}

func (s *backupServer) lastSecretDrill() (*SecretRecoveryDrill, error) {
	data, err := os.ReadFile(s.secretDrillStatePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d SecretRecoveryDrill
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// fetchBitwardenSecret reads one secret with bws. The token goes to bws in
// its environment, never on its command line.
func fetchBitwardenSecret(ctx context.Context, secretID, token string) (bwsSecret, error) {
	var sec bwsSecret
	bin, err := lookPath("bws")
	if err != nil {
		return sec, fmt.Errorf("bws is not installed: %w", err)
	}
	args := []string{"secret", "get", secretID, "--output", "json"}
	if u := strings.TrimSpace(os.Getenv("SCOS_BWS_SERVER_URL")); u != "" {
		args = append(args, "--server-url", u)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := commandContext(ctx, bin, args...)
	cmd.Env = append(cmd.Environ(), "BWS_ACCESS_TOKEN="+token, "NO_COLOR=1")
	if home := os.Getenv("HOME"); home == "" || home == "/" {
		cmd.Env = append(cmd.Env, "HOME="+os.TempDir())
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return sec, fmt.Errorf("bws secret get %s: %v: %s", secretID, err, msg)
	}
	if err := json.Unmarshal(out, &sec); err != nil {
		return sec, fmt.Errorf("bws returned no secret: %w", err)
	}
	if sec.ID != secretID {
		return sec, fmt.Errorf("bws returned secret %q, not %q", sec.ID, secretID)
	}
	return sec, nil
}

// secretRecoveryDrill recovers the backup key from Bitwarden and decrypts
// the newest encrypted backup with it, then records the result.
func (s *backupServer) secretRecoveryDrill(ctx context.Context) (SecretRecoveryDrill, error) {
	d := SecretRecoveryDrill{Status: "failed"}
	finish := func(detail string) (SecretRecoveryDrill, error) {
		d.Detail = detail
		d.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		data, err := json.Marshal(d)
		if err != nil {
			return d, err
		}
		if err := os.MkdirAll(s.backupDir, 0o750); err != nil {
			return d, err
		}
		return d, writeAtomic(s.secretDrillStatePath(), data, 0o600)
	}
	secretID, token, err := secretDrillConfig()
	d.SecretID = secretID
	if err != nil {
		return d, err
	}
	p, ok := s.provider.(*filesystemProvider)
	if !ok || p == nil {
		return finish("the backup provider does not encrypt, so there is no backup key to recover")
	}
	sec, err := fetchBitwardenSecret(ctx, secretID, token)
	if err != nil {
		return finish(err.Error())
	}
	d.SecretKey, d.RevisedAt = sec.Key, sec.RevisionDate
	key, err := parseBackupKey(sec.Value)
	if err != nil {
		return finish(fmt.Sprintf("Bitwarden secret %s (%s) does not hold a backup key: %v", secretID, sec.Key, err))
	}
	if subtle.ConstantTimeCompare(key, p.key) != 1 {
		return finish(fmt.Sprintf("Bitwarden secret %s (%s) holds a different key from the one backups are encrypted with", secretID, sec.Key))
	}
	m, err := s.newestVerified(ctx)
	if err != nil {
		return finish("no encrypted backup to decrypt with the recovered key: " + err.Error())
	}
	artifact, err := os.ReadFile(p.archivePath(m.ID))
	if err == nil {
		var manifest []byte
		if manifest, err = os.ReadFile(p.manifestPath(m.ID)); err == nil {
			_, _, err = verifyEncrypted(key, m.ID, manifest, artifact)
		}
	}
	d.BackupID = m.ID
	if err != nil {
		return finish(fmt.Sprintf("the key recovered from Bitwarden does not open backup %s: %v", m.ID, err))
	}
	d.Status = "verified"
	return finish(fmt.Sprintf("backup key recovered from Bitwarden secret %s (%s, revised %s) authenticated and decrypted backup %s (%s)", secretID, sec.Key, sec.RevisionDate, m.ID, m.CreatedAt))
}

// secretDrillDue reports whether the scheduler should drill.
func (s *backupServer) secretDrillDue(now time.Time) bool {
	if _, _, err := secretDrillConfig(); err != nil {
		return false
	}
	last, err := s.lastSecretDrill()
	if err != nil || last == nil {
		return true
	}
	at, err := time.Parse(time.RFC3339, last.CompletedAt)
	return err != nil || now.Sub(at) >= envDuration("SCOS_SECRET_DRILL_INTERVAL", 24*time.Hour)
}

// secretRecoveryCoverage is secret_manager_recovery: a fresh drill
// recovered the key the backups are encrypted with from the configured
// secret and decrypted a backup with it.
func (s *backupServer) secretRecoveryCoverage(now time.Time) Coverage {
	c := Coverage{}
	secretID, _, err := secretDrillConfig()
	if err != nil {
		c.Reason = err.Error()
		return c
	}
	last, err := s.lastSecretDrill()
	switch {
	case err != nil:
		c.Reason = "the last secret-recovery drill cannot be read: " + err.Error()
		return c
	case last == nil:
		c.Reason = "no secret-recovery drill has run"
		return c
	case last.SecretID != secretID:
		c.Reason = fmt.Sprintf("the last drill read secret %s, not the configured %s", last.SecretID, secretID)
		return c
	case last.Status != "verified":
		c.Reason = fmt.Sprintf("the last secret-recovery drill at %s failed: %s", last.CompletedAt, last.Detail)
		return c
	}
	at, err := time.Parse(time.RFC3339, last.CompletedAt)
	if err != nil || now.Sub(at) > s.config.coverageMaxAge {
		c.Reason = fmt.Sprintf("the last verified secret-recovery drill at %s is older than the %s freshness bound", last.CompletedAt, s.config.coverageMaxAge)
		return c
	}
	c.Covered, c.LastSuccessAt, c.Evidence = true, last.CompletedAt, last.Detail
	return c
}
