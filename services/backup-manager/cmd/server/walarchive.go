package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// WAL archiving evidence. PostgreSQL's own archiver statistics say what it
// handed to archive_command; the archive destination says what arrived.
// Both are needed: the reference deployment's archive_command exits 0
// without copying anything while SCOS_WAL_ARCHIVE_ENABLED is false, so the
// server counts segments as archived that exist nowhere.

type walArchiverState struct {
	ArchiveMode      string
	ArchivedCount    int64
	LastArchivedWAL  string
	LastArchivedTime *time.Time
	FailedCount      int64
	LastFailedWAL    string
	LastFailedTime   *time.Time
	CurrentWAL       string
	SegmentSize      int64
}

func readWALArchiver(ctx context.Context, databaseURL string) (walArchiverState, error) {
	var st walArchiverState
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return st, err
	}
	defer conn.Close(context.Background())
	err = conn.QueryRow(ctx, `SELECT current_setting('archive_mode'), a.archived_count, COALESCE(a.last_archived_wal, ''), a.last_archived_time,
			a.failed_count, COALESCE(a.last_failed_wal, ''), a.last_failed_time,
			pg_walfile_name(pg_current_wal_lsn()), (SELECT setting::bigint FROM pg_settings WHERE name = 'wal_segment_size')
		FROM pg_stat_archiver a`).Scan(&st.ArchiveMode, &st.ArchivedCount, &st.LastArchivedWAL, &st.LastArchivedTime,
		&st.FailedCount, &st.LastFailedWAL, &st.LastFailedTime, &st.CurrentWAL, &st.SegmentSize)
	return st, err
}

// walSegmentNumber is the absolute segment number a WAL file name encodes
// (timeline, then log and segment, 8 hex digits each).
func walSegmentNumber(name string, segmentSize int64) (uint64, error) {
	if len(name) < 24 || segmentSize <= 0 {
		return 0, fmt.Errorf("%q is not a WAL segment name", name)
	}
	log, err := strconv.ParseUint(name[8:16], 16, 32)
	if err != nil {
		return 0, err
	}
	seg, err := strconv.ParseUint(name[16:24], 16, 32)
	if err != nil {
		return 0, err
	}
	perLog := uint64(0x100000000) / uint64(segmentSize)
	return log*perLog + seg, nil
}

// walCoverage decides whether continuous WAL archiving is happening.
func walCoverage(st walArchiverState, destination string, maxLag uint64) Coverage {
	c := Coverage{}
	if st.ArchiveMode != "on" && st.ArchiveMode != "always" {
		c.Reason = fmt.Sprintf("archive_mode is %q: PostgreSQL archives no WAL", st.ArchiveMode)
		return c
	}
	if st.ArchivedCount == 0 || st.LastArchivedWAL == "" {
		c.Reason = "PostgreSQL has not archived any WAL segment since its statistics were reset"
		return c
	}
	if st.LastFailedTime != nil && (st.LastArchivedTime == nil || st.LastFailedTime.After(*st.LastArchivedTime)) {
		c.Reason = fmt.Sprintf("WAL archiving is failing: %s failed at %s after the last success", st.LastFailedWAL, st.LastFailedTime.UTC().Format(time.RFC3339))
		return c
	}
	dir, err := walArchiveDir(destination)
	if err != nil {
		c.Reason = "the archived segments cannot be confirmed at their destination: " + err.Error()
		return c
	}
	info, err := os.Stat(filepath.Join(dir, st.LastArchivedWAL))
	if err != nil {
		c.Reason = fmt.Sprintf("PostgreSQL reports %s archived, but it is not at the destination %s (%v): archive_command reported success without copying it", st.LastArchivedWAL, dir, err)
		return c
	}
	if walSegmentFile.MatchString(st.LastArchivedWAL) && len(st.LastArchivedWAL) == 24 && info.Size() != st.SegmentSize {
		c.Reason = fmt.Sprintf("archived segment %s at the destination is %d bytes, not the %d-byte segment size", st.LastArchivedWAL, info.Size(), st.SegmentSize)
		return c
	}
	last, err := walSegmentNumber(st.LastArchivedWAL, st.SegmentSize)
	if err != nil {
		c.Reason = err.Error()
		return c
	}
	current, err := walSegmentNumber(st.CurrentWAL, st.SegmentSize)
	if err != nil {
		c.Reason = err.Error()
		return c
	}
	lag := uint64(0)
	if current > last {
		lag = current - last
	}
	if lag > maxLag {
		c.Reason = fmt.Sprintf("WAL archiving is %d segments behind the current segment %s (last archived %s); at most %d is tolerated", lag, st.CurrentWAL, st.LastArchivedWAL, maxLag)
		return c
	}
	c.Covered = true
	if st.LastArchivedTime != nil {
		c.LastSuccessAt = st.LastArchivedTime.UTC().Format(time.RFC3339)
	}
	c.Evidence = fmt.Sprintf("PostgreSQL archived %s (%d segments in all) and it is present at %s; %d segment(s) behind the current %s",
		st.LastArchivedWAL, st.ArchivedCount, dir, lag, st.CurrentWAL)
	return c
}
