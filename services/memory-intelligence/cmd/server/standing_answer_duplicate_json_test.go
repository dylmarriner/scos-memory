package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A fact selector cannot be interpreted differently by a gateway and writer.
func TestP7RefreshRejectsDuplicateFactSelector(t *testing.T) {
	for _, body := range []string{
		`{"fact_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a","fact_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a"}`,
		`{"fact_id":"invalid","fact_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a"}`,
		`{"fact_id":"invalid","FACT_ID":"4cbe32c6-891c-4155-a9ec-6aa39e66704a"}`,
	} {
		rec := httptest.NewRecorder()
		(&engineServer{}).handleStandingAnswerRefresh(rec, httptest.NewRequest(http.MethodPost, "/v1/standing-answers/refresh", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("duplicate fact_id accepted: HTTP %d for %s", rec.Code, body)
		}
	}
}
