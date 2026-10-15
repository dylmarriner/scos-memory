package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Server-side repository scanning.
//
// An agent indexes a repository by naming it, not by uploading it: the
// repository lives in the workspace mounted into this service, and the
// service reads, hashes and parses the files itself, then builds a durable
// snapshot through the same pipeline as /v2/code/index (Go AST and
// Tree-sitter extraction, incremental diff, code embeddings when a code
// model runtime is configured).
//
// SCOS_WORKSPACE_HOST_PATH is the host directory that holds repositories;
// it is mounted at SCOS_WORKSPACE_ROOT (default /workspace). Agents pass
// repository paths as they see them on the host, so a path under the host
// directory is translated to the mount; a path already under the mount is
// used as is. Anything else is refused: this service never reads outside
// the workspace.

const (
	scanMaxFileBytes = 512 << 10
	scanMaxFiles     = 20000
)

// scanSkipDirs are generated, vendored or tool-state directories whose
// contents are never source to index.
var scanSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true, ".venv": true, "venv": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true, ".next": true,
	"graphify-out": true, ".idea": true, ".vscode": true, "coverage": true,
}

func workspaceRoot() string {
	if v := strings.TrimSpace(os.Getenv("SCOS_WORKSPACE_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	return "/workspace"
}

// resolveWorkspacePath maps a repository path an agent supplied to the
// directory this service can read, refusing anything outside the workspace.
func resolveWorkspacePath(canonical string) (string, error) {
	canonical = filepath.Clean(strings.TrimSpace(canonical))
	if !filepath.IsAbs(canonical) {
		return "", fmt.Errorf("repository path %q must be absolute", canonical)
	}
	root := workspaceRoot()
	if host := strings.TrimSpace(os.Getenv("SCOS_WORKSPACE_HOST_PATH")); host != "" {
		host = filepath.Clean(host)
		if rel, err := filepath.Rel(host, canonical); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			canonical = filepath.Join(root, rel)
		}
	}
	rel, err := filepath.Rel(root, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repository %q is outside the mounted workspace; place it under SCOS_WORKSPACE_HOST_PATH (%s)", canonical, os.Getenv("SCOS_WORKSPACE_HOST_PATH"))
	}
	resolved, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		return "", fmt.Errorf("repository %q is not readable: %w", canonical, err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace %s is not mounted: %w", root, err)
	}
	if rel, err := filepath.Rel(realRoot, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repository %q resolves outside the workspace", canonical)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", fmt.Errorf("repository %q is not a directory", canonical)
	}
	return resolved, nil
}

// scanRepository reads every indexable source file under dir.
func scanRepository(dir string) ([]PGFileInput, []string, error) {
	var files []PGFileInput
	var skipped []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		if d.IsDir() {
			if path != dir && (scanSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		language := detectLanguage(path)
		if language == "unknown" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			skipped = append(skipped, rel+": "+err.Error())
			return nil
		}
		if info.Size() > scanMaxFileBytes {
			skipped = append(skipped, fmt.Sprintf("%s: larger than %d bytes", rel, scanMaxFileBytes))
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			skipped = append(skipped, rel+": "+err.Error())
			return nil
		}
		if bytes.IndexByte(content, 0) >= 0 {
			skipped = append(skipped, rel+": binary content")
			return nil
		}
		if len(files) >= scanMaxFiles {
			return fmt.Errorf("repository has more than %d indexable files", scanMaxFiles)
		}
		sum := sha256.Sum256(content)
		files = append(files, PGFileInput{
			Path:        filepath.ToSlash(rel),
			Language:    language,
			ContentHash: hex.EncodeToString(sum[:]),
			Content:     string(content),
		})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, skipped, err
}

// gitRevision reads the checked-out branch and commit from dir/.git without
// running git. A detached HEAD reports branch "detached". ok is false when
// dir is not a git checkout.
func gitRevision(dir string) (branch, commit string, ok bool) {
	gitDir := filepath.Join(dir, ".git")
	if data, err := os.ReadFile(gitDir); err == nil && bytes.HasPrefix(data, []byte("gitdir:")) {
		// A worktree or submodule: .git is a file pointing at the real dir.
		target := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		gitDir = target
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(string(head))
	if !strings.HasPrefix(line, "ref: ") {
		return "detached", line, line != ""
	}
	ref := strings.TrimPrefix(line, "ref: ")
	branch = strings.TrimPrefix(ref, "refs/heads/")
	commonDir := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		commonDir = filepath.Join(gitDir, strings.TrimSpace(string(data)))
	}
	for _, base := range []string{gitDir, commonDir} {
		if data, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(ref))); err == nil {
			return branch, strings.TrimSpace(string(data)), true
		}
	}
	if f, err := os.Open(filepath.Join(commonDir, "packed-refs")); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 2 && fields[1] == ref {
				return branch, fields[0], true
			}
		}
	}
	// A branch with no commits yet.
	return branch, "", true
}

