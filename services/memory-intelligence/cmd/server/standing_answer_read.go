package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// standingAnswerRead is a previously generated answer, not a newly generated
// claim. Its evidence is rechecked by the security-invoker PostgreSQL view
// on every read; no cached answer is returned when evidence is stale.
type standingAnswerRead struct {
	ID              string    `json:"id"`
	NamespaceID     string    `json:"namespace_id"`
	QuestionKey     string    `json:"question_key"`
	Answer          string    `json:"answer"`
	EvidenceFactIDs []string  `json:"evidence_fact_ids"`
	Confidence      float64   `json:"confidence"`
	LastRefreshedAt time.Time `json:"last_refreshed_at"`
}

// handleStandingAnswerRead serves only an existing, evidence-current answer.
// It does not create or refresh answers and never queries the unfiltered
// standing-answer table. Authentication is enforced by the server's auth
// middleware; tenant/principal claims are re-resolved for transaction-local RLS.
func (s *engineServer) handleStandingAnswerRead(w http.ResponseWriter, r *http.Request) {
	// HTTP intermediaries must not outlive evidence expiry or access revocation.
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// URL.Query silently discards malformed percent-encoded pairs. Reject
	// the entire request rather than interpreting only its valid subset.
	q, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		http.Error(w, "malformed standing-answer query", http.StatusBadRequest)
		return
	}
	for key, values := range q {
		if (key != "namespace_id" && key != "question_key") || len(values) != 1 {
			http.Error(w, "unsupported or repeated standing-answer selector", http.StatusBadRequest)
			return
		}
	}
	namespaceID := q.Get("namespace_id")
	question := strings.ToLower(strings.TrimSpace(q.Get("question_key")))
	if !isUUIDLike(namespaceID) || question == "" || len(question) > 1024 || !utf8.ValidString(question) || strings.ContainsRune(question, '\x00') {
		http.Error(w, "namespace_id must be a UUID and question_key must be nonempty (max 1024 bytes)", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if identity.TenantID == "" || identity.PrincipalID == "" {
		http.Error(w, "authenticated tenant and principal required", http.StatusUnauthorized)
		return
	}

	result, err := withPrincipalContext(r.Context(), s.db, identity, func(tx querier) (standingAnswerRead, error) {
		var answer standingAnswerRead
		var evidence string
		err := tx.QueryRowContext(r.Context(), `
            SELECT id::text,namespace_id::text,question_key,answer,
                   evidence_fact_ids,confidence,last_refreshed_at
            FROM public.memory_standing_answers_current
            WHERE tenant_id=$1::uuid AND namespace_id=$2::uuid AND question_key=$3
            LIMIT 1`, identity.TenantID, namespaceID, question).
			Scan(&answer.ID, &answer.NamespaceID, &answer.QuestionKey, &answer.Answer,
				&evidence, &answer.Confidence, &answer.LastRefreshedAt)
		if err != nil {
			return answer, err
		}
		if err := json.Unmarshal([]byte(evidence), &answer.EvidenceFactIDs); err != nil {
			return answer, err
		}
		if len(answer.EvidenceFactIDs) == 0 {
			return answer, errors.New("missing standing-answer evidence")
		}
		return answer, nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "standing answer not found", http.StatusNotFound)
		return
	}
	if err != nil {
		// Driver errors can contain credentials, hosts, or SQL. Never echo or
		// log their contents on this externally reachable read path.
		log.Printf("standing-answer read failed: %T", err)
		http.Error(w, "internal standing-answer read failure", http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}
