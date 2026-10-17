package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Retention (GOVERNANCE.md §31): WAL and daily recoverable backups 35 days,
// weekly backups 12 weeks, monthly backups 12 months. It is applied to the
// backups the configured provider holds -- production ones included --
// after every backup, on the scheduler, and on request.
//
// Which backups are daily, weekly or monthly is derived from when each was
// taken, never from a label: every verified backup inside the daily window
// is kept; beyond it, the newest backup of each ISO week inside the weekly
// window and the newest of each calendar month inside the monthly window
// are kept; the newest verified backup is always kept. Everything else is
// expired. An archive that fails verification is never deleted
// automatically (it may be evidence) and is reported instead.
//
// Every deletion is audited: the audit record is appended and fsynced to
// the retention audit log before the backup is removed, and a backup whose
// audit record cannot be written is not removed.

type RetentionPolicy struct {
	WALAndDaily      int  `json:"wal_and_daily_days"`
	Weekly           int  `json:"weekly_weeks"`
	Monthly          int  `json:"monthly_months"`
	OKFIndefinite    bool `json:"okf_git_indefinite"`
	ConversationDays int  `json:"conversation_days"`
}

type retentionBackup struct {
	ID      string
	Taken   time.Time
	Expired bool
	Reason  string
	// Type groups backups for retention: pg_dump and physical base backups
	// each keep their own daily/weekly/monthly set. WALStart is where a
	// physical backup's replay begins.
	Type     string
	WALStart string
}

// classifyRetention marks which backups the policy keeps, and why.
func classifyRetention(policy RetentionPolicy, backups []retentionBackup, now time.Time) []retentionBackup {
	out := append([]retentionBackup(nil), backups...)
	sort.Slice(out, func(i, j int) bool { return out[i].Taken.After(out[j].Taken) })
	dailyCutoff := now.AddDate(0, 0, -policy.WALAndDaily)
	weeklyCutoff := now.AddDate(0, 0, -7*policy.Weekly)
	monthlyCutoff := now.AddDate(0, -policy.Monthly, 0)
	weeks, months := map[string]bool{}, map[string]bool{}
	for i := range out {
		b := &out[i]
		year, week := b.Taken.ISOWeek()
		weekKey := fmt.Sprintf("%d-W%02d", year, week)
		monthKey := b.Taken.Format("2006-01")
		switch {
		case i == 0:
			b.Reason = "newest verified backup"
		case !b.Taken.Before(dailyCutoff):
			b.Reason = fmt.Sprintf("within the %d-day daily window", policy.WALAndDaily)
		case !b.Taken.Before(weeklyCutoff) && !weeks[weekKey]:
			b.Reason = "newest backup of week " + weekKey
		case !b.Taken.Before(monthlyCutoff) && !months[monthKey]:
			b.Reason = "newest backup of month " + monthKey
		default:
			b.Expired = true
			b.Reason = fmt.Sprintf("taken %s: outside the %d-day daily window and not the newest of a week within %d weeks or a month within %d months",
				b.Taken.UTC().Format(time.RFC3339), policy.WALAndDaily, policy.Weekly, policy.Monthly)
		}
		if !b.Expired {
			weeks[weekKey] = true
			months[monthKey] = true
		}
	}
	return out
}

