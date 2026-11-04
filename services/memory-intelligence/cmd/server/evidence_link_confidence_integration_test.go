//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func p6EvidenceLinkConfidenceFixture(t *testing.T) (*behaviourFixture, string, string) {
	t.Helper()
	f := newBehaviourFixture(t)
	fact := f.remember("P6 evidence-link confidence must remain a bounded probability.", 0.9)
	repository := f.scalar(
		"INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text",
		f.tenant, "/p6/confidence/"+fact,
	)
	snapshot := f.scalar(
		"INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','p6-confidence','active') RETURNING id::text",
		repository, f.tenant,
	)
	symbol := f.scalar(
		"INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text",
		repository, "p6-confidence-"+fact, snapshot,
	)
	version := f.scalar(
		"INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'P6.Confidence','p6/confidence.go','file','p6-confidence-hash') RETURNING id::text",
		symbol, snapshot,
	)
	return f, fact, version
}

func postP6EvidenceLink(t *testing.T, f *behaviourFixture, fact, version, linkType string, confidence *float64) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"fact_id":                fact,
		"code_symbol_version_id": version,
	}
	if linkType != "" {
		body["link_type"] = linkType
	}
	if confidence != nil {
		body["confidence"] = *confidence
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, response, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", string(encoded))
	return code, response
}

func TestP6EvidenceLinkConfidenceDefaultsAndPreservesZero(t *testing.T) {
	f, fact, version := p6EvidenceLinkConfidenceFixture(t)

	code, first := postP6EvidenceLink(t, f, fact, version, "evidences", nil)
	if code != http.StatusOK {
		t.Fatalf("omitted confidence = HTTP %d: %v", code, first)
	}
	linkID, ok := first["id"].(string)
	if !ok || linkID == "" {
		t.Fatalf("missing evidence-link id: %v", first)
	}

	var confidence float64
	var count int
	if err := f.admin.QueryRow(
		"SELECT confidence FROM memory_code_links WHERE id=$1",
		linkID,
	).Scan(&confidence); err != nil {
		t.Fatal(err)
	}
	if confidence != 1 {
		t.Fatalf("omitted confidence = %v, want default 1", confidence)
	}

	zero := 0.0
	code, updated := postP6EvidenceLink(t, f, fact, version, "evidences", &zero)
	if code != http.StatusOK || updated["id"] != linkID {
		t.Fatalf("explicit zero update = HTTP %d: %v", code, updated)
	}
	if err := f.admin.QueryRow(
		"SELECT confidence, count(*) OVER () FROM memory_code_links WHERE id=$1",
		linkID,
	).Scan(&confidence, &count); err != nil {
		t.Fatal(err)
	}
	if confidence != 0 || count != 1 {
		t.Fatalf("explicit zero was not preserved without duplication: confidence=%v rows=%d", confidence, count)
	}

	quarter := 0.25
	code, fractional := postP6EvidenceLink(t, f, fact, version, "motivated_by", &quarter)
	if code != http.StatusOK {
		t.Fatalf("fractional confidence = HTTP %d: %v", code, fractional)
	}
	if err := f.admin.QueryRow(
		"SELECT confidence FROM memory_code_links WHERE id=$1",
		fractional["id"],
	).Scan(&confidence); err != nil {
		t.Fatal(err)
	}
	if confidence != quarter {
		t.Fatalf("fractional confidence = %v, want %v", confidence, quarter)
	}
}

func TestP6EvidenceLinkConfidenceRejectsOutOfRangeHTTP(t *testing.T) {
	f, fact, version := p6EvidenceLinkConfidenceFixture(t)
	for _, value := range []float64{-0.01, 1.01} {
		code, response := postP6EvidenceLink(t, f, fact, version, "evidences", &value)
		if code != http.StatusBadRequest {
			t.Fatalf("confidence %v = HTTP %d: %v, want 400", value, code, response)
		}
	}
	var count int
	if err := f.admin.QueryRow(
		"SELECT count(*) FROM memory_code_links WHERE fact_id=$1",
		fact,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("out-of-range requests created %d evidence links", count)
	}
}

func TestP6EvidenceLinkConfidenceRejectsNonNumberHTTP(t *testing.T) {
	f, fact, version := p6EvidenceLinkConfidenceFixture(t)
	for _, raw := range []string{"null", "\"0.5\"", "true", "[]"} {
		body := fmt.Sprintf(`{"fact_id":%q,"code_symbol_version_id":%q,"confidence":%s}`, fact, version, raw)
		code, response, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", body)
		if code != http.StatusBadRequest {
			t.Fatalf("confidence %s = HTTP %d: %v, want 400", raw, code, response)
		}
	}
	var count int
	if err := f.admin.QueryRow(
		"SELECT count(*) FROM memory_code_links WHERE fact_id=$1",
		fact,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid confidence types created %d evidence links", count)
	}
}

func TestP6EvidenceLinkConfidenceConstraintRejectsInvalidSQLValues(t *testing.T) {
	f, fact, version := p6EvidenceLinkConfidenceFixture(t)
	for _, value := range []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		-0.01,
		1.01,
	} {
		_, err := f.admin.Exec(
			"INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source,confidence) VALUES($1,$2,$3,'evidences','explicit',$4)",
			f.tenant, fact, version, value,
		)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("confidence %v must fail with check violation 23514, got %v", value, err)
		}
	}
}
