package release

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerServesCurrentInfo(t *testing.T) {
	oldSHA, oldTime := SourceSHA, BuildTime
	SourceSHA, BuildTime = "abc123", "2026-09-01T00:00:00Z"
	defer func() { SourceSHA, BuildTime = oldSHA, oldTime }()

	rec := httptest.NewRecorder()
	Handler("agent-registry")(rec, httptest.NewRequest(http.MethodGet, "/health/version", nil))

	var got Info
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := Info{Service: "agent-registry", SourceSHA: "abc123", BuildTime: "2026-09-01T00:00:00Z"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestUnstampedBuildReportsUnknown(t *testing.T) {
	if SourceSHA != "unknown" || BuildTime != "unknown" {
		t.Skip("package-level vars already stamped by another test in this run")
	}
	info := Current("agent-registry")
	if info.SourceSHA != "unknown" || info.BuildTime != "unknown" {
		t.Fatalf("a `go test` build (no -ldflags -X) must report unknown, got %+v", info)
	}
}
