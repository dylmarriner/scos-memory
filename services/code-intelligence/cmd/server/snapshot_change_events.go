package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

type snapshotFileFingerprint struct {
	Hash     string
	Language string
}

type snapshotChangeEvidence struct {
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	PreviousPath  string `json:"previous_path"`
	EvidenceClass string `json:"evidence_class"`
}

// classifySnapshotChanges is deliberately conservative about provenance:
// equal bytes at two old paths do not prove which was copied or renamed.
// This compares the actual indexed manifests, not a client-provided label.
func classifySnapshotChanges(previous, current map[string]snapshotFileFingerprint) []snapshotChangeEvidence {
	oldByFingerprint := map[snapshotFileFingerprint][]string{}
	newByFingerprint := map[snapshotFileFingerprint]int{}
	paths := make([]string, 0, len(current))
	for path, fp := range previous {
		oldByFingerprint[fp] = append(oldByFingerprint[fp], path)
	}
	for path, fp := range current {
		paths = append(paths, path)
		newByFingerprint[fp]++
	}
	sort.Strings(paths)
	consumed := map[string]bool{}
	var events []snapshotChangeEvidence
	var unmatched []string
	for _, path := range paths {
		fp := current[path]
		prior, exists := previous[path]
		if !exists {
			unmatched = append(unmatched, path)
			continue
		}
		consumed[path] = true
		if prior != fp {
			events = append(events, snapshotChangeEvidence{
				Kind: "modify", Path: path, EvidenceClass: "exact_path",
			})
		}
	}
	for _, path := range unmatched {
		fp := current[path]
		origins := oldByFingerprint[fp]
		if len(origins) == 1 {
			source := origins[0]
			if surviving, ok := current[source]; ok && surviving == fp {
				events = append(events, snapshotChangeEvidence{
					Kind: "copy", Path: path, PreviousPath: source,
					EvidenceClass: "unambiguous_content",
				})
				continue
			}
			if _, present := current[source]; !present && newByFingerprint[fp] == 1 && !consumed[source] {
				consumed[source] = true
				events = append(events, snapshotChangeEvidence{
					Kind: "rename", Path: path, PreviousPath: source,
					EvidenceClass: "unambiguous_content",
				})
				continue
			}
		}
		events = append(events, snapshotChangeEvidence{
			Kind: "add", Path: path, EvidenceClass: "snapshot_manifest",
		})
	}
	for path := range previous {
		if _, stillPresent := current[path]; !stillPresent && !consumed[path] {
			events = append(events, snapshotChangeEvidence{
				Kind: "delete", Path: path, EvidenceClass: "snapshot_manifest",
			})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Path == events[j].Path {
			return events[i].Kind < events[j].Kind
		}
		return events[i].Path < events[j].Path
	})
	return events
}

// Write events inside the same SQL transaction as index activation.
// Any indexing/activation error rolls back this complete change ledger.
func recordSnapshotChangesLocked(
	ctx context.Context, tx *sql.Tx, snapshotID string,
	previous, current map[string]snapshotFileFingerprint,
) error {
	var tenantID string
	if err := tx.QueryRowContext(ctx,
		"SELECT tenant_id::text FROM code_repository_snapshots WHERE id=$1",
		snapshotID).Scan(&tenantID); err != nil {
		return fmt.Errorf("load event tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('scos.tenant_id',$1,true)", tenantID); err != nil {
		return fmt.Errorf("set event tenant: %w", err)
	}
	events := classifySnapshotChanges(previous, current)
	if len(events) == 0 {
		return nil
	}
	payload, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("marshal snapshot changes: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
        INSERT INTO code_snapshot_change_events(
            tenant_id,repository_id,snapshot_id,
            change_kind,path,previous_path,evidence_class
        )
        SELECT s.tenant_id,s.repository_id,s.id,
            e.kind,e.path,NULLIF(e.previous_path,''),e.evidence_class
        FROM code_repository_snapshots s,
            jsonb_to_recordset($2::jsonb) AS e(
                kind text,path text,previous_path text,evidence_class text
            )
        WHERE s.id=$1
    `, snapshotID, string(payload))
	if err != nil {
		return fmt.Errorf("persist snapshot changes: %w", err)
	}
	return nil
}
