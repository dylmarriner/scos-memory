//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPools(t *testing.T) (admin, app *pgxpool.Pool) {
	t.Helper()
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	app, err = pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return admin, app
}

// TestCheckpointVerificationAgainstEventHistory seeds two sessions inside a
// transaction that is rolled back (events are append-only, so nothing is
// left behind): one whose latest checkpoint has its full event history and
// a matching turn.completed event, and one whose history has a gap. The
// migration-000057 function, as the owner sees it, must report both
// faithfully, and step 4 must pass the first and fail the second; an
// interrupted turn in the first session resumes from its checkpoint. The
// runtime role must be able to execute the function.
func TestCheckpointVerificationAgainstEventHistory(t *testing.T) {
	admin, app := integrationPools(t)
	ctx := context.Background()
	if _, err := app.Exec(ctx, `SELECT scos_recovery_checkpoints()`); err != nil {
		t.Fatalf("runtime role cannot load checkpoints: %v", err)
	}

	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var tenant, principal, namespace string
	if err := tx.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('checkpoint-it') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1,'human','cp-it','CP IT') RETURNING id::text`, tenant).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO namespaces (tenant_id, canonical_name) VALUES ($1,'cp-it') RETURNING id::text`, tenant).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	seed := func(eventSeqs []int64) (session, turn, checkpoint string) {
		t.Helper()
		if err := tx.QueryRow(ctx, `INSERT INTO sessions (tenant_id, namespace_id, created_by) VALUES ($1,$2,$3) RETURNING id::text`, tenant, namespace, principal).Scan(&session); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO turns (session_id, actor_id, sequence, status) VALUES ($1,$2,1,'completed') RETURNING id::text`, session, principal).Scan(&turn); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&checkpoint); err != nil {
			t.Fatal(err)
		}
		for i, seq := range eventSeqs {
			eventType, data := "message.appended", `{}`
			if i == len(eventSeqs)-1 {
				eventType, data = "turn.completed", `{"checkpoint_id":"`+checkpoint+`"}`
			}
			exec(`INSERT INTO events (id, tenant_id, aggregate_id, aggregate_sequence, source, event_type, subject, occurred_at, data_schema, data)
				VALUES (gen_random_uuid(),$1,$2::uuid,$3,'checkpoint-it',$4,$6,now(),'it',$5::jsonb)`, tenant, turn, seq, eventType, data, "turn/"+turn)
		}
		exec(`INSERT INTO checkpoints (id, tenant_id, session_id, turn_id, last_event_sequence, state, content_hash) VALUES ($1,$2,$3,$4,3,'{}','h')`, checkpoint, tenant, session, turn)
		return session, turn, checkpoint
	}
	goodSession, _, goodCheckpoint := seed([]int64{1, 2, 3})
	gapSession, _, gapCheckpoint := seed([]int64{1, 3})
	var interrupted string
	if err := tx.QueryRow(ctx, `INSERT INTO turns (session_id, actor_id, sequence, status) VALUES ($1,$2,2,'interrupted') RETURNING id::text`, goodSession, principal).Scan(&interrupted); err != nil {
		t.Fatal(err)
	}

	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT scos_recovery_checkpoints()`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var report checkpointReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	mine := checkpointReport{}
	for _, c := range report.Checkpoints {
		if c.SessionID == goodSession || c.SessionID == gapSession {
			mine.Checkpoints = append(mine.Checkpoints, c)
		}
	}
	for _, r := range report.ResumePoints {
		if r.TurnID == interrupted {
			mine.ResumePoints = append(mine.ResumePoints, r)
		}
	}
	if len(mine.Checkpoints) != 2 || len(mine.ResumePoints) != 1 || mine.ResumePoints[0].CheckpointID != goodCheckpoint {
		t.Fatalf("function report for the seeded sessions = %+v", mine)
	}
	scoped, _ := json.Marshal(mine)
	ok, detail, _, err := loadCheckpoints(scoped)
	if err != nil || ok || !strings.Contains(detail, gapCheckpoint) || strings.Contains(detail, goodCheckpoint+" ") {
		t.Fatalf("step 4 must fail only the checkpoint with a history gap: ok=%v %q %v", ok, detail, err)
	}
	for _, c := range mine.Checkpoints {
		if c.CheckpointID == goodCheckpoint && checkpointProblem(c) != "" {
			t.Fatalf("sound checkpoint judged unsound: %s", checkpointProblem(c))
		}
		if c.CheckpointID == gapCheckpoint && !strings.Contains(checkpointProblem(c), "2 of 3 events present") {
			t.Fatalf("gap not reported: %q", checkpointProblem(c))
		}
	}
}

