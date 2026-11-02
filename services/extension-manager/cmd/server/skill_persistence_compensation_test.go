package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Failed direct-skill writes must not leave a visible skill or consume an ID.
// The persisted local mirror must remain consistent with the live registry.
func TestDirectSkillCreateFailureCompensatesLiveAndLocal(t *testing.T) {
	srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
	initial := httptest.NewRecorder()
	srv.handleSkills(initial, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"existing"}`)))
	if initial.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", initial.Code, initial.Body.String())
	}
	beforeID := srv.nextID
	srv.persistBeforeWrite = func(context.Context) error { return errors.New("sensitive /private/skill-state.json") }
	res := httptest.NewRecorder()
	srv.handleSkills(res, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"uncommitted","capabilities":["memory.write"]}`)))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503: %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "/private/") {
		t.Fatalf("leaked storage path: %s", res.Body.String())
	}
	if srv.nextID != beforeID || srv.skills["skill-2"] != nil {
		t.Fatalf("failed create retained skill or consumed ID: id=%d skills=%+v", srv.nextID, srv.skills)
	}
	restored := newExtServerAt(srv.storePath)
	if restored.nextID != beforeID || restored.skills["skill-2"] != nil {
		t.Fatalf("failed create changed durable state: %+v", restored.skills)
	}
	srv.persistBeforeWrite = nil
	retry := httptest.NewRecorder()
	srv.handleSkills(retry, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"committed"}`)))
	if retry.Code != http.StatusOK || srv.skills["skill-2"] == nil {
		t.Fatalf("retry failed: %d %s", retry.Code, retry.Body.String())
	}
}

func TestDirectSkillDeleteFailureCompensatesLiveAndLocal(t *testing.T) {
	srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
	create := httptest.NewRecorder()
	srv.handleSkills(create, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"keep","capabilities":["memory.read"]}`)))
	if create.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", create.Code, create.Body.String())
	}
	before := *srv.skills["skill-1"]
	srv.persistBeforeWrite = func(context.Context) error { return errors.New("sensitive /private/delete-state.json") }
	res := httptest.NewRecorder()
	srv.handleSkillByID(res, httptest.NewRequest(http.MethodDelete, "/v1/skills/skill-1", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503: %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "/private/") {
		t.Fatalf("leaked storage path: %s", res.Body.String())
	}
	if got := srv.skills["skill-1"]; got == nil || !reflect.DeepEqual(*got, before) {
		t.Fatalf("failed delete removed live skill: %+v", got)
	}
	restored := newExtServerAt(srv.storePath)
	if got := restored.skills["skill-1"]; got == nil || !reflect.DeepEqual(*got, before) {
		t.Fatalf("failed delete changed durable state: %+v", got)
	}
	srv.persistBeforeWrite = nil
	retry := httptest.NewRecorder()
	srv.handleSkillByID(retry, httptest.NewRequest(http.MethodDelete, "/v1/skills/skill-1", nil))
	if retry.Code != http.StatusOK || srv.skills["skill-1"] != nil {
		t.Fatalf("retry delete failed: %d %s", retry.Code, retry.Body.String())
	}
	var reply map[string]string
	if err := json.Unmarshal(retry.Body.Bytes(), &reply); err != nil || reply["status"] != "deleted" {
		t.Fatalf("unexpected delete response: %s", retry.Body.String())
	}
}

