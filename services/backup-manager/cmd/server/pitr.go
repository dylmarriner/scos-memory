package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Point-in-time recovery (GOVERNANCE.md §31). Logical pg_dump archives
// cannot have WAL replayed onto them, so PITR needs a physical base backup
// (pg_basebackup) plus the continuous WAL archive from that backup onwards.
//
//	physical_base_backup  pg_basebackup in tar format with WAL left to the
//	                      archive (-X none). The stored artifact is a tar
//	                      holding base.tar.gz and backup_manifest; the
//	                      manifest records the WAL range the backup needs.
//	pitr_recovery         writes a marker, creates a named restore point,
//	                      writes a second marker, then restores the newest
//	                      physical base backup into a scratch cluster and
//	                      replays archived WAL up to that restore point.
//	                      It passes only if the first marker is present,
//	                      the second is absent, and the restored schema
//	                      catalog matches the live one -- recovery stopped
//	                      exactly where it was asked to.
const (
	backupTypeLogical   = "base_backup"
	backupTypePhysical  = "physical_base_backup"
	formatPhysical      = "postgresql-basebackup-tar-v1"
	restoreTestTypePITR = "pitr_recovery"
	pitrCadenceInterval = 7 * 24 * time.Hour
	// pitrWALWait bounds how long a backup or drill waits for PostgreSQL to
	// archive the segment it needs.
	pitrWALWait = 5 * time.Minute
	// pitrRecoveryWait bounds WAL replay in the scratch cluster.
	pitrRecoveryWait = 15 * time.Minute
)

// walRange is the WAL a physical base backup needs, from pg_basebackup's
// backup_manifest.
type walRange struct {
	Timeline int    `json:"Timeline"`
	StartLSN string `json:"Start-LSN"`
	EndLSN   string `json:"End-LSN"`
}

// parseLSN reads PostgreSQL's "X/Y" log sequence number notation.
func parseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fmt.Errorf("%q is not an LSN", s)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an LSN: %w", s, err)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an LSN: %w", s, err)
	}
	return h<<32 | l, nil
}

// walFileName is pg_walfile_name: the archived segment holding lsn.
func walFileName(timeline int, lsn uint64, segmentSize int64) (string, error) {
	if timeline <= 0 || segmentSize <= 0 {
		return "", fmt.Errorf("timeline %d and segment size %d must be positive", timeline, segmentSize)
	}
	segment := lsn / uint64(segmentSize)
	perLog := uint64(0x100000000) / uint64(segmentSize)
	return fmt.Sprintf("%08X%08X%08X", timeline, segment/perLog, segment%perLog), nil
}

