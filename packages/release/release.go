// Package release answers "which commit is this specific running process
// actually built from?" -- a question the fabric had no runtime way to
// answer before this package existed. Production has drifted from the
// repository at least twice in this project's history (once to a plain file
// copy with no .git at all, once with six services left running a stale
// image for 42+ hours after a rebuild) and both were discovered forensically,
// after the fact, from commit history and manual inspection rather than from
// any signal the running fabric itself exposed.
package release

import (
	"encoding/json"
	"net/http"
)

// SourceSHA and BuildTime are stamped at image-build time via `go build
// -ldflags -X`; see deploy/Dockerfile and deploy/native/build.sh. Both stay
// "unknown" for a `go run`/`go test`/local invocation, which never passes
// that flag -- that is expected, not a misconfiguration, and callers should
// not treat "unknown" itself as an error.
var (
	SourceSHA = "unknown"
	BuildTime = "unknown"
)

// Info is what every service reports at GET /health/version.
type Info struct {
	Service   string `json:"service"`
	SourceSHA string `json:"source_sha"`
	BuildTime string `json:"build_time"`
}

// Current returns this process's release info for the named service.
func Current(service string) Info {
	return Info{Service: service, SourceSHA: SourceSHA, BuildTime: BuildTime}
}

// Handler serves Current(service) as JSON. Deploy tooling polls this on
// every service after a rollout and compares SourceSHA against the commit
// it just deployed, so a container left on a stale image is caught
// immediately instead of being discovered later by accident.
func Handler(service string) http.HandlerFunc {
	info := Current(service)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	}
}
