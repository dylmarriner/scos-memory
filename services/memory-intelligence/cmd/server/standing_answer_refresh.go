package main

import (
	"database/sql"
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
            JOIN principals p ON p.id=m.principal_id AND p.tenant_id=f.tenant_id
                             AND p.disabled_at IS NULL
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
                SELECT 1 FROM memory_items f
                JOIN principals p ON p.id=$3::uuid AND p.tenant_id=f.tenant_id
                                 AND p.disabled_at IS NULL
                WHERE f.id=$1::uuid AND f.tenant_id=$2::uuid
            )`, input.FactID, identity.TenantID, identity.PrincipalID).Scan(&readable)
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
		err = tx.QueryRowContext(r.Context(), `SELECT id::text FROM (
                SELECT public.scos_refresh_verified_standing_answer(
                    $1::uuid, $2
                ) AS id
            ) verified WHERE id IS NOT NULL`, input.FactID, out.QuestionKey).Scan(&out.ID)
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