// backupWALRange extracts the WAL range from a backup_manifest.
func backupWALRange(manifest []byte) (walRange, error) {
	var m struct {
		WALRanges []walRange `json:"WAL-Ranges"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return walRange{}, fmt.Errorf("backup_manifest is not valid JSON: %w", err)
	}
	if len(m.WALRanges) == 0 {
		return walRange{}, fmt.Errorf("backup_manifest names no WAL range")
	}
	// A backup taken on one timeline has one range; the last one is where
	// replay must reach.
	first, last := m.WALRanges[0], m.WALRanges[len(m.WALRanges)-1]
	return walRange{Timeline: last.Timeline, StartLSN: first.StartLSN, EndLSN: last.EndLSN}, nil
}

// waitForArchivedWAL waits until segment exists in the archive directory.
func waitForArchivedWAL(ctx context.Context, dir, segment string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if info, err := os.Stat(filepath.Join(dir, segment)); err == nil && info.Mode().IsRegular() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("WAL segment %s did not reach the archive at %s within %s", segment, dir, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// walArchivingReady returns the archive directory and the archiver state
// when continuous WAL archiving is demonstrably working.
func (s *backupServer) walArchivingReady(ctx context.Context) (string, walArchiverState, error) {
	if s.config.databaseURL == "" {
		return "", walArchiverState{}, fmt.Errorf("no database is configured")
	}
	st, err := s.walState(ctx)
	if err != nil {
		return "", st, fmt.Errorf("PostgreSQL's archiver statistics could not be read: %w", err)
	}
	if c := walCoverage(st, s.config.walArchiveDestination, s.config.walMaxSegmentLag); !c.Covered {
		return "", st, errors.New(c.Reason)
	}
	dir, err := walArchiveDir(s.config.walArchiveDestination)
	return dir, st, err
}

// createPhysicalBackup takes a pg_basebackup, waits until the WAL it needs is
// archived, stores it and reads it back. Callers hold s.opMu.
func (s *backupServer) createPhysicalBackup(ctx context.Context, actor string) (backupStartResponse, error) {
	if err := s.readinessError(ctx); err != nil {
		return backupStartResponse{}, &unavailableError{err}
	}
	walDir, st, err := s.walArchivingReady(ctx)
	if err != nil {
		return backupStartResponse{}, &unavailableError{fmt.Errorf("point-in-time base backups need working WAL archiving: %w", err)}
	}
	if _, err := lookPath("pg_basebackup"); err != nil {
		return backupStartResponse{}, &unavailableError{fmt.Errorf("pg_basebackup is unavailable: %w", err)}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp(s.backupDir, "basebackup-")
	if err != nil {
		return backupStartResponse{}, err
	}
	defer os.RemoveAll(work)
	out := filepath.Join(work, "out")
	id := s.nextID()
	cmd := commandContext(ctx, "pg_basebackup", "--dbname="+s.config.databaseURL, "--pgdata="+out, "--format=tar", "--gzip",
		"--wal-method=none", "--checkpoint=fast", "--manifest-checksums=SHA256", "--label=scos-"+id, "--no-password")
	if output, err := cmd.CombinedOutput(); err != nil {
		return backupStartResponse{}, fmt.Errorf("pg_basebackup failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	manifest, err := os.ReadFile(filepath.Join(out, "backup_manifest"))
	if err != nil {
		return backupStartResponse{}, fmt.Errorf("pg_basebackup wrote no backup_manifest: %w", err)
	}
	wr, err := backupWALRange(manifest)
	if err != nil {
		return backupStartResponse{}, err
	}
	start, err := s.walSegmentFor(wr.Timeline, wr.StartLSN, st.SegmentSize)
	if err != nil {
		return backupStartResponse{}, err
	}
	end, err := s.walSegmentFor(wr.Timeline, wr.EndLSN, st.SegmentSize)
	if err != nil {
		return backupStartResponse{}, err
	}
	// The backup restores only once the segment holding its end is archived.
	if err := waitForArchivedWAL(ctx, walDir, end, pitrWALWait); err != nil {
		return backupStartResponse{}, fmt.Errorf("physical backup %s is not restorable: %w", id, err)
	}
	archive, err := tarFiles(out, "base.tar.gz", "backup_manifest")
	if err != nil {
		return backupStartResponse{}, err
	}
	m := backupManifest{ID: id, Type: backupTypePhysical, Format: formatPhysical, ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)),
		Production: s.provider.Encrypted(), CreatedAt: time.Now().UTC().Format(time.RFC3339),
		WALTimeline: wr.Timeline, WALStartLSN: wr.StartLSN, WALEndLSN: wr.EndLSN, WALStart: start, WALEnd: end}
	if _, err := s.provider.Store(ctx, m, archive); err != nil {
		return backupStartResponse{}, fmt.Errorf("backup upload failed: %w", err)
	}
	stored, _, location, err := s.provider.Load(ctx, m.ID)
	if err != nil {
		return backupStartResponse{}, fmt.Errorf("stored backup %s does not verify: %w", m.ID, err)
	}
	resp := backupStartResponse{BackupRecord: recordFromManifest(stored, location, time.Now())}
	ret, err := s.applyRetention(ctx, actor, time.Now())
	if err != nil {
		resp.RetentionError = err.Error()
	} else {
		resp.Retention = &ret
	}
	return resp, nil
}

func (s *backupServer) walSegmentFor(timeline int, lsn string, segmentSize int64) (string, error) {
	v, err := parseLSN(lsn)
	if err != nil {
		return "", err
	}
	return walFileName(timeline, v, segmentSize)
}

// tarFiles packs the named files of dir into an uncompressed tar.
func tarFiles(dir string, names ...string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("pg_basebackup output is missing %s: %w", name, err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fileFromTar returns one member of an uncompressed tar.
func fileFromTar(archive []byte, name string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is not in the backup", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == name {
			return io.ReadAll(tr)
		}
	}
}

// extractDataDirectory unpacks pg_basebackup's base.tar.gz into dir, which
// PostgreSQL requires to be private to the user that runs it.
func extractDataDirectory(baseTarGz []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(baseTarGz))
	if err != nil {
		return fmt.Errorf("base.tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := filepath.Clean(dir) + string(os.PathSeparator)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("base.tar.gz: %w", err)
		}
		target := filepath.Join(dir, h.Name)
		if !strings.HasPrefix(target+string(os.PathSeparator), root) && target != filepath.Clean(dir) {
			return fmt.Errorf("base.tar.gz entry %q escapes the data directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("base.tar.gz entry %q has unsupported type %c (tablespaces are not supported)", h.Name, h.Typeflag)
		}
	}
	// -X none leaves pg_wal out; recovery fetches every segment through
	// restore_command, but the directory itself must exist.
	return os.MkdirAll(filepath.Join(dir, "pg_wal", "archive_status"), 0o700)
}

// postgresBinary finds the server binary the drill runs.
func postgresBinary() (string, error) {
	if p, err := lookPath("postgres"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/libexec/postgresql17/postgres", "/usr/lib/postgresql/17/bin/postgres"} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("the postgres server binary is not installed")
}

// pitrDrill replays archived WAL onto the newest physical base backup up to a
// restore point it creates, and checks recovery stopped exactly there.
func (s *backupServer) pitrDrill(ctx context.Context, m backupManifest, archive []byte) ([]string, error) {
	if m.Type != backupTypePhysical {
		return nil, fmt.Errorf("backup %s is a %s; a point-in-time recovery drill needs a %s", m.ID, m.Type, backupTypePhysical)
	}
	walDir, _, err := s.walArchivingReady(ctx)
	if err != nil {
		return nil, fmt.Errorf("point-in-time recovery needs working WAL archiving: %w", err)
	}
	if _, err := os.Stat(filepath.Join(walDir, m.WALStart)); err != nil {
		return nil, fmt.Errorf("WAL segment %s, where backup %s starts, is no longer in the archive", m.WALStart, m.ID)
	}
	bin, err := postgresBinary()
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(s.config.databaseURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, pitrRecoveryWait+pitrWALWait+5*time.Minute)
	defer cancel()

	// Mark, set the restore point, mark again, and push the segment holding
	// all three to the archive.
	live, err := pgx.Connect(ctx, s.config.databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to the live database: %w", err)
	}
	defer live.Close(context.Background())
	run := fmt.Sprintf("pitr-%d", time.Now().UnixNano())
	target := "scos-" + run
	defer func() {
		_, _ = live.Exec(context.Background(), `DELETE FROM scos_ops.pitr_markers WHERE run_id = $1`, run)
	}()
	if _, err := live.Exec(ctx, `INSERT INTO scos_ops.pitr_markers (run_id, phase) VALUES ($1, 'before')`, run); err != nil {
		return nil, fmt.Errorf("write the pre-restore-point marker (is migration 000059 applied?): %w", err)
	}
	var restoreLSN string
	if err := live.QueryRow(ctx, `SELECT pg_create_restore_point($1)::text`, target).Scan(&restoreLSN); err != nil {
		return nil, fmt.Errorf("create restore point: %w", err)
	}
	liveCatalog, err := fingerprintCatalog(ctx, live)
	if err != nil {
		return nil, fmt.Errorf("fingerprint the live schema: %w", err)
	}
	if _, err := live.Exec(ctx, `INSERT INTO scos_ops.pitr_markers (run_id, phase) VALUES ($1, 'after')`, run); err != nil {
		return nil, fmt.Errorf("write the post-restore-point marker: %w", err)
	}
	var segment string
	if err := live.QueryRow(ctx, `SELECT pg_walfile_name(pg_switch_wal())`).Scan(&segment); err != nil {
		return nil, fmt.Errorf("switch WAL: %w", err)
	}
	if err := waitForArchivedWAL(ctx, walDir, segment, pitrWALWait); err != nil {
		return nil, err
	}

	// Restore into a scratch cluster reachable only on a private socket.
	work, err := os.MkdirTemp(s.backupDir, "pitr-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	baseTar, err := fileFromTar(archive, "base.tar.gz")
	if err != nil {
		return nil, err
	}
	data, sock := filepath.Join(work, "data"), filepath.Join(work, "s")
	if err := extractDataDirectory(baseTar, data); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sock, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(data, "recovery.signal"), nil, 0o600); err != nil {
		return nil, err
	}
	logPath := filepath.Join(work, "postgres.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	srv := commandContext(ctx, bin, "-D", data,
		"-c", "listen_addresses=", "-c", "unix_socket_directories="+sock, "-c", "port=5432",
		"-c", "archive_mode=off", "-c", "hot_standby=off", "-c", "ssl=off", "-c", "logging_collector=off",
		"-c", "shared_buffers=32MB", "-c", "max_connections=20",
		"-c", fmt.Sprintf("restore_command=cp '%s/%%f' '%%p'", walDir),
		"-c", "recovery_target_name="+target, "-c", "recovery_target_action=promote")
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("start the scratch cluster: %w", err)
	}
	// exited closes when the server process ends; waitErr is its exit status.
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = srv.Wait(); close(exited) }()
	defer stopCluster(srv, exited)

	restoredURL := fmt.Sprintf("host=%s port=5432 user=%s dbname=%s sslmode=disable", sock, cfg.User, cfg.Database)
	restored, err := waitForPromotion(ctx, restoredURL, exited, func() error { return waitErr }, logPath)
	if err != nil {
		return nil, err
	}
	defer restored.Close(context.Background())

	rows, err := restored.Query(ctx, `SELECT phase FROM scos_ops.pitr_markers WHERE run_id = $1 ORDER BY phase`, run)
	if err != nil {
		return nil, fmt.Errorf("read markers from the recovered cluster: %w", err)
	}
	var phases []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		phases = append(phases, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if strings.Join(phases, ",") != "before" {
		return nil, fmt.Errorf("recovery to restore point %s did not stop there: markers present %v, want only [before]", target, phases)
	}
	restoredCatalog, err := fingerprintCatalog(ctx, restored)
	if err != nil {
		return nil, fmt.Errorf("fingerprint the recovered schema: %w", err)
	}
	if restoredCatalog.Hash != liveCatalog.Hash {
		return nil, fmt.Errorf("recovered schema catalog %s differs from the live catalog %s at the restore point", restoredCatalog.Hash[:12], liveCatalog.Hash[:12])
	}
	return []string{
		fmt.Sprintf("restored physical base backup %s (WAL %s..%s) into a scratch cluster", m.ID, m.WALStart, m.WALEnd),
		fmt.Sprintf("replayed archived WAL through %s to restore point %s at %s", segment, target, restoreLSN),
		"marker written before the restore point is present",
		"marker written after the restore point is absent",
		fmt.Sprintf("recovered schema catalog matches the live one (%d tables, %s)", restoredCatalog.Tables, restoredCatalog.Hash[:12]),
	}, nil
}

// waitForPromotion connects to the scratch cluster once it has finished
// recovery and promoted, or reports why it never did.
func waitForPromotion(ctx context.Context, url string, exited <-chan struct{}, exitErr func() error, logPath string) (*pgx.Conn, error) {
	deadline := time.Now().Add(pitrRecoveryWait)
	for {
		select {
		case <-exited:
			return nil, fmt.Errorf("the scratch cluster exited during recovery (%v): %s", exitErr(), logTail(logPath))
		default:
		}
		if conn, err := pgx.Connect(ctx, url); err == nil {
			var inRecovery bool
			if err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err == nil && !inRecovery {
				return conn, nil
			}
			conn.Close(context.Background())
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("recovery did not finish within %s: %s", pitrRecoveryWait, logTail(logPath))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// stopCluster shuts the scratch cluster down (fast shutdown), killing it if
// it does not stop.
func stopCluster(cmd *exec.Cmd, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "no server log"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}

// pitrCoverage derives pitr_configured from evidence: working WAL archiving,
// a fresh verified physical base backup whose starting WAL is still
// archived, and a passing pitr_recovery drill within its cadence.
func (s *backupServer) pitrCoverage(ctx context.Context, wal Coverage, now time.Time) Coverage {
	c := Coverage{}
	if !wal.Covered {
		c.Reason = "point-in-time recovery needs working WAL archiving: " + wal.Reason
		return c
	}
	m, err := s.newestVerifiedOfType(ctx, backupTypePhysical)
	if err != nil {
		c.Reason = "no verified physical base backup (pg_basebackup) exists to replay WAL onto: " + err.Error()
		return c
	}
	taken, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil || now.Sub(taken) > s.config.coverageMaxAge {
		c.Reason = fmt.Sprintf("the newest physical base backup %s (%s) is older than the %s freshness bound", m.ID, m.CreatedAt, s.config.coverageMaxAge)
		return c
	}
	if dir, err := walArchiveDir(s.config.walArchiveDestination); err != nil {
		c.Reason = err.Error()
		return c
	} else if _, err := os.Stat(filepath.Join(dir, m.WALStart)); err != nil {
		c.Reason = fmt.Sprintf("WAL segment %s, where physical base backup %s starts, is not in the archive", m.WALStart, m.ID)
		return c
	}
	var drill CadenceStatus
	for _, st := range restoreCadenceStatus(s.restoreTestSnapshot(), now) {
		if st.TestType == restoreTestTypePITR {
			drill = st
		}
	}
	if drill.LastPassedAt == "" {
		c.Reason = "no pitr_recovery drill has passed: WAL replay onto a physical base backup has not been demonstrated"
		return c
	}
	if drill.Overdue {
		c.Reason = fmt.Sprintf("the last passing pitr_recovery drill (%s) is outside its %d-hour cadence", drill.LastPassedAt, drill.IntervalHours)
		return c
	}
	c.Covered, c.LastSuccessAt = true, drill.LastPassedAt
	c.Evidence = fmt.Sprintf("physical base backup %s (%s, WAL from %s) with continuous WAL archiving; pitr_recovery drill replayed WAL to a restore point at %s",
		m.ID, m.CreatedAt, m.WALStart, drill.LastPassedAt)
	return c
}
