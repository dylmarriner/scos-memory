package readiness

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
)

// Handler turns a local readiness check into the service health contract.
// Checks must not include secret values in their errors.
func Handler(check func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if check != nil {
			if err := check(); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_ready", "reason": err.Error()})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// WritableDir verifies that a service can persist a small temporary file.
func WritableDir(path string) error {
	if path == "" {
		return errors.New("storage directory is not configured")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return errors.New("storage directory is unavailable")
	}
	tmp, err := os.CreateTemp(path, ".readiness-*")
	if err != nil {
		return errors.New("storage directory is not writable")
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return errors.New("storage directory is not writable")
	}
	if err := os.Remove(name); err != nil {
		return errors.New("storage directory cleanup failed")
	}
	return nil
}
