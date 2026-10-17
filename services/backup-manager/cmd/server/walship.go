package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Encrypted off-host WAL. PostgreSQL's archive_command copies each finished
// segment to SCOS_WAL_ARCHIVE_DESTINATION, a directory on the database host
// (which already holds the live data in the clear). backup-manager ships
// every archived segment from there to the encrypted provider, under
// <destination>/wal/<segment>.enc, AES-256-GCM with the backup key -- so a
// network destination never holds WAL in the clear, and point-in-time
// recovery replays from those copies through `backup-manager wal-fetch`.

const walShipSuffix = ".enc"

// walShipDir is where encrypted WAL copies go, when the provider encrypts.
func (s *backupServer) walShipDir() (*filesystemProvider, string, bool) {
	p, ok := s.provider.(*filesystemProvider)
	if !ok || p == nil {
		return nil, "", false
	}
	return p, filepath.Join(p.dir, "wal"), true
}

// shipWAL encrypts every archived segment the provider does not hold yet.
func (s *backupServer) shipWAL(ctx context.Context) (int, error) {
	p, shipDir, ok := s.walShipDir()
	if !ok {
		return 0, errors.New("the backup provider does not encrypt, so WAL is not shipped")
	}
	src, err := walArchiveDir(s.config.walArchiveDestination)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(shipDir, 0o750); err != nil {
		return 0, err
	}
	shipped := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(walSegmentFile.MatchString(name) || strings.HasSuffix(name, ".history")) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return shipped, err
		}
		target := filepath.Join(shipDir, name+walShipSuffix)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		plain, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return shipped, err
		}
		enc, err := encryptBackup(p.key, plain)
		if err != nil {
			return shipped, err
		}
		if err := writeAtomic(target, enc, 0o600); err != nil {
			return shipped, fmt.Errorf("ship %s: %w", name, err)
		}
		shipped++
	}
	return shipped, nil
}

// fetchShippedWAL decrypts one shipped segment to dest: the restore_command
// of point-in-time recovery. A segment that was never shipped is reported
// as not found, which is how recovery learns it has reached the end.
func (s *backupServer) fetchShippedWAL(name, dest string) error {
	p, shipDir, ok := s.walShipDir()
	if !ok {
		return errors.New("the backup provider does not encrypt, so there is no shipped WAL")
	}
	if strings.ContainsAny(name, `/\`) || name == "" {
		return fmt.Errorf("%q is not a WAL file name", name)
	}
	enc, err := os.ReadFile(filepath.Join(shipDir, name+walShipSuffix))
	if err != nil {
		return err
	}
	plain, err := decryptBackup(p.key, enc)
	if err != nil {
		return fmt.Errorf("%s does not decrypt: %w", name, err)
	}
	return writeAtomic(dest, plain, 0o600)
}

// shippedWALPresent reports whether segment has an encrypted copy that
// decrypts.
func (s *backupServer) shippedWALPresent(segment string) error {
	p, shipDir, ok := s.walShipDir()
	if !ok {
		return errors.New("the backup provider does not encrypt")
	}
	enc, err := os.ReadFile(filepath.Join(shipDir, segment+walShipSuffix))
	if err != nil {
		return fmt.Errorf("no encrypted off-host copy of %s", segment)
	}
	if _, err := decryptBackup(p.key, enc); err != nil {
		return fmt.Errorf("the encrypted copy of %s does not decrypt: %w", segment, err)
	}
	return nil
}

// runWALShipper ships new segments every interval until ctx ends.
func (s *backupServer) runWALShipper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.walShipMu.Lock()
			n, err := s.shipWAL(ctx)
			s.walShipMu.Unlock()
			if err != nil {
				log.Printf("WAL shipper: %v", err)
			} else if n > 0 {
				log.Printf("WAL shipper: shipped %d segment(s)", n)
			}
		}
	}
}

// ensureShipped ships now and confirms segment's encrypted copy, when WAL
// shipping applies; otherwise it does nothing.
func (s *backupServer) ensureShipped(ctx context.Context, segment string) error {
	if _, _, ok := s.walShipDir(); !ok {
		return nil
	}
	s.walShipMu.Lock()
	_, err := s.shipWAL(ctx)
	s.walShipMu.Unlock()
	if err != nil {
		return fmt.Errorf("ship WAL: %w", err)
	}
	return s.shippedWALPresent(segment)
}

// walShipCoverage is encrypted_off_host_wal: the newest archived segment has
// an encrypted copy at an off-host provider that decrypts.
func (s *backupServer) walShipCoverage(ctx context.Context, wal Coverage, provider ProviderReport) Coverage {
	c := Coverage{}
	_, shipDir, ok := s.walShipDir()
	switch {
	case !wal.Covered:
		c.Reason = "WAL archiving is not covered: " + wal.Reason
		return c
	case !ok:
		c.Reason = "the backup provider does not encrypt, so archived WAL is not shipped off-host"
		return c
	case !provider.OffHost:
		c.Reason = "WAL is shipped encrypted to " + shipDir + ", which is on this host"
		return c
	}
	st, err := s.walState(ctx)
	if err != nil {
		c.Reason = "PostgreSQL's archiver statistics could not be read: " + err.Error()
		return c
	}
	if err := s.shippedWALPresent(st.LastArchivedWAL); err != nil {
		c.Reason = fmt.Sprintf("the newest archived segment has not been shipped: %v", err)
		return c
	}
	c.Covered = true
	if st.LastArchivedTime != nil {
		c.LastSuccessAt = st.LastArchivedTime.UTC().Format(time.RFC3339)
	}
	c.Evidence = fmt.Sprintf("archived WAL through %s is shipped AES-256-GCM encrypted to %s (%s) and decrypts", st.LastArchivedWAL, shipDir, provider.Detail)
	return c
}
