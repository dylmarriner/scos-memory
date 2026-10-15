package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A backup records, in its manifest, a fingerprint of the data it holds:
// the row count and an order-independent checksum of every table in the
// public schema, and a fingerprint of the schema catalog. The fingerprint
// and the pg_dump archive are taken from the same exported snapshot, so
// they describe exactly the same data; restore drills restore the archive
// and compare what came back against this fingerprint.

type tableFingerprint struct {
	Rows     int64  `json:"rows"`
	Checksum string `json:"checksum"`
	// Partitioned tables hold no data of their own (their partitions do),
	// so an archive has no TABLE DATA entry for them.
	Partitioned bool `json:"partitioned,omitempty"`
}

type catalogFingerprint struct {
	Tables    int    `json:"tables"`
	Columns   int    `json:"columns"`
	Functions int    `json:"functions"`
	Policies  int    `json:"policies"`
	Triggers  int    `json:"triggers"`
	Indexes   int    `json:"indexes"`
	Hash      string `json:"hash"`
}

type dataFingerprint struct {
	TakenAt string                      `json:"taken_at"`
	Tables  map[string]tableFingerprint `json:"tables"`
	Catalog catalogFingerprint          `json:"catalog"`
}

// pgQuerier is the part of pgx a fingerprint needs.
type pgQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// publicTables lists the ordinary and partitioned tables of the public
// schema, with whether each is partitioned.
func publicTables(ctx context.Context, q pgQuerier) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT c.relname, c.relkind = 'p' FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p') ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		var partitioned bool
		if err := rows.Scan(&name, &partitioned); err != nil {
			return nil, err
		}
		tables[name] = partitioned
	}
	return tables, rows.Err()
}

// fingerprintTable counts a table's rows and checksums them independently
// of physical order: the md5 of the sorted md5s of each row's text form.
func fingerprintTable(ctx context.Context, q pgQuerier, table string, partitioned bool) (tableFingerprint, error) {
	fp := tableFingerprint{Partitioned: partitioned}
	ident := pgx.Identifier{"public", table}.Sanitize()
	err := q.QueryRow(ctx, `SELECT count(*), md5(COALESCE(string_agg(h, '' ORDER BY h), '')) FROM (SELECT md5(t::text) AS h FROM `+ident+` t) rows`).Scan(&fp.Rows, &fp.Checksum)
	if err != nil {
		return fp, fmt.Errorf("fingerprint %s: %w", table, err)
	}
	return fp, nil
}

// fingerprintCatalog fingerprints the public schema's structure: tables and
// their columns, functions, row-level security policies, triggers and
// indexes. Owners and grants are not part of it (restores use --no-owner).
func fingerprintCatalog(ctx context.Context, q pgQuerier) (catalogFingerprint, error) {
	var c catalogFingerprint
	var entries []string
	collect := func(kind, sql string) (int, error) {
		rows, err := q.Query(ctx, sql)
		if err != nil {
			return 0, fmt.Errorf("catalog %s: %w", kind, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return 0, err
			}
			entries = append(entries, kind+":"+v)
			n++
		}
		return n, rows.Err()
	}
	var err error
	if c.Tables, err = collect("table", `SELECT c.relname || ':' || c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p')`); err != nil {
		return c, err
	}
	if c.Columns, err = collect("column", `SELECT c.relname || '.' || a.attname || ':' || format_type(a.atttypid, a.atttypmod) || ':' || a.attnotnull::text FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND a.attnum > 0 AND NOT a.attisdropped`); err != nil {
		return c, err
	}
	if c.Functions, err = collect("function", `SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'`); err != nil {
		return c, err
	}
	if c.Policies, err = collect("policy", `SELECT tablename || '.' || policyname FROM pg_policies WHERE schemaname='public'`); err != nil {
		return c, err
	}
	if c.Triggers, err = collect("trigger", `SELECT c.relname || '.' || t.tgname FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal`); err != nil {
		return c, err
	}
	if c.Indexes, err = collect("index", `SELECT indexname FROM pg_indexes WHERE schemaname='public'`); err != nil {
		return c, err
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	c.Hash = hex.EncodeToString(sum[:])
	return c, nil
}

// fingerprintDatabase fingerprints the named tables (every public table when
// tables is nil) and, when withCatalog, the schema catalog.
func fingerprintDatabase(ctx context.Context, q pgQuerier, tables []string, withCatalog bool) (dataFingerprint, error) {
	fp := dataFingerprint{TakenAt: time.Now().UTC().Format(time.RFC3339), Tables: map[string]tableFingerprint{}}
	present, err := publicTables(ctx, q)
	if err != nil {
		return fp, err
	}
	if tables == nil {
		for name := range present {
			tables = append(tables, name)
		}
		sort.Strings(tables)
	}
	for _, name := range tables {
		partitioned, ok := present[name]
		if !ok {
			continue
		}
		t, err := fingerprintTable(ctx, q, name, partitioned)
		if err != nil {
			return fp, err
		}
		fp.Tables[name] = t
	}
	if withCatalog {
		if fp.Catalog, err = fingerprintCatalog(ctx, q); err != nil {
			return fp, err
		}
	}
	return fp, nil
}

// snapshotDump takes a pg_dump custom-format archive of databaseURL and the
// data fingerprint of the same exported snapshot. Row security is turned
// off for the fingerprint, as pg_dump does for the archive, so a role that
// cannot read every row fails loudly instead of fingerprinting a subset.
func snapshotDump(ctx context.Context, databaseURL string) ([]byte, dataFingerprint, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, dataFingerprint{}, fmt.Errorf("connect to the database to back up: %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, dataFingerprint{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL row_security = off`); err != nil {
		return nil, dataFingerprint{}, err
	}
	var snapshot string
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snapshot); err != nil {
		return nil, dataFingerprint{}, fmt.Errorf("export snapshot: %w", err)
	}
	f, err := os.CreateTemp("", "scos-pg-dump-*")
	if err != nil {
		return nil, dataFingerprint{}, err
	}
	path := f.Name()
	_ = f.Close()
	defer os.Remove(path)
	type dumpOutcome struct {
		out []byte
		err error
	}
	done := make(chan dumpOutcome, 1)
	go func() {
		out, err := commandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--snapshot="+snapshot, "--file", path, databaseURL).CombinedOutput()
		done <- dumpOutcome{out, err}
	}()
	fp, fpErr := fingerprintDatabase(ctx, tx, nil, true)
	outcome := <-done
	if outcome.err != nil {
		return nil, fp, fmt.Errorf("pg_dump failed: %v: %s", outcome.err, strings.TrimSpace(string(outcome.out)))
	}
	if fpErr != nil {
		return nil, fp, fpErr
	}
	archive, err := os.ReadFile(path)
	if err != nil {
		return nil, fp, err
	}
	if len(archive) == 0 {
		return nil, fp, fmt.Errorf("pg_dump produced an empty archive")
	}
	return archive, fp, nil
}

// fingerprintURL connects to a (restored) database and fingerprints it.
func fingerprintURL(ctx context.Context, databaseURL string, tables []string, withCatalog bool) (dataFingerprint, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return dataFingerprint{}, fmt.Errorf("connect to the restored database: %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return dataFingerprint{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL row_security = off`); err != nil {
		return dataFingerprint{}, err
	}
	return fingerprintDatabase(ctx, tx, tables, withCatalog)
}
