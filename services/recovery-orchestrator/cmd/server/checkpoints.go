package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResumePoint is where recovery of one interrupted turn resumes: the latest
// checkpoint of the turn's session, or none when that session has never
// checkpointed (the turn then restarts from its accepted messages).
type ResumePoint struct {
	TurnID       string `json:"turn_id"`
	SessionID    string `json:"session_id"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

// checkpointFacts is one session's latest checkpoint as
// scos_recovery_checkpoints() (migration 000057) reports it.
type checkpointFacts struct {
	CheckpointID           string  `json:"checkpoint_id"`
	SessionID              string  `json:"session_id"`
	TurnID                 *string `json:"turn_id"`
	LastEventSequence      int64   `json:"last_event_sequence"`
	CreatedAt              string  `json:"created_at"`
	TurnStatus             *string `json:"turn_status"`
	EventsPresent          int64   `json:"events_present"`
	CompletionEventMatches bool    `json:"completion_event_matches"`
}

type checkpointReport struct {
	Checkpoints  []checkpointFacts `json:"checkpoints"`
	ResumePoints []ResumePoint     `json:"resume_points"`
}

// checkpointProblem says why a checkpoint is not a sound resume point, or
// "" when it is: its turn must exist, the turn's events 1..N up to the
// checkpoint's sequence must all be present, and the turn's turn.completed
// event (written in the same transaction as the checkpoint) must name it.
func checkpointProblem(c checkpointFacts) string {
	switch {
	case c.TurnID == nil || c.TurnStatus == nil:
		return "its turn no longer exists"
	case c.LastEventSequence <= 0:
		return fmt.Sprintf("it records no event sequence (%d)", c.LastEventSequence)
	case c.EventsPresent != c.LastEventSequence:
		return fmt.Sprintf("event history is incomplete: %d of %d events present", c.EventsPresent, c.LastEventSequence)
	case !c.CompletionEventMatches:
		return "the turn's turn.completed event does not reference it"
	}
	return ""
}

// loadCheckpoints verifies every session's latest checkpoint against its
// turn's event history and maps interrupted turns to their resume points.
// It reports ok only when every latest checkpoint is sound.
func loadCheckpoints(raw []byte) (ok bool, detail string, resume []ResumePoint, err error) {
	var report checkpointReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return false, "", nil, fmt.Errorf("decode checkpoints: %w", err)
	}
	resume = report.ResumePoints
	if resume == nil {
		resume = []ResumePoint{}
	}
	var problems []string
	latest := ""
	for _, c := range report.Checkpoints {
		if p := checkpointProblem(c); p != "" {
			problems = append(problems, c.CheckpointID+" (session "+c.SessionID+"): "+p)
		}
		if c.CreatedAt > latest {
			latest = c.CreatedAt
		}
	}
	withCheckpoint := 0
	for _, r := range resume {
		if r.CheckpointID != "" {
			withCheckpoint++
		}
	}
	resumeDetail := ""
	if len(resume) > 0 {
		resumeDetail = fmt.Sprintf("; %s resume from their session's checkpoint, %d without one restart from their accepted messages",
			countNoun(withCheckpoint, "interrupted turn"), len(resume)-withCheckpoint)
	}
	if len(problems) > 0 {
		shown := problems
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(problems)-5))
		}
		return false, fmt.Sprintf("%d of %d latest session checkpoints are not sound resume points: %s",
			len(problems), len(report.Checkpoints), strings.Join(shown, "; ")), resume, nil
	}
	if len(report.Checkpoints) == 0 {
		return true, "no checkpoints recorded; no session has a resume point" + resumeDetail, resume, nil
	}
	return true, fmt.Sprintf("%s loaded and verified against their turns' event history, latest at %s%s",
		countNoun(len(report.Checkpoints), "latest session checkpoint"), latest, resumeDetail), resume, nil
}
