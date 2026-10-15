package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The backup plan (GOVERNANCE.md §31) reports, item by item, what this
// deployment actually does. Nothing is taken from configuration flags: an
// item is covered only by evidence -- a provider that just accepted a
// durable write, a backup that verifies against its manifest and is recent,
// PostgreSQL's archiver statistics confirmed by the archived files, a
// mirror run whose refs were read back from the remote. An item with no
// implementation is reported not covered, with the reason.

// Coverage is one plan item.
type Coverage struct {
	Covered       bool   `json:"covered"`
	Evidence      string `json:"evidence,omitempty"`
	Reason        string `json:"reason,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
}

// ProviderReport is the configured provider as it stands now.
type ProviderReport struct {
	Name        string `json:"name"`
	Destination string `json:"destination,omitempty"`
	Encrypted   bool   `json:"encrypted"`
	Working     bool   `json:"working"`
	OffHost     bool   `json:"off_host"`
	Detail      string `json:"detail"`
	// KeySource names where the deployment says the encryption key is
	// kept (SCOS_SECRET_MANAGER, SCOS_BACKUP_ENCRYPTION_KEY_REF). It is a
	// label, not evidence: secret_manager_recovery stays not covered.
	KeySource string `json:"key_source,omitempty"`
}

// BackupPlan keeps the §31 item keys as booleans (derived from Items) so
// existing readers see the same shape; Items carries the evidence.
type BackupPlan struct {
	PostgreSQLWAL    bool                `json:"postgresql_wal_archiving"`
	BaseBackups      bool                `json:"base_backups"`
	EncryptedOffHost bool                `json:"encrypted_off_host_copies"`
	ObjectStoreSnaps bool                `json:"object_store_snapshots"`
	GitMirrors       bool                `json:"remote_git_mirrors"`
	SchemaExports    bool                `json:"schema_and_migration_exports"`
	SecretRecovery   bool                `json:"secret_manager_recovery"`
	ConfigBackups    bool                `json:"configuration_backups"`
	RemoteVerified   bool                `json:"remote_provider_verified"`
	PITRConfigured   bool                `json:"pitr_configured"`
	Provider         ProviderReport      `json:"provider"`
	Items            map[string]Coverage `json:"coverage"`
	EvaluatedAt      string              `json:"evaluated_at"`
}

// newestVerified returns the manifest of the newest backup that verifies.
func (s *backupServer) newestVerified(ctx context.Context) (backupManifest, error) {
	if s.provider == nil {
		return backupManifest{}, s.providerErr
	}
	ids, err := s.provider.List(ctx)
	if err != nil {
		return backupManifest{}, err
	}
	var newest backupManifest
	var newestAt time.Time
	for _, id := range ids {
		m, _, _, err := s.provider.Load(ctx, id)
		if err != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			continue
		}
		if at.After(newestAt) {
			newest, newestAt = m, at
		}
	}
	if newestAt.IsZero() {
		return backupManifest{}, errNoBackups
	}
	return newest, nil
}

// evaluatePlan derives every plan item from current evidence.
func (s *backupServer) evaluatePlan(ctx context.Context, now time.Time) BackupPlan {
	plan := BackupPlan{Items: map[string]Coverage{}, EvaluatedAt: now.UTC().Format(time.RFC3339)}

	// Provider.
	working := false
	if s.provider == nil {
		plan.Provider = ProviderReport{Name: s.config.provider, Detail: s.providerErr.Error()}
	} else {
		plan.Provider = ProviderReport{Name: s.provider.Name(), Destination: s.provider.Destination(), Encrypted: s.provider.Encrypted()}
		if s.provider.Encrypted() && (s.config.secretManager != "" || s.config.encryptionKeyRef != "") {
			plan.Provider.KeySource = strings.TrimSpace(s.config.secretManager + " " + s.config.encryptionKeyRef)
		}
		if err := s.provider.Verify(ctx); err != nil {
			plan.Provider.Detail = "destination did not accept a verified write: " + err.Error()
		} else {
			working = true
			offHost, evidence := s.provider.OffHost()
			plan.Provider.Working, plan.Provider.OffHost = true, offHost
			plan.Provider.Detail = "destination accepted, returned and removed a verified write; " + evidence
		}
	}

	// Base backups: a recent backup on a working provider that verifies.
	base := Coverage{}
	var newest backupManifest
	switch {
	case !working:
		base.Reason = "no working backup provider: " + plan.Provider.Detail
	default:
		m, err := s.newestVerified(ctx)
		if err != nil {
			base.Reason = "no backup verifies against its manifest: " + err.Error()
			break
		}
		newest = m
		taken, _ := time.Parse(time.RFC3339, m.CreatedAt)
		base.LastSuccessAt = m.CreatedAt
		if age := now.Sub(taken); age > s.config.coverageMaxAge {
			base.Reason = fmt.Sprintf("the newest verified backup %s was taken %s ago, more than the %s freshness bound", m.ID, age.Round(time.Minute), s.config.coverageMaxAge)
			break
		}
		base.Covered = true
		base.Evidence = fmt.Sprintf("backup %s taken %s verifies against its manifest (%d bytes, sha256 %s)", m.ID, m.CreatedAt, m.SizeBytes, shortHash(m.ContentHash))
	}
	plan.Items["base_backups"] = base

	// Schema exports: every base backup is a pg_dump archive holding the
	// full schema.
	schema := Coverage{Covered: base.Covered, LastSuccessAt: base.LastSuccessAt}
	if base.Covered {
		schema.Evidence = fmt.Sprintf("the schema is exported in backup %s (pg_dump custom format; catalog fingerprint %s)", newest.ID, catalogSummary(newest))
	} else {
		schema.Reason = "no recent verified base backup carries the schema: " + base.Reason
	}
	plan.Items["schema_exports"] = schema
	plan.Items["schema_and_migration_exports"] = Coverage{Reason: "migration exports are not implemented: the schema alone is exported, inside every base backup (see schema_exports)"}

	// Encrypted off-host copies and a verified remote provider.
	offHost := Coverage{}
	remote := Coverage{}
	switch {
	case !working:
		offHost.Reason = base.Reason
		remote.Reason = plan.Provider.Detail
	case !plan.Provider.OffHost:
		offHost.Reason = "backups are written to this host: " + plan.Provider.Detail
		remote.Reason = offHost.Reason
	case !s.provider.Encrypted():
		offHost.Reason = "the provider writes unencrypted archives"
		remote = Coverage{Covered: true, Evidence: plan.Provider.Detail}
	default:
		remote = Coverage{Covered: true, Evidence: plan.Provider.Detail}
		if base.Covered {
			offHost = Coverage{Covered: true, LastSuccessAt: base.LastSuccessAt, Evidence: "encrypted (AES-256-GCM) " + base.Evidence + " on another host: " + plan.Provider.Detail}
		} else {
			offHost.Reason = base.Reason
		}
	}
	plan.Items["encrypted_off_host_copies"] = offHost
	plan.Items["remote_provider_verified"] = remote

	plan.Items["object_store_snapshots"] = Coverage{Reason: "no object-store provider is implemented; object-store versioning and snapshots are not taken"}

	// Continuous WAL archiving.
	wal := Coverage{}
	if s.config.databaseURL == "" {
		wal.Reason = "no database is configured to read PostgreSQL's archiver statistics from"
	} else if st, err := s.walState(ctx); err != nil {
		wal.Reason = "PostgreSQL's archiver statistics could not be read: " + err.Error()
	} else {
		wal = walCoverage(st, s.config.walArchiveDestination, s.config.walMaxSegmentLag)
	}
	plan.Items["postgresql_wal_archiving"] = wal

	// Point-in-time recovery needs a physical base backup to replay WAL
	// onto; this service takes logical ones.
	plan.Items["pitr_configured"] = Coverage{Reason: "base backups are logical pg_dump archives, onto which WAL cannot be replayed; point-in-time recovery needs a physical base backup (pg_basebackup), which this service does not take"}

	// Remote Git mirror.
	mirror := Coverage{}
	if offHostRemote, why := remoteIsOffHost(s.config.gitMirrorRemote); !offHostRemote {
		mirror.Reason = "no off-host OKF mirror: " + why
	} else if run, err := s.lastGitMirrorRun(); err != nil {
		mirror.Reason = "the last mirror run cannot be read: " + err.Error()
	} else if run == nil {
		mirror.Reason = "no mirror run has been recorded"
	} else if run.Status != "verified" {
		mirror.Reason = fmt.Sprintf("the last mirror run at %s failed: %s", run.CompletedAt, run.Detail)
	} else if run.Remote != s.config.gitMirrorRemote || run.Repository != s.config.gitRepo {
		mirror.Reason = "the last verified mirror run was for a different repository or remote than is configured now"
	} else if completed, err := time.Parse(time.RFC3339, run.CompletedAt); err != nil || now.Sub(completed) > s.config.mirrorMaxAge {
		mirror.Reason = fmt.Sprintf("the last verified mirror run at %s is older than the %s freshness bound", run.CompletedAt, s.config.mirrorMaxAge)
	} else {
		mirror = Coverage{Covered: true, LastSuccessAt: run.CompletedAt, Evidence: fmt.Sprintf("%s to %s at %s (HEAD %s)", run.Detail, run.Remote, run.CompletedAt, shortHash(run.Head))}
	}
	plan.Items["remote_git_mirrors"] = mirror

	plan.Items["secret_manager_recovery"] = Coverage{Reason: "secret-manager recovery procedures are not implemented or exercised by this service"}
	plan.Items["configuration_backups"] = Coverage{Reason: "configuration backups are not implemented"}

	plan.BaseBackups = plan.Items["base_backups"].Covered
	plan.SchemaExports = plan.Items["schema_and_migration_exports"].Covered
	plan.EncryptedOffHost = plan.Items["encrypted_off_host_copies"].Covered
	plan.RemoteVerified = plan.Items["remote_provider_verified"].Covered
	plan.ObjectStoreSnaps = plan.Items["object_store_snapshots"].Covered
	plan.PostgreSQLWAL = plan.Items["postgresql_wal_archiving"].Covered
	plan.PITRConfigured = plan.Items["pitr_configured"].Covered
	plan.GitMirrors = plan.Items["remote_git_mirrors"].Covered
	plan.SecretRecovery = plan.Items["secret_manager_recovery"].Covered
	plan.ConfigBackups = plan.Items["configuration_backups"].Covered
	return plan
}

func catalogSummary(m backupManifest) string {
	if m.Fingerprint == nil {
		return "absent"
	}
	c := m.Fingerprint.Catalog
	return fmt.Sprintf("%s: %d tables, %d functions, %d policies", shortHash(c.Hash), c.Tables, c.Functions, c.Policies)
}
