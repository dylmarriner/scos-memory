package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseEvidenceLinkConfidenceUsesExactDecimalBounds(t *testing.T) {
	for _, raw := range []string{
		"0",
		"-0",
		"0.25",
		"0.0000000000000001",
		"0.9999999999999999",
		"1",
		"1.000e0",
	} {
		if _, err := parseEvidenceLinkConfidence(json.RawMessage(raw)); err != nil {
			t.Errorf("confidence %s rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"1.0000000000000001",
		"-0.0000000000000001",
		"1.01",
		"-0.01",
		"null",
		"\"0.5\"",
		"true",
		"[]",
		"1e309",
	} {
		if _, err := parseEvidenceLinkConfidence(json.RawMessage(raw)); err == nil {
			t.Errorf("confidence %s accepted, want rejection", raw)
		}
	}
}

func TestEvidenceLinksRejectsDecimalAboveOneBeforeDatabaseAccess(t *testing.T) {
	srv := &engineServer{}
	body := `{"fact_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a","code_symbol_version_id":"5cbe32c6-891c-4155-a9ec-6aa39e66704a","confidence":1.0000000000000001}`
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-links", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	srv.handleEvidenceLinks(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("precision-loss confidence returned HTTP %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}
