package readiness

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHandlerReportsCheckFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(func() error { return os.ErrNotExist })(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"not_ready"`) {
		t.Fatalf("unexpected readiness response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWritableDir(t *testing.T) {
	if err := WritableDir(t.TempDir()); err != nil {
		t.Fatalf("expected writable directory: %v", err)
	}
}

func TestWritableDirRejectsFile(t *testing.T) {
	file := t.TempDir() + "/file"
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := WritableDir(file); err == nil {
		t.Fatal("expected file path to be rejected")
	}
}
