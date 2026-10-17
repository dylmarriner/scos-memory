package release

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	want := Info{Service: "agent-registry", SourceSHA: "abc123", BuildTime: "2026-09-01T00:00:00Z", StartedAt: startedAt.Format(time.RFC3339)}
	got.UptimeSeconds = 0 // measured, not fixed
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	startedAt = startedAt.Add(-90 * time.Second)
	defer func() { startedAt = startedAt.Add(90 * time.Second) }()
	if up := Current("agent-registry").UptimeSeconds; up < 90 {
		t.Fatalf("uptime = %d, want at least 90", up)
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