// A real PostgreSQL CHECK rejection happens after the local mirror was
// written. Both the in-memory registry and mirror must be compensated.
func TestDirectSkillRejectedDBWriteRepairsLocalMirror(t *testing.T) {
	for _, tc := range []struct {
		name, constraint, method, path, body string
	}{
		{"create", `CHECK (NOT (state->'skills' ? 'skill-2'))`, http.MethodPost, "/v1/skills", `{"name":"blocked"}`},
		{"delete", `CHECK (state->'skills' ? 'skill-1')`, http.MethodDelete, "/v1/skills/skill-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := extensionOperatorTestDB(t)
			srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
			srv.db = pool
			seed := httptest.NewRecorder()
			srv.handleSkills(seed, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"existing"}`)))
			if seed.Code != http.StatusOK {
				t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
			}
			if _, err := pool.Exec(context.Background(), "ALTER TABLE extension_manager_state ADD CONSTRAINT reject_skill_mutation "+tc.constraint); err != nil {
				t.Fatal(err)
			}
			res := httptest.NewRecorder()
			if tc.method == http.MethodPost {
				srv.handleSkills(res, httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body)))
			} else {
				srv.handleSkillByID(res, httptest.NewRequest(tc.method, tc.path, nil))
			}
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503: %d %s", res.Code, res.Body.String())
			}
			if srv.nextID != 1 || srv.skills["skill-1"] == nil || srv.skills["skill-2"] != nil {
				t.Fatalf("live state not compensated: id=%d skills=%+v", srv.nextID, srv.skills)
			}
			restored := newExtServerAt(srv.storePath)
			if restored.nextID != 1 || restored.skills["skill-1"] == nil || restored.skills["skill-2"] != nil {
				t.Fatalf("local mirror not repaired: id=%d skills=%+v", restored.nextID, restored.skills)
			}
			var raw []byte
			if err := pool.QueryRow(context.Background(), "SELECT state FROM extension_manager_state WHERE id=true").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var state persistedExtensionState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if state.NextID != 1 || state.Skills["skill-1"] == nil || state.Skills["skill-2"] != nil {
				t.Fatalf("DB state diverged: %+v", state.Skills)
			}
		})
	}
}

// A lost acknowledgement after PostgreSQL commit must be reconciled, not
// blindly compensated. Both direct-skill mutation paths use persist().
func TestDirectSkillLostDBAcknowledgementReconciles(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			pool, _ := extensionOperatorTestDB(t)
			srv := newExtServerAt("")
			srv.db = pool
			seed := httptest.NewRecorder()
			srv.handleSkills(seed, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"existing"}`)))
			if seed.Code != http.StatusOK {
				t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
			}
			srv.persistAfterDBWrite = func(context.Context) error { return errors.New("lost acknowledgement after commit") }
			res := httptest.NewRecorder()
			if method == http.MethodPost {
				srv.handleSkills(res, httptest.NewRequest(method, "/v1/skills", bytes.NewBufferString(`{"name":"committed"}`)))
			} else {
				srv.handleSkillByID(res, httptest.NewRequest(method, "/v1/skills/skill-1", nil))
			}
			if res.Code != http.StatusOK {
				t.Fatalf("committed change wrongly rejected: %d %s", res.Code, res.Body.String())
			}
			var raw []byte
			if err := pool.QueryRow(context.Background(), "SELECT state FROM extension_manager_state WHERE id=true").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var state persistedExtensionState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if method == http.MethodPost {
				if srv.skills["skill-2"] == nil || state.Skills["skill-2"] == nil {
					t.Fatal("committed create lost")
				}
			} else if srv.skills["skill-1"] != nil || state.Skills["skill-1"] != nil {
				t.Fatal("committed delete was rolled back")
			}
		})
	}
}

// Hook completion can allocate an execution ID while a skill write is pending.
// Compensating the skill must not rewind that newer ID and cause collisions.
func TestDirectSkillCreateFailurePreservesConcurrentExecutionID(t *testing.T) {
	srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
	srv.persistBeforeWrite = func(context.Context) error {
		srv.recordExecutions([]*HookExecution{{Event: "on_checkpoint", TenantID: "tenant-a", Status: "success"}})
		return errors.New("injected skill persistence failure")
	}
	res := httptest.NewRecorder()
	srv.handleSkills(res, httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewBufferString(`{"name":"not-committed"}`)))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503: %d", res.Code)
	}
	if srv.skills["skill-1"] != nil {
		t.Fatal("failed skill was not removed")
	}
	if len(srv.executions) != 1 || srv.executions[0].ID != "hookexec-2" || srv.nextID != 2 {
		t.Fatalf("concurrent execution ID was rewound: nextID=%d executions=%+v", srv.nextID, srv.executions)
	}
	restored := newExtServerAt(srv.storePath)
	if restored.nextID != 2 || len(restored.executions) != 1 || restored.executions[0].ID != "hookexec-2" {
		t.Fatalf("concurrent execution was lost on restart: nextID=%d executions=%+v", restored.nextID, restored.executions)
	}
}