// TestCompletionReplaysTheRunsEventLog completes a run through the runtime
// role and checks the replay marked its events and advanced the cursor;
// then completes a second run whose row was altered behind the log's back,
// which must be refused with nothing marked replayed.
func TestCompletionReplaysTheRunsEventLog(t *testing.T) {
	admin, app := integrationPools(t)
	ctx := context.Background()
	const good, tampered = "replay-it-good", "replay-it-tampered"
	cleanup := func() {
		_, _ = admin.Exec(ctx, `DELETE FROM recovery_runs WHERE service_id = ANY($1)`, []string{good, tampered})
	}
	cleanup()
	t.Cleanup(cleanup)

	readyChecklist := func() RecoveryChecklist {
		c := RecoveryChecklist{}
		for step := 1; step <= 10; step++ {
			status := "ok"
			if step == 10 {
				status = "pending"
			}
			c = append(c, ChecklistItem{Step: step, Name: "step", Status: status})
		}
		return c
	}
	s := &recoveryServer{statuses: map[string]*RecoveryStatus{}, db: app}
	start := func(service string) *RecoveryStatus {
		t.Helper()
		st := &RecoveryStatus{ServiceID: service, Phase: "recovery", Checklist: readyChecklist(), InterruptedTurns: []string{}, OpenOperations: []string{}, ResumePoints: []ResumePoint{}}
		if err := s.persistRecoveryStatus(ctx, st, "recovery_started"); err != nil {
			t.Fatal(err)
		}
		s.statuses[service] = st
		return st
	}
	complete := func(service string) *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		s.handleRecoveryComplete(res, httptest.NewRequest(http.MethodPost, "/v1/recovery/complete", bytes.NewBufferString(`{"service_id":"`+service+`"}`)))
		return res
	}

	run := start(good)
	if res := complete(good); res.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", res.Code, res.Body.String())
	}
	var cursor int64
	var pending int
	if err := admin.QueryRow(ctx, `SELECT replay_cursor, (SELECT count(*) FROM recovery_events WHERE recovery_run_id=r.id AND replay_state <> 'replayed') FROM recovery_runs r WHERE id=$1`, run.RecoveryRunID).Scan(&cursor, &pending); err != nil {
		t.Fatal(err)
	}
	if cursor != 2 || pending != 0 {
		t.Fatalf("replay cursor=%d pending=%d, want 2 and 0", cursor, pending)
	}

	run = start(tampered)
	if _, err := admin.Exec(ctx, `UPDATE recovery_runs SET checklist='[]' WHERE id=$1`, run.RecoveryRunID); err != nil {
		t.Fatal(err)
	}
	res := complete(tampered)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "replay") {
		t.Fatalf("completion over a diverged row: %d %s", res.Code, res.Body.String())
	}
	var phase string
	if err := admin.QueryRow(ctx, `SELECT phase, replay_cursor, (SELECT count(*) FROM recovery_events WHERE recovery_run_id=r.id AND replay_state='replayed') FROM recovery_runs r WHERE id=$1`, run.RecoveryRunID).Scan(&phase, &cursor, &pending); err != nil {
		t.Fatal(err)
	}
	if phase == "completed" || cursor != 0 || pending != 0 {
		t.Fatalf("refused completion left phase=%s cursor=%d replayed=%d", phase, cursor, pending)
	}
	if s.statuses[tampered].Phase == "completed" {
		t.Fatal("the served status reports a completion the database refused")
	}
}