type retentionAuditRecord struct {
	At       string `json:"at"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	Provider string `json:"provider,omitempty"`
	Target   string `json:"target"`
	TakenAt  string `json:"taken_at,omitempty"`
	Reason   string `json:"reason"`
	Error    string `json:"error,omitempty"`
}

type RetentionResult struct {
	AppliedAt     string   `json:"applied_at"`
	Actor         string   `json:"actor"`
	Kept          []string `json:"kept"`
	Deleted       []string `json:"deleted"`
	Unverifiable  []string `json:"unverifiable"`
	WALDeleted    int      `json:"wal_segments_deleted"`
	WALDetail     string   `json:"wal_detail"`
	Errors        []string `json:"errors,omitempty"`
	AuditLogPath  string   `json:"audit_log"`
	PolicyApplied string   `json:"policy"`
}

func (s *backupServer) retentionAuditPath() string {
	return filepath.Join(s.backupDir, "retention-audit.jsonl")
}

// appendAudit appends one record and fsyncs it.
func (s *backupServer) appendAudit(rec retentionAuditRecord) error {
	if err := os.MkdirAll(s.backupDir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(s.retentionAuditPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// readAudit returns the newest limit audit records, newest first.
func (s *backupServer) readAudit(limit int) ([]retentionAuditRecord, error) {
	f, err := os.Open(s.retentionAuditPath())
	if os.IsNotExist(err) {
		return []retentionAuditRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []retentionAuditRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var rec retentionAuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("retention audit log is corrupt: %w", err)
		}
		all = append(all, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	out := make([]retentionAuditRecord, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out, nil
}

// applyRetention expires backups and archived WAL under the policy, as
// actor. Callers hold s.opMu.
func (s *backupServer) applyRetention(ctx context.Context, actor string, now time.Time) (RetentionResult, error) {
	res := RetentionResult{AppliedAt: now.UTC().Format(time.RFC3339), Actor: actor, Kept: []string{}, Deleted: []string{}, Unverifiable: []string{},
		AuditLogPath: s.retentionAuditPath(),
		PolicyApplied: fmt.Sprintf("daily %d days, weekly %d weeks, monthly %d months, WAL %d days",
			s.retention.WALAndDaily, s.retention.Weekly, s.retention.Monthly, s.retention.WALAndDaily)}
	if s.provider == nil {
		return res, s.providerErr
	}
	ids, err := s.provider.List(ctx)
	if err != nil {
		return res, err
	}
	var verified []retentionBackup
	for _, id := range ids {
		m, _, _, err := s.provider.Load(ctx, id)
		if err != nil {
			res.Unverifiable = append(res.Unverifiable, id)
			continue
		}
		taken, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			res.Unverifiable = append(res.Unverifiable, id)
			continue
		}
		verified = append(verified, retentionBackup{ID: id, Taken: taken, Type: m.Type, WALStart: m.WALStart})
	}
	byType := map[string][]retentionBackup{}
	for _, b := range verified {
		byType[b.Type] = append(byType[b.Type], b)
	}
	var classified []retentionBackup
	for _, group := range byType {
		classified = append(classified, classifyRetention(s.retention, group, now)...)
	}
	// WAL from the start of the oldest physical backup that is kept must
	// stay archived, or that backup can no longer be recovered.
	keepWALFrom := ""
	for _, b := range classified {
		if !b.Expired && b.Type == backupTypePhysical && b.WALStart != "" && (keepWALFrom == "" || b.WALStart < keepWALFrom) {
			keepWALFrom = b.WALStart
		}
	}
	for _, b := range classified {
		if !b.Expired {
			res.Kept = append(res.Kept, b.ID)
			continue
		}
		rec := retentionAuditRecord{At: time.Now().UTC().Format(time.RFC3339Nano), Action: "backup_deleted", Actor: actor, Provider: s.provider.Name(),
			Target: b.ID, TakenAt: b.Taken.UTC().Format(time.RFC3339), Reason: b.Reason}
		if err := s.appendAudit(rec); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s kept: audit record could not be written: %v", b.ID, err))
			res.Kept = append(res.Kept, b.ID)
			continue
		}
		if err := s.provider.Remove(ctx, b.ID); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", b.ID, err))
			_ = s.appendAudit(retentionAuditRecord{At: time.Now().UTC().Format(time.RFC3339Nano), Action: "backup_delete_failed", Actor: actor,
				Provider: s.provider.Name(), Target: b.ID, Reason: b.Reason, Error: err.Error()})
			continue
		}
		res.Deleted = append(res.Deleted, b.ID)
	}
	res.WALDeleted, res.WALDetail = s.applyWALRetention(actor, now, &res, keepWALFrom)
	sort.Strings(res.Kept)
	return res, nil
}

// walSegmentFile matches archived WAL segments (and their .partial and
// .backup companions); timeline .history files are never expired.
var walSegmentFile = regexp.MustCompile(`^[0-9A-F]{24}(\.partial|\.[0-9A-F]{8}\.backup)?$`)

// applyWALRetention removes archived WAL older than the WAL window from a
// file:// archive destination this service can reach. Segments at or after
// keepFrom (the start of the oldest kept physical base backup) are kept
// whatever their age: point-in-time recovery replays them.
func (s *backupServer) applyWALRetention(actor string, now time.Time, res *RetentionResult, keepFrom string) (int, string) {
	dir, err := walArchiveDir(s.config.walArchiveDestination)
	if err != nil {
		return 0, "WAL retention not applied: " + err.Error()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, "WAL retention not applied: " + err.Error()
	}
	cutoff := now.AddDate(0, 0, -s.retention.WALAndDaily)
	deleted := 0
	for _, e := range entries {
		if e.IsDir() || !walSegmentFile.MatchString(e.Name()) {
			continue
		}
		if keepFrom != "" && e.Name()[:24] >= keepFrom {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		reason := fmt.Sprintf("archived %s, outside the %d-day WAL window", info.ModTime().UTC().Format(time.RFC3339), s.retention.WALAndDaily)
		if err := s.appendAudit(retentionAuditRecord{At: time.Now().UTC().Format(time.RFC3339Nano), Action: "wal_segment_deleted", Actor: actor,
			Target: filepath.Join(dir, e.Name()), TakenAt: info.ModTime().UTC().Format(time.RFC3339), Reason: reason}); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("WAL %s kept: audit record could not be written: %v", e.Name(), err))
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("WAL %s: %v", e.Name(), err))
			continue
		}
		deleted++
	}
	return deleted, fmt.Sprintf("%d archived WAL segment(s) older than %d days removed from %s", deleted, s.retention.WALAndDaily, dir)
}

// walArchiveDir resolves a file:// (or absolute) WAL archive destination.
func walArchiveDir(dest string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", fmt.Errorf("SCOS_WAL_ARCHIVE_DESTINATION is not configured")
	}
	if strings.HasPrefix(dest, "file://") {
		dest = strings.TrimPrefix(dest, "file://")
	}
	if !filepath.IsAbs(dest) {
		return "", fmt.Errorf("WAL archive destination %q is not a path this service can read", dest)
	}
	return dest, nil
}
