// Package workspace maps the paths agents name -- paths on the operator's
// own machine -- to the shared workspace mount services see, and refuses
// anything outside it.
//
// Agents run on the host and name repositories by host path. Services run
// in containers where the host's workspace directory (SCOS_WORKSPACE_HOST_PATH)
// is mounted at SCOS_WORKSPACE_ROOT (default /workspace). Resolve translates
// one to the other and is the containment boundary for any service that
// reads, indexes or runs commands in a caller-named directory.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is the directory the workspace is mounted at inside this process.
func Root() string {
	if v := strings.TrimSpace(os.Getenv("SCOS_WORKSPACE_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	return "/workspace"
}

// Resolve maps an absolute path an agent supplied (a host path under
// SCOS_WORKSPACE_HOST_PATH, or a path already under Root) to the real
// directory it names, refusing anything that is not an existing directory
// inside the workspace once symlinks are resolved.
func Resolve(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be absolute", path)
	}
	root := Root()
	if host := strings.TrimSpace(os.Getenv("SCOS_WORKSPACE_HOST_PATH")); host != "" {
		host = filepath.Clean(host)
		if rel, err := filepath.Rel(host, path); err == nil && within(rel) {
			path = filepath.Join(root, rel)
		}
	}
	if rel, err := filepath.Rel(root, path); err != nil || !within(rel) {
		return "", fmt.Errorf("%q is outside the mounted workspace; place it under SCOS_WORKSPACE_HOST_PATH (%s)", path, os.Getenv("SCOS_WORKSPACE_HOST_PATH"))
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%q is not accessible: %w", path, err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace %s is not mounted: %w", root, err)
	}
	if rel, err := filepath.Rel(realRoot, resolved); err != nil || !within(rel) {
		return "", fmt.Errorf("%q resolves outside the workspace", path)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return resolved, nil
}

func within(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
