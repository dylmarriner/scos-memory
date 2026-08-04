package main

import (
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"strings"
	"time"
)

// Side-by-side extension versions for canary testing (GOVERNANCE.md §22.3).
//
// Rollback (§22.3 "atomic enable, disable, update and rollback") is a linear
// undo of the installed manifest. A canary is different in kind: the stable
// and candidate versions are active *at the same time*, and each caller is
// resolved to one of them by activation scope. Promotion makes the candidate
// the installed version; rollback discards it. Neither ever leaves the package
// without an active version.

// CanaryScope declares which callers receive the candidate version. §22.3
// requires per-agent, per-profile, per-workspace, per-project and
// organisation-wide activation, so each is expressible here.
type CanaryScope struct {
	AgentIDs     []string `json:"agent_ids,omitempty"`
	ProfileIDs   []string `json:"profile_ids,omitempty"`
	WorkspaceIDs []string `json:"workspace_ids,omitempty"`
	ProjectIDs   []string `json:"project_ids,omitempty"`
	// Percentage rolls the candidate out to a deterministic share of callers
	// when no explicit list is given. 0 means "explicit lists only".
	Percentage int `json:"percentage,omitempty"`
	// Organisation activates the candidate for every caller. It is mutually
	// exclusive with the narrower selectors.
	Organisation bool `json:"organisation,omitempty"`
}

// empty reports whether the scope would activate the candidate for nobody.
func (s CanaryScope) empty() bool {
	return !s.Organisation && s.Percentage <= 0 &&
		len(s.AgentIDs) == 0 && len(s.ProfileIDs) == 0 &&
		len(s.WorkspaceIDs) == 0 && len(s.ProjectIDs) == 0
}

func (s CanaryScope) validate() error {
	if s.empty() {
		return errors.New("scope must activate the canary for at least one agent, profile, workspace, project, percentage or the organisation")
	}
	if s.Percentage < 0 || s.Percentage > 100 {
		return errors.New("percentage must be between 0 and 100")
	}
	if s.Organisation && (s.Percentage > 0 || len(s.AgentIDs) > 0 || len(s.ProfileIDs) > 0 ||
		len(s.WorkspaceIDs) > 0 || len(s.ProjectIDs) > 0) {
		return errors.New("organisation scope cannot be combined with narrower selectors")
	}
	return nil
}

// CanaryDeployment is a candidate version running beside the installed one.
type CanaryDeployment struct {
	PackageID      string            `json:"package_id"`
	StableVersion  string            `json:"stable_version"`
	CanaryVersion  string            `json:"canary_version"`
	CanaryManifest ExtensionManifest `json:"canary_manifest"`
	Scope          CanaryScope       `json:"scope"`
	Isolation      string            `json:"isolation"`
	Status         string            `json:"status"` // active, promoted, rolled_back
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
}

