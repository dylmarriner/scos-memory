package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dylmarriner/scos-memory/packages/strictjson"
	"github.com/google/uuid"
)

// This is a read-only, deterministic evidence draft, NOT a governed standing
// answer. No model, unverified prose, or cache mutation is involved. A human
// review must still precede publication of any composed answer.
type standingAnswerComposeRequest struct {
	NamespaceID string   `json:"namespace_id"`
	FactIDs     []string `json:"fact_ids"`
}

type standingAnswerComposeEvidence struct {
	FactID            string  `json:"fact_id"`
	Summary           string  `json:"summary"`
	Confidence        float64 `json:"confidence"`
	VersionID         string  `json:"version_id,omitempty"`
	SourceFingerprint string  `json:"source_fingerprint"`
}

// Versioned, domain-separated digest of the source fields and its tenant/
// namespace security boundary. Null current_version_id is encoded as empty. The
// source summary cannot contain NUL; UUIDs and decimal floats cannot either.
// This is a drift detector, not a signature or a substitute for approval.
func standingAnswerComposeFingerprint(ev standingAnswerComposeEvidence, tenantID, namespaceID string) string {
	data := "scos:p7:source:v2\x00" + tenantID + "\x00" + namespaceID + "\x00" +
		ev.FactID + "\x00" + ev.VersionID + "\x00" +
		ev.Summary + "\x00" + strconv.FormatFloat(ev.Confidence, 'g', -1, 64)
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

type standingAnswerComposeResult struct {
	Status      string                          `json:"status"`
	NamespaceID string                          `json:"namespace_id"`
	DraftText   string                          `json:"draft_text"`
	Evidence    []standingAnswerComposeEvidence `json:"evidence"`
	Confidence  float64                         `json:"confidence"`
}

var errStandingAnswerComposeUnavailable = errors.New("one or more evidence facts are unavailable")
var errStandingAnswerComposeUnsafe = errors.New("evidence cannot be composed without truncation")

func (s *engineServer) handleStandingAnswerCompose(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input standingAnswerComposeRequest
	if err := strictjson.DecodeObject(http.MaxBytesReader(w, r.Body, 4096), &input); err != nil {
		http.Error(w, "invalid evidence composition request", http.StatusBadRequest)
		return
	}
	namespace, err := uuid.Parse(input.NamespaceID)
	if err != nil || namespace == uuid.Nil || len(input.FactIDs) < 2 || len(input.FactIDs) > 8 {
		http.Error(w, "namespace_id and 2 to 8 unique fact_ids are required", http.StatusBadRequest)
		return
	}
	ids := make([]string, 0, len(input.FactIDs))
	seen := make(map[string]bool, len(input.FactIDs))
	for _, raw := range input.FactIDs {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil || seen[id.String()] {
			http.Error(w, "fact_ids must be distinct nonzero UUIDs", http.StatusBadRequest)
			return
		}
		seen[id.String()] = true
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	input.NamespaceID = namespace.String()
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
		http.Error(w, "evidence composition unavailable", http.StatusServiceUnavailable)
		return
	}
	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		http.Error(w, "invalid evidence composition request", http.StatusBadRequest)
		return
	}
	result, err := withPrincipalContext(r.Context(), s.db, identity, func(tx querier) (standingAnswerComposeResult, error) {
		out := standingAnswerComposeResult{
			Status:      "draft_requires_review",
			NamespaceID: input.NamespaceID,
			Evidence:    make([]standingAnswerComposeEvidence, 0, len(ids)),
			Confidence:  1,
		}
		// One statement snapshot, under the caller's transaction-local RLS.
		// Every source must be active, current, visible, in the same namespace
		// and free of unresolved P7 revalidation. Missing any source rejects
		// the ENTIRE draft; there is no partial evidence fallback.
		rows, err := tx.QueryContext(r.Context(), `
            SELECT f.id::text, f.canonical_summary, f.confidence_score,
                   COALESCE(f.current_version_id::text, '')
            FROM public.memory_items f
            JOIN public.namespaces ns
              ON ns.id=f.namespace_id AND ns.tenant_id=f.tenant_id
            JOIN public.namespace_memberships m
              ON m.namespace_id=f.namespace_id
             AND m.principal_id=$4::uuid AND m.can_read
            JOIN public.principals p
              ON p.id=m.principal_id AND p.tenant_id=f.tenant_id
             AND p.disabled_at IS NULL
            WHERE f.tenant_id=$1::uuid AND f.namespace_id=$2::uuid
              AND f.id IN (
                  SELECT value::uuid FROM jsonb_array_elements_text($3::jsonb) AS v(value)
              )
              AND f.status='active' AND f.superseded_by_id IS NULL
              AND f.sensitivity<=ns.sensitivity_ceiling
              AND (f.expires_at IS NULL OR f.expires_at>statement_timestamp())
              AND (f.valid_from IS NULL OR f.valid_from<=statement_timestamp())
              AND (f.valid_until IS NULL OR f.valid_until>statement_timestamp())
              AND NOT EXISTS (
                  SELECT 1 FROM public.memory_revalidation_events ev
                  LEFT JOIN public.memory_revalidation_decisions d
                    ON d.event_id=ev.id AND d.tenant_id=ev.tenant_id
                  WHERE ev.fact_id=f.id AND ev.tenant_id=f.tenant_id
                    AND ev.review_status IN ('needs_review','superseded','invalidated','unresolved')
                    AND (d.outcome IS NULL OR d.outcome<>'still_valid')
              )
            ORDER BY f.id::text
        `, identity.TenantID, input.NamespaceID, string(encodedIDs), identity.PrincipalID)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		summaries := make([]string, 0, len(ids))
		for rows.Next() {
			var ev standingAnswerComposeEvidence
			if err := rows.Scan(&ev.FactID, &ev.Summary, &ev.Confidence, &ev.VersionID); err != nil {
				return out, err
			}
			if strings.TrimSpace(ev.Summary) == "" || !utf8.ValidString(ev.Summary) ||
				strings.ContainsRune(ev.Summary, '\x00') || len(ev.Summary) > 8192 ||
				math.IsNaN(ev.Confidence) || math.IsInf(ev.Confidence, 0) ||
				ev.Confidence < 0 || ev.Confidence > 1 {
				return out, errStandingAnswerComposeUnsafe
			}
			ev.SourceFingerprint = standingAnswerComposeFingerprint(ev, identity.TenantID, input.NamespaceID)
			out.Evidence = append(out.Evidence, ev)
			// Escape line breaks and control bytes in untrusted source prose.
			// Otherwise a source could forge a second "Source fact" header
			// or terminal formatting in the human review draft. The exact
			// original summary remains available in structured Evidence.
			summaries = append(summaries, "Source fact "+ev.FactID+":\n"+strconv.Quote(ev.Summary))
			if ev.Confidence < out.Confidence {
				out.Confidence = ev.Confidence
			}
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		if len(out.Evidence) != len(ids) {
			return out, errStandingAnswerComposeUnavailable
		}
		out.DraftText = strings.Join(summaries, "\n\n")
		if len(out.DraftText) > 16384 {
			return out, errStandingAnswerComposeUnsafe
		}
		return out, nil
	})
	switch {
	case errors.Is(err, errStandingAnswerComposeUnavailable), errors.Is(err, sql.ErrNoRows):
		http.Error(w, "evidence not found", http.StatusNotFound)
	case errors.Is(err, errStandingAnswerComposeUnsafe):
		http.Error(w, "evidence cannot be safely composed", http.StatusConflict)
	case err != nil:
		log.Printf("evidence composition failed (%T)", err)
		http.Error(w, "evidence composition failed", http.StatusInternalServerError)
	default:
		writeJSON(w, result)
	}
}
