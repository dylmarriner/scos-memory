package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func strp(v string) *string { return &v }

func checkpointJSON(t *testing.T, report checkpointReport) []byte {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLoadCheckpointsVerifiesEachAgainstItsEventHistory(t *testing.T) {
	sound := checkpointFacts{CheckpointID: "cp-1", SessionID: "s-1", TurnID: strp("t-1"), TurnStatus: strp("completed"),
		LastEventSequence: 4, EventsPresent: 4, CompletionEventMatches: true, CreatedAt: "2026-10-15T01:00:00Z"}
	ok, detail, resume, err := loadCheckpoints(checkpointJSON(t, checkpointReport{
		Checkpoints:  []checkpointFacts{sound},
		ResumePoints: []ResumePoint{{TurnID: "t-9", SessionID: "s-1", CheckpointID: "cp-1"}, {TurnID: "t-8", SessionID: "s-7"}},
	}))
	if err != nil || !ok {
		t.Fatalf("a sound checkpoint must load: %v %q", err, detail)
	}
	if !strings.Contains(detail, "1 latest session checkpoint loaded and verified") || !strings.Contains(detail, "1 interrupted turn resume from") || !strings.Contains(detail, "1 without one") {
		t.Fatalf("detail = %q", detail)
	}
	if len(resume) != 2 || resume[0].CheckpointID != "cp-1" || resume[1].CheckpointID != "" {
		t.Fatalf("resume points = %+v", resume)
	}

	for name, broken := range map[string]checkpointFacts{
		"turn gone":         {CheckpointID: "cp-2", SessionID: "s-2", LastEventSequence: 3, EventsPresent: 3, CompletionEventMatches: true},
		"history gap":       {CheckpointID: "cp-3", SessionID: "s-3", TurnID: strp("t-3"), TurnStatus: strp("completed"), LastEventSequence: 5, EventsPresent: 4, CompletionEventMatches: true},
		"no completion":     {CheckpointID: "cp-4", SessionID: "s-4", TurnID: strp("t-4"), TurnStatus: strp("completed"), LastEventSequence: 2, EventsPresent: 2},
		"no event sequence": {CheckpointID: "cp-5", SessionID: "s-5", TurnID: strp("t-5"), TurnStatus: strp("completed"), CompletionEventMatches: true},
	} {
		ok, detail, _, err := loadCheckpoints(checkpointJSON(t, checkpointReport{Checkpoints: []checkpointFacts{sound, broken}}))
		if err != nil || ok {
			t.Fatalf("%s: an unsound checkpoint must fail the step: ok=%v err=%v", name, ok, err)
		}
		if !strings.Contains(detail, "1 of 2 latest session checkpoints are not sound") || !strings.Contains(detail, broken.CheckpointID) {
			t.Fatalf("%s: detail = %q", name, detail)
		}
	}

	ok, detail, resume, err = loadCheckpoints([]byte(`{"checkpoints":[],"resume_points":[]}`))
	if err != nil || !ok || !strings.Contains(detail, "no checkpoints recorded") || resume == nil {
		t.Fatalf("no checkpoints: ok=%v %q %v %v", ok, detail, resume, err)
	}
	if _, _, _, err := loadCheckpoints([]byte(`not json`)); err == nil {
		t.Fatal("an undecodable report must be an error")
	}
}

func event(t *testing.T, seq int64, typ, phase string, steps ...int) recoveryEvent {
	t.Helper()
	checklist := RecoveryChecklist{}
	for _, s := range steps {
		checklist = append(checklist, ChecklistItem{Step: s, Status: "ok"})
	}
	payload, err := json.Marshal(map[string]any{"phase": phase, "checklist": checklist})
	if err != nil {
		t.Fatal(err)
	}
	return recoveryEvent{Sequence: seq, EventType: typ, Payload: payload}
}

func TestFoldRecoveryEventsReplaysAWellFormedLog(t *testing.T) {
	run, err := foldRecoveryEvents([]recoveryEvent{
		event(t, 1, "recovery_started", "recovery", 1, 2),
		event(t, 2, "checklist_updated", "recovery", 1, 2),
		event(t, 3, "recovery_completed", "completed", 1, 2),
	})
	if err != nil || run.Phase != "completed" || run.LastSequence != 3 || len(run.Checklist) != 2 {
		t.Fatalf("replay = %+v, %v", run, err)
	}
}

func TestFoldRecoveryEventsRefusesAMalformedLog(t *testing.T) {
	cases := map[string][]recoveryEvent{
		"empty":            nil,
		"gap":              {event(t, 1, "recovery_started", "recovery", 1), event(t, 3, "checklist_updated", "recovery", 1)},
		"not started":      {event(t, 1, "checklist_updated", "recovery", 1)},
		"restarted":        {event(t, 1, "recovery_started", "recovery", 1), event(t, 2, "recovery_started", "recovery", 1)},
		"after completion": {event(t, 1, "recovery_started", "recovery", 1), event(t, 2, "recovery_completed", "completed", 1), event(t, 3, "checklist_updated", "completed", 1)},
		"unknown type":     {event(t, 1, "recovery_started", "recovery", 1), event(t, 2, "flag_flipped", "recovery", 1)},
		"step dropped":     {event(t, 1, "recovery_started", "recovery", 1, 2), event(t, 2, "checklist_updated", "recovery", 1)},
		"bad payload":      {event(t, 1, "recovery_started", "recovery", 1), {Sequence: 2, EventType: "checklist_updated", Payload: []byte(`{`)}},
	}
	for name, events := range cases {
		if _, err := foldRecoveryEvents(events); err == nil {
			t.Fatalf("%s: a malformed log must not replay", name)
		}
	}
}
