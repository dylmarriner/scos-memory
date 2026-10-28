package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/dylmarriner/scos-memory/packages/strictjson"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// standingAnswerRefreshRequest intentionally accepts only an existing fact
// identity. Caller-provided question/answer text cannot be promoted into a
// trusted standing answer by this endpoint.
type standingAnswerRefreshRequest struct {
	FactID string `json:"fact_id"`
}

type standingAnswerRefreshResult struct {
	ID           string  `json:"id"`
	NamespaceID  string  `json:"namespace_id"`
	QuestionKey  string  `json:"question_key"`
	Answer       string  `json:"answer"`
	SourceFactID string  `json:"source_fact_id"`
	Confidence   float64 `json:"confidence"`
}

var errStandingAnswerWriteDenied = errors.New("standing answer refresh not authorized")
var errStandingAnswerEvidenceStale = errors.New("standing answer evidence is not current")
var errStandingAnswerConflict = errors.New("standing answer key is already governed by another source")

// handleStandingAnswerRefresh materializes only a deterministic, exact-title
// lookup from an active memory's already-governed canonical summary. This is
// deliberately NOT an LLM answer generator or an arbitrary question writer.
// The same transaction holds the source row's SHARE lock until the cache
// dependency trigger validates and commits the supporting version.
func (s *engineServer) handleStandingAnswerRefresh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input standingAnswerRefreshRequest
	// A duplicate fact_id must not be interpreted differently by a gateway
	// and the authenticated writer. Reject all ambiguous JSON objects.
	if err := strictjson.DecodeObject(http.MaxBytesReader(w, r.Body, 4096), &input); err != nil {
		http.Error(w, "invalid refresh request", http.StatusBadRequest)
		return
	}
	factUUID, err := uuid.Parse(input.FactID)
	if err != nil || factUUID == uuid.Nil {
		http.Error(w, "fact_id must be a nonzero UUID", http.StatusBadRequest)
		return
	}
	input.FactID = factUUID.String()
	identity, status, message := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, message, status)
		return
	}
	if identity.TenantID == "" || identity.PrincipalID == "" {
		http.Error(w, "authenticated tenant and principal required", http.StatusUnauthorized)
		return
	}
	if s.db == nil {
		http.Error(w, "standing answer refresh unavailable", http.StatusServiceUnavailable)
		return
	}

	result, err := withPrincipalContext(r.Context(), s.db, identity, func(tx querier) (standingAnswerRefreshResult, error) {
		var out standingAnswerRefreshResult
		// This is a write operation. Reading a namespace is not enough to
		// authorize materializing a new answer in it.
		var canWrite bool
		err := tx.QueryRowContext(r.Context(), `SELECT EXISTS (
            SELECT 1 FROM memory_items f
            JOIN namespace_memberships m ON m.namespace_id=f.namespace_id
            WHERE f.id=$1::uuid AND f.tenant_id=$2::uuid
              AND m.principal_id=$3::uuid AND m.can_read AND m.can_write_candidates
        )`, input.FactID, identity.TenantID, identity.PrincipalID).Scan(&canWrite)
		if err != nil {
			return out, err
		}
		if !canWrite {
			// A nonexistent fact must remain indistinguishable from a fact
			// the principal is not allowed to read.
			var readable bool
			err = tx.QueryRowContext(r.Context(), `SELECT EXISTS (
                SELECT 1 FROM memory_items f WHERE f.id=$1::uuid AND f.tenant_id=$2::uuid
            )`, input.FactID, identity.TenantID).Scan(&readable)
			if err != nil {
				return out, err
			}
			if readable {
				return out, errStandingAnswerWriteDenied
			}
			return out, sql.ErrNoRows
		}
		var title, summary string
		err = tx.QueryRowContext(r.Context(), `SELECT f.namespace_id::text, f.title, f.canonical_summary, f.confidence_score
            FROM memory_items f
            JOIN namespaces n ON n.id=f.namespace_id AND n.tenant_id=f.tenant_id
            WHERE f.id=$1::uuid AND f.tenant_id=$2::uuid
              AND f.status='active' AND f.superseded_by_id IS NULL
              AND f.sensitivity<=n.sensitivity_ceiling
              AND (f.expires_at IS NULL OR f.expires_at>statement_timestamp())
              AND (f.valid_from IS NULL OR f.valid_from<=statement_timestamp())
              AND (f.valid_until IS NULL OR f.valid_until>statement_timestamp())
            FOR SHARE OF f`, input.FactID, identity.TenantID).
			Scan(&out.NamespaceID, &title, &summary, &out.Confidence)
		if err != nil {
			return out, err
		}
		out.QuestionKey = strings.ToLower(strings.TrimSpace(title))
		if out.QuestionKey == "" || len(out.QuestionKey) > 1024 || !utf8.ValidString(out.QuestionKey) ||
			strings.ContainsRune(out.QuestionKey, '\x00') || strings.TrimSpace(summary) == "" ||
			len(summary) > 16384 || !utf8.ValidString(summary) || strings.ContainsRune(summary, '\x00') {
			return out, errStandingAnswerEvidenceStale
		}
		out.Answer = summary
		out.SourceFactID = input.FactID
		evidence, err := json.Marshal([]string{input.FactID})
		if err != nil {
			return out, err
		}
		err = tx.QueryRowContext(r.Context(), `INSERT INTO memory_standing_answers (
                tenant_id,namespace_id,question_key,answer,evidence_fact_ids,
                confidence,last_refreshed_at,refresh_reason,stale_at,stale_reason)
            VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,statement_timestamp(),
                    'verified_fact_title_summary',NULL,NULL)
            ON CONFLICT (tenant_id,namespace_id,question_key) DO UPDATE SET
                answer=EXCLUDED.answer,evidence_fact_ids=EXCLUDED.evidence_fact_ids,
                confidence=EXCLUDED.confidence,last_refreshed_at=EXCLUDED.last_refreshed_at,
                refresh_reason=EXCLUDED.refresh_reason,stale_at=NULL,stale_reason=NULL
            WHERE memory_standing_answers.refresh_reason='verified_fact_title_summary'
              AND (memory_standing_answers.evidence_fact_ids=EXCLUDED.evidence_fact_ids
                   OR memory_standing_answers.stale_at IS NOT NULL
                   -- A clock-only expiry cannot fire a row-change invalidation
                   -- trigger. Allow replacing only a verified single-source
                   -- generated cache whose ORIGINAL source has objectively
                   -- expired, not a merely unreadable/restricted cache.
                   OR EXISTS (
                       SELECT 1 FROM memory_items old_source
                       WHERE old_source.tenant_id=memory_standing_answers.tenant_id
                         AND old_source.namespace_id=memory_standing_answers.namespace_id
                         AND memory_standing_answers.evidence_fact_ids=
                             ('["' || old_source.id::text || '"]')
                         AND (old_source.expires_at<=statement_timestamp()
                              OR old_source.valid_until<=statement_timestamp())
                   ))
            RETURNING id::text`, identity.TenantID, out.NamespaceID, out.QuestionKey,
			out.Answer, string(evidence), out.Confidence).Scan(&out.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return out, errStandingAnswerConflict
		}
		if err != nil {
			// The dependency trigger rejects facts with unresolved code
			// revalidation. Map only its exact, expected integrity denial;
			// unrelated constraint failures must remain server errors.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" &&
				pgErr.Message == "standing answer references inaccessible or stale evidence" {
				return out, errStandingAnswerEvidenceStale
			}
			return out, err
		}
		// Never report success for an answer that the current-evidence view
		// would refuse to serve under this same caller's RLS context.
		var verifiedID string
		err = tx.QueryRowContext(r.Context(), `SELECT id::text FROM memory_standing_answers_current
            WHERE id=$1::uuid AND tenant_id=$2::uuid`, out.ID, identity.TenantID).Scan(&verifiedID)
		if errors.Is(err, sql.ErrNoRows) {
			return out, errStandingAnswerEvidenceStale
		}
		return out, err
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "source fact not found", http.StatusNotFound)
	case errors.Is(err, errStandingAnswerWriteDenied):
		http.Error(w, "standing answer refresh not authorized", http.StatusForbidden)
	case errors.Is(err, errStandingAnswerConflict):
		http.Error(w, "standing answer key is already owned by another source", http.StatusConflict)
	case errors.Is(err, errStandingAnswerEvidenceStale):
		http.Error(w, "source evidence is not eligible for refresh", http.StatusConflict)
	case err != nil:
		// Do not expose SQL, connection credentials, policy details, or
		// evidence from another tenant in public HTTP responses or logs.
		log.Printf("standing answer refresh failed (%T)", err)
		http.Error(w, "standing answer refresh failed", http.StatusInternalServerError)
	default:
		writeJSON(w, result)
	}
}