// workingTreeID identifies uncommitted content: the hash of every scanned
// file's path and content hash. Two scans of an unchanged tree agree.
func workingTreeID(files []PGFileInput) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(f.ContentHash))
		h.Write([]byte{0})
	}
	return "worktree-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// handlePGScan indexes a registered repository from the workspace:
// POST /v2/code/scan {"repository_id": "..."}.
func (s *server) handlePGScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
		return
	}
	var req struct {
		RepositoryID string `json:"repository_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.RepositoryID) == "" {
		http.Error(w, "repository_id is required", http.StatusBadRequest)
		return
	}
	var owningTenant, canonical string
	if err := s.pgStore.db.QueryRowContext(r.Context(),
		`SELECT tenant_id::text, canonical_path FROM code_repositories WHERE id = $1`, req.RepositoryID).Scan(&owningTenant, &canonical); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load repository: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if owningTenant != tenant {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	dir, err := resolveWorkspacePath(canonical)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	files, skipped, err := scanRepository(dir)
	if err != nil {
		http.Error(w, "scan repository: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if len(files) == 0 {
		http.Error(w, "no indexable source files found under "+canonical, http.StatusUnprocessableEntity)
		return
	}
	branch, head, isGit := gitRevision(dir)
	if !isGit {
		branch = "workspace"
	}
	// The snapshot identity is the commit plus the working tree it was read
	// from: uncommitted edits are their own snapshot, and an unchanged tree
	// is recognised rather than indexed again.
	tree := workingTreeID(files)
	commit := tree
	if head != "" {
		commit = head + "+" + strings.TrimPrefix(tree, "worktree-")[:16]
	}
	if existing, status, err := s.pgStore.SnapshotByKey(r.Context(), req.RepositoryID, branch, commit, scanExtractorVersion, ""); err != nil {
		http.Error(w, "look up snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	} else if existing != "" && status != "failed" {
		writeJSON(w, map[string]any{"snapshot_id": existing, "status": status, "unchanged": true,
			"branch": branch, "commit_id": commit, "git_commit": head, "file_count": len(files)})
		return
	} else if existing != "" {
		if err := s.pgStore.DeleteFailedSnapshot(r.Context(), existing); err != nil {
			http.Error(w, "clear failed snapshot: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	snapshotID, err := s.pgStore.CreateSnapshot(r.Context(), req.RepositoryID, tenant, branch, commit, scanExtractorVersion, "")
	if err != nil {
		http.Error(w, "create snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.pgStore.SetSnapshotStatus(r.Context(), snapshotID, "indexing", ""); err != nil {
		http.Error(w, "mark snapshot indexing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// A real repository takes longer to parse than any client waits, so the
	// snapshot is built in the background under its own deadline; progress
	// and the outcome are read from GET /v2/code/snapshots/{id}.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		if _, err := s.buildSnapshot(ctx, tenant, req.RepositoryID, snapshotID, files); err != nil {
			log.Printf("scan of repository %s snapshot %s failed: %v", req.RepositoryID, snapshotID, err)
			if markErr := s.pgStore.SetSnapshotStatus(context.Background(), snapshotID, "failed", err.Error()); markErr != nil {
				log.Printf("record failure of snapshot %s: %v", snapshotID, markErr)
			}
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"snapshot_id": snapshotID, "status": "indexing", "branch": branch, "commit_id": commit,
		"git_commit": head, "file_count": len(files), "skipped": skipped,
	})
}

// scanTimeout bounds one background repository index.
const scanTimeout = 30 * time.Minute

// handlePGSnapshot reports one snapshot's indexing state:
// GET /v2/code/snapshots/{id}.
func (s *server) handlePGSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v2/code/snapshots/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	var out struct {
		ID           string  `json:"snapshot_id"`
		RepositoryID string  `json:"repository_id"`
		Branch       string  `json:"branch"`
		CommitID     string  `json:"commit_id"`
		Status       string  `json:"status"`
		FileCount    int     `json:"file_count"`
		SymbolCount  int     `json:"symbol_count"`
		Error        *string `json:"error,omitempty"`
		CreatedAt    string  `json:"created_at"`
		ActivatedAt  *string `json:"activated_at,omitempty"`
	}
	err := s.pgStore.db.QueryRowContext(r.Context(), `
		SELECT id::text, repository_id::text, branch, commit_id, status::text, file_count, symbol_count, error,
		       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       to_char(activated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		  FROM code_repository_snapshots
		 WHERE id::text = $1 AND tenant_id = $2::uuid`, id, tenant).Scan(&out.ID, &out.RepositoryID, &out.Branch, &out.CommitID,
		&out.Status, &out.FileCount, &out.SymbolCount, &out.Error, &out.CreatedAt, &out.ActivatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

// scanExtractorVersion names the extractor set a scan runs (Go AST with
// import-qualified call names, plus Tree-sitter), part of the snapshot's
// identity. Bump it whenever extraction output changes, so snapshots never
// reuse declarations parsed by an older extractor.
const scanExtractorVersion = "scan-goast-treesitter-v2"

// indexDurableSnapshot creates and builds one snapshot synchronously: the
// /v2/code/index path, whose uploads are bounded in size.
func (s *server) indexDurableSnapshot(ctx context.Context, tenant, repositoryID, branch, commit, extractorVersion, configurationHash string, files []PGFileInput) (map[string]any, error) {
	snapshotID, err := s.pgStore.CreateSnapshot(ctx, repositoryID, tenant, branch, commit, extractorVersion, configurationHash)
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	result, err := s.buildSnapshot(ctx, tenant, repositoryID, snapshotID, files)
	if err != nil {
		if markErr := s.pgStore.SetSnapshotStatus(context.Background(), snapshotID, "failed", err.Error()); markErr != nil {
			log.Printf("record failure of snapshot %s: %v", snapshotID, markErr)
		}
		return nil, err
	}
	return result, nil
}

// buildSnapshot indexes, embeds (when a code model runtime is configured)
// and diffs an already-created snapshot.
func (s *server) buildSnapshot(ctx context.Context, tenant, repositoryID, snapshotID string, files []PGFileInput) (map[string]any, error) {
	if err := s.pgStore.IndexSnapshot(ctx, snapshotID, repositoryID, files); err != nil {
		return nil, fmt.Errorf("index snapshot: %w", err)
	}
	degradedModels := []string{}
	if codeModelRuntimeConfigured() {
		if err := s.embedCodeSnapshot(ctx, tenant, snapshotID); err != nil {
			log.Printf("code embedding backfill degraded for snapshot %s: %v", snapshotID, err)
			degradedModels = append(degradedModels, "code_embedding")
		}
	}
	diff, err := s.pgStore.DiffSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("diff snapshot: %w", err)
	}
	var symbols int
	if err := s.pgStore.db.QueryRowContext(ctx,
		`SELECT symbol_count FROM code_repository_snapshots WHERE id = $1`, snapshotID).Scan(&symbols); err != nil {
		return nil, fmt.Errorf("count snapshot symbols: %w", err)
	}
	return map[string]any{
		"snapshot_id":     snapshotID,
		"file_count":      len(files),
		"symbol_count":    symbols,
		"diff":            diff,
		"degraded_models": degradedModels,
	}, nil
}
