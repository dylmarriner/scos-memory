package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Restore drills (GOVERNANCE.md §31): "automated integrity tests, monthly
// database restore tests and quarterly full recovery drills". Each type
// does what it names, validates data against the backup's manifest, and
// its cadence is satisfied only by a passing drill of that same type.
//
//	automated_integrity      verifies the stored archive against its
//	                         manifest (hash, MAC and decryption for
//	                         encrypted backups) and reads the archive's
//	                         table of contents, which must hold a data
//	                         entry for every table the manifest
//	                         fingerprinted. Nothing is restored.
//	monthly_restore          restores the archive into a temporary database
//	                         and compares the row count and checksum of
//	                         each key table with the manifest.
//	quarterly_full_recovery  restores the archive into a temporary database
//	                         and compares every table and the schema
//	                         catalog with the manifest; a table missing or
//	                         extra on either side fails the drill.
const (
	restoreTestTypeIntegrity = "automated_integrity"
	restoreTestTypeMonthly   = "monthly_restore"
	restoreTestTypeQuarterly = "quarterly_full_recovery"
	integrityCadenceInterval = 24 * time.Hour
	monthlyCadenceInterval   = 31 * 24 * time.Hour
	quarterlyCadenceInterval = 93 * 24 * time.Hour
)

var drillTypes = map[string]bool{
	restoreTestTypeIntegrity: true,
	restoreTestTypeMonthly:   true,
	restoreTestTypeQuarterly: true,
}

// defaultKeyTables are the tables a monthly restore test validates when
// SCOS_BACKUP_KEY_TABLES does not name them: the journal, the memory store,
// identity and curated knowledge.
var defaultKeyTables = []string{
	"tenants", "principals", "namespaces", "sessions", "turns", "messages", "events",
	"checkpoints", "memory_items", "memory_versions", "okf_proposals", "okf_concepts",
}

// runDrill executes one drill of testType against a loaded backup and
// returns what it validated.
func (s *backupServer) runDrill(ctx context.Context, testType string, m backupManifest, archive []byte) ([]string, error) {
	if m.Fingerprint == nil {
		return nil, fmt.Errorf("backup %s has no data fingerprint in its manifest, so its contents cannot be validated", m.ID)
	}
	switch testType {
	case restoreTestTypeIntegrity:
		return s.integrityDrill(ctx, m, archive)
	case restoreTestTypeMonthly:
		return s.restoreDrill(ctx, m, archive, false)
	case restoreTestTypeQuarterly:
		return s.restoreDrill(ctx, m, archive, true)
	}
	return nil, fmt.Errorf("unknown restore test type %q", testType)
}

