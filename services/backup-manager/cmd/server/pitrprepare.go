package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pitrPreparation reports what pitrPrepare laid out.
type pitrPreparation struct {
	BackupID        string
	BackupTakenAt   string
	WALStart        string
	ArchivedThrough string
	Segments        int
	Target          time.Time
}

// pitrPrepare lays out a data directory that recovers the database to
// target when PostgreSQL starts on it: the newest verified physical base
// backup taken before target, unpacked into dir, with recovery.signal and a
// recovery configuration that replays archived WAL up to target and then
// promotes. It refuses unless every WAL segment from the backup's start to
// the newest archived one on its timeline is in the archive, so recovery
// cannot stop short of target for want of WAL. restoreWALDir is the archive
// directory as the PostgreSQL that will run the recovery sees it.
func (s *backupServer) pitrPrepare(ctx context.Context, target time.Time, dir, restoreWALDir string) (pitrPreparation, error) {
	if s.provider == nil {
		return pitrPreparation{}, s.providerErr
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return pitrPreparation{}, fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return pitrPreparation{}, err
	}
	walDir, err := walArchiveDir(s.config.walArchiveDestination)
	if err != nil {
		return pitrPreparation{}, err
	}
	if restoreWALDir == "" {
		restoreWALDir = walDir
	}

	ids, err := s.provider.List(ctx)
	if err != nil {
		return pitrPreparation{}, err
	}
	var chosen backupManifest
	var chosenAt time.Time
	var archive []byte
	for _, id := range ids {
		m, _, _, err := s.provider.Load(ctx, id)
		if err != nil || m.Type != backupTypePhysical {
			continue
		}
		at, err := time.Parse(time.RFC3339, m.CreatedAt)
		// CreatedAt is when the backup finished; recovery can stop only
		// after the backup's end, so it must precede target.
		if err != nil || !at.Before(target) {
			continue
		}
		if at.After(chosenAt) || at.Equal(chosenAt) && m.ID > chosen.ID {
			chosen, chosenAt = m, at
		}
	}
	if chosenAt.IsZero() {
		return pitrPreparation{}, fmt.Errorf("no verified %s was taken before %s", backupTypePhysical, target.Format(time.RFC3339))
	}
	// Load again for the archive of the chosen backup only: each backup is
	// held in memory once, not all of them at the same time.
	if _, archive, _, err = s.provider.Load(ctx, chosen.ID); err != nil {
		return pitrPreparation{}, fmt.Errorf("load backup %s: %w", chosen.ID, err)
	}

	through, count, err := contiguousArchivedWAL(walDir, chosen.WALStart)
	if err != nil {
		return pitrPreparation{}, err
	}

	baseTar, err := fileFromTar(archive, "base.tar.gz")
	if err != nil {
		return pitrPreparation{}, err
	}
	if err := extractDataDirectory(baseTar, dir); err != nil {
		return pitrPreparation{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "recovery.signal"), nil, 0o600); err != nil {
		return pitrPreparation{}, err
	}
	conf := fmt.Sprintf(`
# Point-in-time recovery written by backup-manager pitr-prepare from backup %s.
# Remove these settings with ALTER SYSTEM RESET once the server has promoted.
restore_command = 'cp ''%s/%%f'' ''%%p'''
recovery_target_time = '%s'
recovery_target_inclusive = off
recovery_target_action = 'promote'
`, chosen.ID, restoreWALDir, target.UTC().Format("2006-01-02 15:04:05.000000-07"))
	f, err := os.OpenFile(filepath.Join(dir, "postgresql.auto.conf"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return pitrPreparation{}, err
	}
	if _, err := f.WriteString(conf); err != nil {
		f.Close()
		return pitrPreparation{}, err
	}
	if err := f.Close(); err != nil {
		return pitrPreparation{}, err
	}
	return pitrPreparation{
		BackupID: chosen.ID, BackupTakenAt: chosen.CreatedAt, WALStart: chosen.WALStart,
		ArchivedThrough: through, Segments: count, Target: target,
	}, nil
}

// contiguousArchivedWAL checks that the archive in dir holds every segment
// from start to the newest archived segment on start's timeline, and
// returns that newest segment and how many there are.
func contiguousArchivedWAL(dir, start string) (string, int, error) {
	if len(start) != 24 {
		return "", 0, fmt.Errorf("%q is not a WAL segment name", start)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, fmt.Errorf("read the WAL archive: %w", err)
	}
	timeline := start[:8]
	var names []string
	for _, e := range entries {
		n := e.Name()
		if len(n) == 24 && strings.HasPrefix(n, timeline) && n >= start {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 || names[0] != start {
		return "", 0, fmt.Errorf("WAL segment %s, where the backup starts, is not in the archive", start)
	}
	// Every archived segment is exactly one segment long, so the first one's
	// size is the cluster's segment size; consecutive segments then differ
	// by one in absolute segment number.
	info, err := os.Stat(filepath.Join(dir, start))
	if err != nil {
		return "", 0, err
	}
	segmentSize := info.Size()
	prev, err := walSegmentNumber(names[0], segmentSize)
	if err != nil {
		return "", 0, err
	}
	for _, n := range names[1:] {
		cur, err := walSegmentNumber(n, segmentSize)
		if err != nil {
			return "", 0, err
		}
		if cur != prev+1 {
			return "", 0, fmt.Errorf("the WAL archive has a gap before %s: recovery could not replay past it", n)
		}
		prev = cur
	}
	return names[len(names)-1], len(names), nil
}
