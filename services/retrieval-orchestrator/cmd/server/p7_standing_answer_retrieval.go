package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// StandingAnswerEvidence is a previously materialized exact-title answer,
// never an independently generated response. Its evidence is revalidated by
// the security-invoker PostgreSQL view at the instant of retrieval.
type StandingAnswerEvidence struct {
	ID              string    `json:"id"`
	NamespaceID     string    `json:"namespace_id"`
	QuestionKey     string    `json:"question_key"`
	Answer          string    `json:"answer"`
	EvidenceFactIDs []string  `json:"evidence_fact_ids"`
	Confidence      float64   `json:"confidence"`
	LastRefreshedAt time.Time `json:"last_refreshed_at"`
}

func standingAnswerQuestionKey(query string) string {
	if !utf8.ValidString(query) {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(query))
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.ContainsRune(key, '\x00') {
		return ""
	}
	return key
}

// readStandingAnswers is opt-in and strictly current-time. It cannot substitute
// cached text for a historical answer or bypass a requested sensitivity ceiling.
// The RLS view validates ALL evidence, while the additional anti-join applies
// the request's narrower sensitivity ceiling to EVERY cited fact.
func (s *server) readStandingAnswers(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]StandingAnswerEvidence, error) {
	key := standingAnswerQuestionKey(req.Query)
	if key == "" || len(namespaces) == 0 || limit <= 0 {
		return nil, nil
	}
	var answers []StandingAnswerEvidence
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, `
            SELECT a.id::text, a.namespace_id::text, a.question_key,
                   a.answer, a.evidence_fact_ids, a.confidence, a.last_refreshed_at
            FROM public.memory_standing_answers_current a
            JOIN public.namespaces n ON n.id=a.namespace_id AND n.tenant_id=a.tenant_id
            WHERE a.tenant_id=$1::uuid
              AND n.canonical_name=ANY($2::text[])
              AND a.question_key=$3
              AND EXISTS (
                  SELECT 1 FROM public.principals p
                  WHERE p.id=$6::uuid AND p.tenant_id=a.tenant_id
                    AND p.disabled_at IS NULL
              )
              AND NOT EXISTS (
                  SELECT 1
                  FROM jsonb_array_elements_text(a.evidence_fact_ids::jsonb) AS cited(fact_id)
                  LEFT JOIN public.memory_items m
                    ON m.id=cited.fact_id::uuid
                   AND m.tenant_id=a.tenant_id
                   AND m.namespace_id=a.namespace_id
                  WHERE m.id IS NULL OR m.sensitivity > $4
              )
            ORDER BY a.namespace_id, a.id
            LIMIT $5
        `, req.TenantID, namespaces, key, sensitivityMax, limit, req.PrincipalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a StandingAnswerEvidence
			var evidence string
			if err := rows.Scan(&a.ID, &a.NamespaceID, &a.QuestionKey, &a.Answer,
				&evidence, &a.Confidence, &a.LastRefreshedAt); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(evidence), &a.EvidenceFactIDs); err != nil {
				return fmt.Errorf("decode standing answer evidence: %w", err)
			}
			if len(a.EvidenceFactIDs) == 0 {
				return errors.New("standing answer missing evidence")
			}
			answers = append(answers, a)
		}
		return rows.Err()
	})
	return answers, err
}

// selectBudgetedStandingAnswers never truncates an answer into a potentially
// misleading claim. Cache output shares the same token ceiling as memory text.
func selectBudgetedStandingAnswers(candidates []StandingAnswerEvidence, remaining int) ([]StandingAnswerEvidence, int) {
	var selected []StandingAnswerEvidence
	used := 0
	for _, a := range candidates {
		cost := estimateTokens(a.Answer) + estimateTokens(a.QuestionKey) + len(a.EvidenceFactIDs)*16
		if cost <= 0 || cost > remaining-used {
			continue
		}
		selected = append(selected, a)
		used += cost
	}
	return selected, used
}

func standingAnswerAuditTokens(answers []StandingAnswerEvidence) int {
	used := 0
	for _, a := range answers {
		used += estimateTokens(a.Answer) + estimateTokens(a.QuestionKey) + len(a.EvidenceFactIDs)*16
	}
	return used
}
