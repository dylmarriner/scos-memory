package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Migration exports (GOVERNANCE.md §31 "schema and migration exports"). A
// pg_dump archive holds the schema as it is, not how it came to be; the
// migration and policy files scos-migrate applied are what rebuild it on
// an empty server and what the next migration builds on. Every base backup
// is followed by a migration_export: the SQL files under
// SCOS_MIGRATION_EXPORT_PATHS (default /scos-database/migrations and
// /scos-database/policies, mounted read only) with their SHA-256 digests,
// linked to the base backup and recording which versions
// scos_schema_migrations listed as applied when it was taken.
const backupTypeMigrations = "migration_export"

func migrationExportPaths() []string {
	raw := os.Getenv("SCOS_MIGRATION_EXPORT_PATHS")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

// appliedMigrations lists the versions scos-migrate recorded as applied.
func appliedMigrations(ctx context.Context, databaseURL string) ([]string, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT version FROM scos_schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// exportMigrations stores the migration export for base backup baseID and
// reads it back. Callers hold s.opMu.
func (s *backupServer) exportMigrations(ctx context.Context, baseID string) (string, error) {
	paths := migrationExportPaths()
	if len(paths) == 0 {
		return "", fmt.Errorf("SCOS_MIGRATION_EXPORT_PATHS names no migration directories")
	}
	applied, err := s.applied(ctx, s.config.databaseURL)
	if err != nil {
		return "", fmt.Errorf("read applied migrations: %w", err)
	}
	archive, files, err := archiveConfig(paths)
	if err != nil {
		return "", err
	}
	m := backupManifest{ID: s.nextID(), Type: backupTypeMigrations, Format: formatConfig, ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)),
		Production: s.provider.Encrypted(), CreatedAt: time.Now().UTC().Format(time.RFC3339), Files: files, Sources: paths,
		ExportOf: baseID, AppliedMigrations: applied}
	if _, err := s.provider.Store(ctx, m, archive); err != nil {
		return "", fmt.Errorf("store migration export: %w", err)
	}
	stored, plain, _, err := s.provider.Load(ctx, m.ID)
	if err != nil {
		return "", fmt.Errorf("stored migration export %s does not verify: %w", m.ID, err)
	}
	if _, err := unpackConfig(plain, stored.Files); err != nil {
		return "", fmt.Errorf("stored migration export %s does not unpack: %w", m.ID, err)
	}
	if missing := missingMigrationFiles(stored); len(missing) > 0 {
		return m.ID, fmt.Errorf("migration export %s lacks files for applied versions %s", m.ID, strings.Join(missing, ", "))
	}
	return m.ID, nil
}

// missingMigrationFiles lists applied versions with no <version>.sql file.
func missingMigrationFiles(m backupManifest) []string {
	have := map[string]bool{}
	for _, f := range m.Files {
		have[strings.TrimSuffix(filepath.Base(f.Path), ".sql")] = true
	}
	var missing []string
	for _, v := range m.AppliedMigrations {
		if !have[v] {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	return missing
}

// migrationCoverage is schema_and_migration_exports: the newest base backup
// (which carries the schema) has a migration export that unpacks against its
// digests and holds a file for every migration applied when it was taken.
func (s *backupServer) migrationCoverage(ctx context.Context, schema Coverage) Coverage {
	c := Coverage{}
	if !schema.Covered {
		c.Reason = "the schema is not exported: " + schema.Reason
		return c
	}
	if len(migrationExportPaths()) == 0 {
		c.Reason = "SCOS_MIGRATION_EXPORT_PATHS names no migration directories, so migrations are not exported"
		return c
	}
	base, err := s.newestVerified(ctx)
	if err != nil {
		c.Reason = err.Error()
		return c
	}
	ids, err := s.provider.List(ctx)
	if err != nil {
		c.Reason = err.Error()
		return c
	}
	for _, id := range ids {
		m, plain, _, err := s.provider.Load(ctx, id)
		if err != nil || m.Type != backupTypeMigrations || m.ExportOf != base.ID {
			continue
		}
		if _, err := unpackConfig(plain, m.Files); err != nil {
			c.Reason = fmt.Sprintf("migration export %s of backup %s does not unpack: %v", m.ID, base.ID, err)
			return c
		}
		if len(m.AppliedMigrations) == 0 {
			c.Reason = fmt.Sprintf("migration export %s recorded no applied migrations", m.ID)
			return c
		}
		if missing := missingMigrationFiles(m); len(missing) > 0 {
			c.Reason = fmt.Sprintf("migration export %s lacks files for applied versions %s", m.ID, strings.Join(missing, ", "))
			return c
		}
		c.Covered, c.LastSuccessAt = true, m.CreatedAt
		c.Evidence = fmt.Sprintf("backup %s exports the schema and migration export %s holds %d migration and policy files covering all %d versions applied when it was taken (newest %s)",
			base.ID, m.ID, len(m.Files), len(m.AppliedMigrations), m.AppliedMigrations[len(m.AppliedMigrations)-1])
		return c
	}
	c.Reason = fmt.Sprintf("the newest base backup %s has no migration export", base.ID)
	return c
}