// CanaryContext identifies the caller a version must be resolved for.
type CanaryContext struct {
	AgentID     string `json:"agent_id,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
}

// VersionResolution reports which version a caller receives and why.
type VersionResolution struct {
	PackageID string `json:"package_id"`
	Version   string `json:"version"`
	Channel   string `json:"channel"` // stable or canary
	Reason    string `json:"reason"`
}

// canaryBucket maps a caller to a stable bucket in [0,100). The same caller
// always lands in the same bucket, so percentage rollouts do not flap a caller
// between versions from one request to the next.
func canaryBucket(packageID string, c CanaryContext) int {
	key := packageID + "|" + c.AgentID + "|" + c.ProfileID + "|" + c.WorkspaceID + "|" + c.ProjectID
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % 100)
}

// selects reports whether this scope activates the candidate for a caller.
func (d *CanaryDeployment) selects(c CanaryContext) (bool, string) {
	switch {
	case d.Scope.Organisation:
		return true, "organisation_wide"
	case contains(d.Scope.AgentIDs, c.AgentID):
		return true, "agent_scope"
	case contains(d.Scope.ProfileIDs, c.ProfileID):
		return true, "profile_scope"
	case contains(d.Scope.WorkspaceIDs, c.WorkspaceID):
		return true, "workspace_scope"
	case contains(d.Scope.ProjectIDs, c.ProjectID):
		return true, "project_scope"
	case d.Scope.Percentage > 0 && canaryBucket(d.PackageID, c) < d.Scope.Percentage:
		return true, "percentage_rollout"
	}
	return false, "not_in_canary_scope"
}

// resolveVersionLocked returns the version a caller receives. Callers must
// hold at least a read lock.
func (s *extServer) resolveVersionLocked(packageID string, c CanaryContext) (VersionResolution, error) {
	ext, ok := s.extensions[packageID]
	if !ok {
		return VersionResolution{}, errors.New("extension not found")
	}
	stable := VersionResolution{
		PackageID: packageID,
		Version:   ext.Manifest.Version,
		Channel:   "stable",
		Reason:    "no_active_canary",
	}
	deployment, ok := s.canaries[packageID]
	if !ok || deployment.Status != "active" {
		return stable, nil
	}
	if selected, reason := deployment.selects(c); selected {
		return VersionResolution{
			PackageID: packageID,
			Version:   deployment.CanaryVersion,
			Channel:   "canary",
			Reason:    reason,
		}, nil
	}
	stable.Reason = "not_in_canary_scope"
	return stable, nil
}

// handleCanary starts, inspects, promotes or rolls back a canary deployment.
func (s *extServer) handleCanary(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/extensions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "canary" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	packageID := parts[0]
	action := ""
	if len(parts) > 2 {
		action = parts[2]
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		s.canaryStatus(w, packageID)
	case r.Method == http.MethodPost && action == "":
		s.canaryStart(w, r, packageID)
	case r.Method == http.MethodPost && action == "resolve":
		s.canaryResolve(w, r, packageID)
	case r.Method == http.MethodPost && action == "promote":
		s.canaryFinish(w, r, packageID, true)
	case r.Method == http.MethodPost && action == "rollback":
		s.canaryFinish(w, r, packageID, false)
	default:
		http.Error(w, "unsupported canary action", http.StatusNotFound)
	}
}

func (s *extServer) canaryStatus(w http.ResponseWriter, packageID string) {
	s.mu.RLock()
	deployment, ok := s.canaries[packageID]
	var snapshot CanaryDeployment
	if ok {
		snapshot = *deployment
	}
	s.mu.RUnlock()
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{
			"package_id": packageID, "error": "no canary deployment for this package",
		})
		return
	}
	writeJSON(w, snapshot)
}

func (s *extServer) canaryStart(w http.ResponseWriter, r *http.Request, packageID string) {
	var req struct {
		Manifest ExtensionManifest `json:"manifest"`
		Scope    CanaryScope       `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// A canary candidate is an installation and faces the same fail-closed
	// manifest gate as any other (§22.2) -- it is not a privileged side door.
	if err := s.validateInstallManifest(req.Manifest); err != nil {
		http.Error(w, "canary manifest rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := req.Scope.validate(); err != nil {
		http.Error(w, "invalid canary scope: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Manifest.PackageID != packageID {
		http.Error(w, "manifest package_id does not match the target package", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	ext, ok := s.extensions[packageID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if req.Manifest.Version == ext.Manifest.Version {
		s.mu.Unlock()
		http.Error(w, "canary version must differ from the installed version", http.StatusConflict)
		return
	}
	if existing, ok := s.canaries[packageID]; ok && existing.Status == "active" {
		s.mu.Unlock()
		http.Error(w, "a canary deployment is already active for this package", http.StatusConflict)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	deployment := &CanaryDeployment{
		PackageID:      packageID,
		StableVersion:  ext.Manifest.Version,
		CanaryVersion:  req.Manifest.Version,
		CanaryManifest: req.Manifest,
		Scope:          req.Scope,
		Isolation:      s.determineIsolation(req.Manifest),
		Status:         "active",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.canaries[packageID] = deployment
	snapshot := *deployment
	s.mu.Unlock()

	if err := s.persist(r.Context()); err != nil {
		s.mu.Lock()
		delete(s.canaries, packageID)
		s.mu.Unlock()
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
			"error": "persist canary deployment: " + err.Error(),
		})
		return
	}
	writeJSONStatus(w, http.StatusCreated, snapshot)
}

func (s *extServer) canaryResolve(w http.ResponseWriter, r *http.Request, packageID string) {
	var c CanaryContext
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	resolution, err := s.resolveVersionLocked(packageID, c)
	s.mu.RUnlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, resolution)
}

// canaryFinish promotes the candidate to installed, or discards it. Either way
// the package is left with exactly one active version and no partial state.
func (s *extServer) canaryFinish(w http.ResponseWriter, r *http.Request, packageID string, promote bool) {
	s.mu.Lock()
	deployment, ok := s.canaries[packageID]
	if !ok || deployment.Status != "active" {
		s.mu.Unlock()
		http.Error(w, "no active canary deployment for this package", http.StatusNotFound)
		return
	}
	ext, ok := s.extensions[packageID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	previous := *ext
	previousHistory := append([]ExtensionManifest(nil), s.history[packageID]...)

	if promote {
		// The superseded manifest joins the undo stack so a promoted canary
		// remains rollback-able like any other update (§22.3).
		s.history[packageID] = append(s.history[packageID], ext.Manifest)
		s.removeExtensionRegistrationsLocked(packageID)
		ext.Manifest = deployment.CanaryManifest
		ext.Isolation = s.determineIsolation(ext.Manifest)
		s.registerExtensionLocked(ext.Manifest)
		ext.Status = "enabled"
		ext.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		deployment.Status = "promoted"
	} else {
		deployment.Status = "rolled_back"
	}
	deployment.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	result := map[string]any{"package_id": packageID, "canary": *deployment, "installed_version": ext.Manifest.Version}
	s.mu.Unlock()

	if err := s.persist(r.Context()); err != nil {
		// Restore the pre-change state so a failed persist cannot leave the
		// package half-promoted.
		s.mu.Lock()
		*ext = previous
		s.history[packageID] = previousHistory
		deployment.Status = "active"
		s.mu.Unlock()
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
			"error": "persist canary decision: " + err.Error(),
		})
		return
	}
	writeJSON(w, result)
}
