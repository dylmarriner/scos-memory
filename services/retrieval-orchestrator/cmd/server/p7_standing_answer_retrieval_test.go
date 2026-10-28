package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestP7StandingAnswerOptInRejectsIncompatibleTemporalAndClassSelectors(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"historical valid time", map[string]any{"as_of_valid_time": "2021-07-01T00:00:00Z"}},
		{"historical code", map[string]any{"as_of_commit": "abc", "repository_id": "repo", "branch": "main"}},
		{"memory classes", map[string]any{"memory_classes": []string{"semantic"}}},
		{"oversized question", map[string]any{"query": string(bytes.Repeat([]byte("x"), 1025))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"tenant_id": "tenant", "principal_id": "principal", "query": "question",
				"include_standing_answers": true,
			}
			for k, v := range tc.fields {
				payload[k] = v
			}
			raw, _ := json.Marshal(payload)
			rec := httptest.NewRecorder()
			(&server{}).handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(raw)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got HTTP %d, want 400 before DB access: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestP7StandingAnswerQuestionNormalization(t *testing.T) {
	if got := standingAnswerQuestionKey("  WhErE Is ThE Report? \t"); got != "where is the report?" {
		t.Fatalf("question key %q", got)
	}
	if got := standingAnswerQuestionKey(string([]byte{0xff})); got != "" {
		t.Fatalf("invalid UTF-8 accepted: %q", got)
	}
	if got := standingAnswerQuestionKey(""); got != "" {
		t.Fatalf("empty key %q", got)
	}
	if got := standingAnswerQuestionKey(string(bytes.Repeat([]byte("x"), 1025))); got != "" {
		t.Fatalf("oversized key accepted")
	}
}

func TestP7StandingAnswerBudgetDoesNotTruncateEvidence(t *testing.T) {
	a := StandingAnswerEvidence{ID: "a", QuestionKey: "what?", Answer: "A complete governed answer", EvidenceFactIDs: []string{"fact"}}
	selected, used := selectBudgetedStandingAnswers([]StandingAnswerEvidence{a}, 1)
	if len(selected) != 0 || used != 0 {
		t.Fatalf("partial answer served: %+v / %d", selected, used)
	}
	selected, used = selectBudgetedStandingAnswers([]StandingAnswerEvidence{a}, 1000)
	if len(selected) != 1 || selected[0].Answer != a.Answer || used <= 0 {
		t.Fatalf("complete answer not served: %+v / %d", selected, used)
	}
}
