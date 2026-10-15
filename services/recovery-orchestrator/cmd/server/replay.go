package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// Event replay. Every change to a recovery run is appended to
// recovery_events with the run's full state at that point, and the run row
// holds the current state. Before a run is completed, its event log is
// replayed: the events are applied in sequence order, checking that the log
// is gap-free, starts with recovery_started, contains only known event
// types, never drops a checklist step, and has nothing after a completion.
// The state the log replays to must equal the run row, or the row and its
// audit trail disagree and the completion is refused. Only events that
// replayed are marked replayed, and the run's replay_cursor records the
// last sequence applied.

type recoveryEvent struct {
	Sequence  int64
	EventType string
	Payload   []byte
}

type replayedRun struct {
	Phase        string
	Checklist    RecoveryChecklist
	LastSequence int64
}

var replayableEventTypes = map[string]bool{
	"recovery_started":   true,
	"checklist_updated":  true,
	"recovery_completed": true,
}

// foldRecoveryEvents replays events (ordered by sequence) into the run state
// they describe.
func foldRecoveryEvents(events []recoveryEvent) (replayedRun, error) {
	var run replayedRun
	if len(events) == 0 {
		return run, fmt.Errorf("the run has no events to replay")
	}
	var steps []int
	for i, ev := range events {
		if ev.Sequence != int64(i+1) {
			return run, fmt.Errorf("event log has a gap: expected sequence %d, found %d", i+1, ev.Sequence)
		}
		if !replayableEventTypes[ev.EventType] {
			return run, fmt.Errorf("event %d has unknown type %q", ev.Sequence, ev.EventType)
		}
		if i == 0 && ev.EventType != "recovery_started" {
			return run, fmt.Errorf("event log starts with %q, not recovery_started", ev.EventType)
		}
		if i > 0 && ev.EventType == "recovery_started" {
			return run, fmt.Errorf("event %d restarts a run that already started", ev.Sequence)
		}
		if run.Phase == "completed" {
			return run, fmt.Errorf("event %d follows the run's completion", ev.Sequence)
		}
		var state struct {
			Phase     string            `json:"phase"`
			Checklist RecoveryChecklist `json:"checklist"`
		}
		if err := json.Unmarshal(ev.Payload, &state); err != nil {
			return run, fmt.Errorf("event %d payload: %w", ev.Sequence, err)
		}
		if state.Phase == "" {
			return run, fmt.Errorf("event %d records no phase", ev.Sequence)
		}
		eventSteps := make([]int, 0, len(state.Checklist))
		for _, item := range state.Checklist {
			eventSteps = append(eventSteps, item.Step)
		}
		if steps != nil && !reflect.DeepEqual(steps, eventSteps) {
			return run, fmt.Errorf("event %d changes the checklist's steps from %v to %v", ev.Sequence, steps, eventSteps)
		}
		steps = eventSteps
		run.Phase, run.Checklist, run.LastSequence = state.Phase, state.Checklist, ev.Sequence
	}
	return run, nil
}

// replayRunEvents replays the run's event log inside tx and verifies it
// reproduces the run row as it currently stands.
func replayRunEvents(ctx context.Context, tx pgx.Tx, runID string) (replayedRun, error) {
	rows, err := tx.Query(ctx, `SELECT sequence, event_type, payload FROM recovery_events WHERE recovery_run_id=$1 ORDER BY sequence`, runID)
	if err != nil {
		return replayedRun{}, err
	}
	var events []recoveryEvent
	for rows.Next() {
		var ev recoveryEvent
		if err := rows.Scan(&ev.Sequence, &ev.EventType, &ev.Payload); err != nil {
			rows.Close()
			return replayedRun{}, err
		}
		events = append(events, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return replayedRun{}, err
	}
	run, err := foldRecoveryEvents(events)
	if err != nil {
		return run, err
	}
	var phase string
	var rawChecklist []byte
	if err := tx.QueryRow(ctx, `SELECT phase, checklist FROM recovery_runs WHERE id=$1`, runID).Scan(&phase, &rawChecklist); err != nil {
		return run, err
	}
	var checklist RecoveryChecklist
	if err := json.Unmarshal(rawChecklist, &checklist); err != nil {
		return run, fmt.Errorf("run checklist: %w", err)
	}
	if phase != run.Phase || !reflect.DeepEqual(normaliseChecklist(checklist), normaliseChecklist(run.Checklist)) {
		return run, fmt.Errorf("the run row (phase %q) does not match the state its %d events replay to (phase %q)", phase, run.LastSequence, run.Phase)
	}
	return run, nil
}

func normaliseChecklist(c RecoveryChecklist) RecoveryChecklist {
	if c == nil {
		return RecoveryChecklist{}
	}
	return c
}