// integrityDrill reads the archive's table of contents.
func (s *backupServer) integrityDrill(ctx context.Context, m backupManifest, archive []byte) ([]string, error) {
	validations := []string{fmt.Sprintf("archive verified against its manifest (sha256 %s, %d bytes)", shortHash(m.ContentHash), m.SizeBytes)}
	if m.Encrypted {
		validations = append(validations, "manifest MAC and AES-256-GCM decryption verified")
	}
	if _, err := lookPath("pg_restore"); err != nil {
		return validations, fmt.Errorf("pg_restore is unavailable: %w", err)
	}
	path, cleanup, err := tempArchive(archive)
	if err != nil {
		return validations, err
	}
	defer cleanup()
	out, err := commandContext(ctx, "pg_restore", "--list", path).CombinedOutput()
	if err != nil {
		return validations, fmt.Errorf("archive table of contents is unreadable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	entries, dataTables := parseTOC(string(out))
	var missing []string
	expected := 0
	for name, t := range m.Fingerprint.Tables {
		if t.Partitioned {
			continue
		}
		expected++
		if !dataTables[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return validations, fmt.Errorf("archive has no data entry for %d fingerprinted table(s): %s", len(missing), strings.Join(missing, ", "))
	}
	return append(validations, fmt.Sprintf("table of contents readable (%d entries); all %d fingerprinted tables have data entries", entries, expected)), nil
}

// parseTOC counts pg_restore --list entries and collects the public tables
// with a TABLE DATA entry.
func parseTOC(toc string) (int, map[string]bool) {
	entries := 0
	tables := map[string]bool{}
	for _, line := range strings.Split(toc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		entries++
		idx := strings.Index(line, " TABLE DATA ")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(line[idx+len(" TABLE DATA "):])
		if len(fields) >= 2 && fields[0] == "public" {
			tables[fields[1]] = true
		}
	}
	return entries, tables
}

// restoreDrill restores the archive into a temporary database and compares
// the restored data with the manifest: the key tables, or every table and
// the catalog when full.
func (s *backupServer) restoreDrill(ctx context.Context, m backupManifest, archive []byte, full bool) ([]string, error) {
	var validations []string
	keyTables := 0
	err := restoreDump(ctx, m.ID, archive, func(ctx context.Context, target string) error {
		var tables []string
		if !full {
			for _, name := range s.config.keyTables {
				if _, ok := m.Fingerprint.Tables[name]; ok {
					tables = append(tables, name)
				} else {
					validations = append(validations, name+": not in this backup")
				}
			}
			if len(tables) == 0 {
				return fmt.Errorf("none of the key tables (%s) is in the backup", strings.Join(s.config.keyTables, ", "))
			}
			keyTables = len(tables)
		}
		got, err := s.fingerprintTarget(ctx, target, tables, full)
		if err != nil {
			return err
		}
		checked, problems := compareFingerprints(m.Fingerprint, got, tables, full)
		validations = append(validations, checked...)
		if len(problems) > 0 {
			return fmt.Errorf("restored data does not match the manifest: %s", strings.Join(problems, "; "))
		}
		return nil
	})
	if err != nil {
		return validations, err
	}
	scope := fmt.Sprintf("%d key tables", keyTables)
	if full {
		scope = fmt.Sprintf("all %d tables and the schema catalog", len(m.Fingerprint.Tables))
	}
	return append(validations, "restored into a temporary database; "+scope+" match the manifest"), nil
}

// compareFingerprints compares restored data with the manifest. Without
// full, only tables are compared; with full, every manifest table, no
// extra restored table, and the catalog.
func compareFingerprints(want *dataFingerprint, got dataFingerprint, tables []string, full bool) (checked, problems []string) {
	if full {
		tables = tables[:0]
		for name := range want.Tables {
			tables = append(tables, name)
		}
		for name := range got.Tables {
			if _, ok := want.Tables[name]; !ok {
				problems = append(problems, name+": present after restore but not in the backup")
			}
		}
	}
	sort.Strings(tables)
	for _, name := range tables {
		w := want.Tables[name]
		g, ok := got.Tables[name]
		switch {
		case !ok:
			problems = append(problems, name+": missing after restore")
		case g.Rows != w.Rows:
			problems = append(problems, fmt.Sprintf("%s: %d rows restored, manifest has %d", name, g.Rows, w.Rows))
		case g.Checksum != w.Checksum:
			problems = append(problems, fmt.Sprintf("%s: checksum %s restored, manifest has %s", name, shortHash(g.Checksum), shortHash(w.Checksum)))
		default:
			checked = append(checked, fmt.Sprintf("%s: %d rows, checksum %s matches", name, g.Rows, shortHash(g.Checksum)))
		}
	}
	if full {
		if got.Catalog.Hash != want.Catalog.Hash {
			problems = append(problems, fmt.Sprintf("schema catalog differs (restored %d tables, %d functions, %d policies, %d triggers, %d indexes; manifest %d, %d, %d, %d, %d)",
				got.Catalog.Tables, got.Catalog.Functions, got.Catalog.Policies, got.Catalog.Triggers, got.Catalog.Indexes,
				want.Catalog.Tables, want.Catalog.Functions, want.Catalog.Policies, want.Catalog.Triggers, want.Catalog.Indexes))
		} else {
			checked = append(checked, fmt.Sprintf("schema catalog matches (%d tables, %d columns, %d functions, %d policies, %d triggers, %d indexes)",
				got.Catalog.Tables, got.Catalog.Columns, got.Catalog.Functions, got.Catalog.Policies, got.Catalog.Triggers, got.Catalog.Indexes))
		}
	}
	return checked, problems
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func tempArchive(archive []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "scos-restore-*")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err = f.Write(archive); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// restoreDump restores a pg_dump archive into a fresh temporary database on
// the restore server, runs verify against it, and drops it: the drill
// proves the archive restores without touching the live database.
func restoreDump(ctx context.Context, id string, archive []byte, verify func(ctx context.Context, targetURL string) error) error {
	restoreURL := strings.TrimSpace(os.Getenv("SCOS_BACKUP_RESTORE_DATABASE_URL"))
	if restoreURL == "" {
		return fmt.Errorf("SCOS_BACKUP_RESTORE_DATABASE_URL is required for a restore drill")
	}
	for _, tool := range []string{"createdb", "dropdb", "pg_restore"} {
		if _, err := lookPath(tool); err != nil {
			return fmt.Errorf("%s is unavailable: %w", tool, err)
		}
	}
	name := "scos_restore_" + strings.ReplaceAll(id, "-", "_") + fmt.Sprintf("_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if out, err := commandContext(ctx, "createdb", "--maintenance-db", restoreURL, name).CombinedOutput(); err != nil {
		return fmt.Errorf("temporary database creation failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer commandContext(context.Background(), "dropdb", "--if-exists", "--maintenance-db", restoreURL, name).Run()
	target, err := databaseWithName(restoreURL, name)
	if err != nil {
		return err
	}
	path, cleanup, err := tempArchive(archive)
	if err != nil {
		return err
	}
	defer cleanup()
	if out, err := commandContext(ctx, "pg_restore", "--exit-on-error", "--no-owner", "--dbname", target, path).CombinedOutput(); err != nil {
		return fmt.Errorf("temporary database restore failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return verify(ctx, target)
}
