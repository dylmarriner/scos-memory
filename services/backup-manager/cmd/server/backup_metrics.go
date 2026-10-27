package main

import (
	"context"
	"errors"
	"log"
	"sort"
	"time"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// emitBackupAge uses only provider-verified backup records. A missing or
// unverifiable backup must not be represented by a fabricated zero-age gauge.
func (s *backupServer) emitBackupAge(backups []*BackupRecord, now time.Time) {
	if age, ok := newestVerifiedBackupAge(backups, now); ok && age >= 0 {
		s.metrics.Gauge(telemetry.MetricBackupAge, age.Seconds(), "s",
			map[string]string{"provider": s.provider.Name()}, telemetry.Correlation{})
	}
}

// reportBackupAge verifies the newest available backup without decrypting
// every historical archive on each heartbeat. Manifest() is cheap (and HMAC
// authenticated for encrypted providers); Load() proves the selected archive
// actually matches its manifest. A corrupt newest archive falls back to the
// next newest candidate. This is a freshness observation, not a restore drill.
func (s *backupServer) reportBackupAge(ctx context.Context) error {
	if s.provider == nil {
		return errors.New("backup provider unavailable")
	}
	ids, err := s.provider.List(ctx)
	if err != nil {
		return err
	}
	type candidate struct {
		id    string
		taken time.Time
	}
	candidates := make([]candidate, 0, len(ids))
	now := time.Now()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := s.provider.Manifest(ctx, id)
		if err != nil {
			continue
		}
		taken, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil || taken.After(now) {
			continue
		}
		candidates = append(candidates, candidate{id: id, taken: taken})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].taken.Equal(candidates[j].taken) {
			return candidates[i].id > candidates[j].id
		}
		return candidates[i].taken.After(candidates[j].taken)
	})
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, _, _, err := s.provider.Load(ctx, c.id)
		if err != nil {
			continue
		}
		// The verified manifest, not an unauthenticated listing, determines
		// the timestamp used in the emitted gauge.
		verifiedAt := time.Now()
		s.emitBackupAge([]*BackupRecord{recordFromManifest(m, "", verifiedAt)}, verifiedAt)
		return nil
	}
	return nil // no verified backup: no fabricated freshness claim
}

// runBackupAgeReporter observes verified-backup freshness independently of
// the optional restore/backup scheduler. Delivery is asynchronous and cannot
// block backup operations. Each provider scan has a bounded lifetime.
func (s *backupServer) runBackupAgeReporter(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	report := func() {
		scanCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := s.reportBackupAge(scanCtx); err != nil && ctx.Err() == nil {
			// Do not log provider errors verbatim: remote URLs may contain
			// credentials. No success metric is emitted on scan failure.
			log.Printf("backup age metric unavailable (%T)", err)
		}
	}
	report() // emit immediately, rather than waiting for the first tick
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report()
		}
	}
}
