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
	"github.com/dylmarriner/scos-memory/packages/workspace"
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
	canonical, err := s.pgStore.RepositoryPathForTenant(r.Context(), tenant, req.RepositoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load repository failed", http.StatusInternalServerError)
		return
	}

	dir, err := workspace.Resolve(canonical)
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
	if existing, status, err := s.pgStore.SnapshotByKey(r.Context(), tenant, req.RepositoryID, branch, commit, extractorVersion, ""); err != nil {
		http.Error(w, "look up snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	} else if existing != "" && status != "failed" {
		// A queued/in-progress snapshot is not a completed unchanged scan.
		// Return its existing job identity so callers can poll it, without
		// starting a second worker or claiming that its symbols are queryable.
		switch status {
		case "queued", "scanning", "parsing", "indexing", "validating":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"snapshot_id": existing, "status": status, "unchanged": false,
				"branch": branch, "commit_id": commit, "git_commit": head, "file_count": len(files),
			})
			return
		case "ready", "active", "superseded":
			// A fully indexed generation can be reused below.
		default:
			http.Error(w, "unexpected snapshot status: "+status, http.StatusConflict)
			return
		}
		if status == "superseded" {
			if err := s.pgStore.ReactivateSnapshot(r.Context(), existing); err != nil {
				http.Error(w, "reactivate snapshot: "+err.Error(), http.StatusInternalServerError)
				return
			}
			status = "active"
		}
		writeJSON(w, map[string]any{"snapshot_id": existing, "status": status, "unchanged": true,
			"branch": branch, "commit_id": commit, "git_commit": head, "file_count": len(files)})
		return
	} else if existing != "" {
		if err := s.pgStore.DeleteFailedSnapshot(r.Context(), existing); err != nil {
			http.Error(w, "clear failed snapshot: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	snapshotID, err := s.pgStore.CreateSnapshot(r.Context(), req.RepositoryID, tenant, branch, commit, extractorVersion, "")
	if err != nil {
		http.Error(w, "create snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.pgStore.SetSnapshotStatusScoped(r.Context(), tenant, snapshotID, "indexing", ""); err != nil {
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
			if markErr := s.pgStore.SetSnapshotStatusScoped(context.Background(), tenant, snapshotID, "failed", err.Error()); markErr != nil {
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
// snapshotView is a durable snapshot as the scan, status and listing
// endpoints report it.
type snapshotView struct {
	ID            string  `json:"snapshot_id"`
	RepositoryID  string  `json:"repository_id"`
	CanonicalPath string  `json:"canonical_path,omitempty"`
	Branch        string  `json:"branch"`
	CommitID      string  `json:"commit_id"`
	Status        string  `json:"status"`
	FileCount     int     `json:"file_count"`
	SymbolCount   int     `json:"symbol_count"`
	Error         *string `json:"error,omitempty"`
	CreatedAt     string  `json:"created_at"`
	ActivatedAt   *string `json:"activated_at,omitempty"`
}

const snapshotViewColumns = `snap.id::text, snap.repository_id::text, repo.canonical_path, snap.branch, snap.commit_id,
	snap.status::text, snap.file_count, snap.symbol_count, snap.error,
	to_char(snap.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(snap.activated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scanSnapshotView(row interface{ Scan(...any) error }) (snapshotView, error) {
	var v snapshotView
	err := row.Scan(&v.ID, &v.RepositoryID, &v.CanonicalPath, &v.Branch, &v.CommitID, &v.Status,
		&v.FileCount, &v.SymbolCount, &v.Error, &v.CreatedAt, &v.ActivatedAt)
	return v, err
}

// handlePGSnapshots lists the caller's most recent durable snapshots across
// every repository, newest first -- what has been indexed, what is
// indexing now, and what failed and why.
func (s *server) handlePGSnapshots(w http.ResponseWriter, r *http.Request) {
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
	rows, err := s.pgStore.db.QueryContext(r.Context(), `
		SELECT `+snapshotViewColumns+`
		  FROM code_repository_snapshots snap
		  JOIN code_repositories repo ON repo.id = snap.repository_id
		 WHERE snap.tenant_id = $1::uuid
		 ORDER BY snap.created_at DESC
		 LIMIT 50`, tenant)
	if err != nil {
		http.Error(w, "list snapshots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []snapshotView{}
	for rows.Next() {
		v, err := scanSnapshotView(rows)
		if err != nil {
			http.Error(w, "list snapshots: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "list snapshots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

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
	out, err := scanSnapshotView(s.pgStore.db.QueryRowContext(r.Context(), `
		SELECT `+snapshotViewColumns+`
		  FROM code_repository_snapshots snap
		  JOIN code_repositories repo ON repo.id = snap.repository_id
		 WHERE snap.id::text = $1 AND snap.tenant_id = $2::uuid`, id, tenant))
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

// extractorVersion names the extractor set every snapshot built here runs
// (Go AST with import-qualified call names, plus Tree-sitter) -- a scan and
// an uploaded /v2/code/index alike, since both index through buildSnapshot
// -- and is part of the snapshot's identity. Bump it whenever extraction
// output changes, so snapshots never reuse declarations parsed by an older
// extractor. SCIP imports are labelled by their own indexer instead.
const extractorVersion = "scan-goast-treesitter-v3"

// errSnapshotConflict marks a request for a snapshot key that already
// names a different tree, or one still being built.
var errSnapshotConflict = errors.New("snapshot conflict")

// indexDurableSnapshot creates and builds one snapshot synchronously: the
// /v2/code/index path, whose uploads are bounded in size.
func (s *server) indexDurableSnapshot(ctx context.Context, tenant, repositoryID, branch, commit, extractorVersion, configurationHash string, files []PGFileInput) (map[string]any, error) {
	// A snapshot's key names one tree. Sending the same tree again (a
	// client retry) returns the snapshot already built; different files
	// under a key that is already indexed are refused, never merged in.
	existing, status, err := s.pgStore.SnapshotByKey(ctx, tenant, repositoryID, branch, commit, extractorVersion, configurationHash)
	if err != nil {
		return nil, fmt.Errorf("look up snapshot: %w", err)
	}
	switch {
	case existing == "":
	case status == "failed":
		if err := s.pgStore.DeleteFailedSnapshot(ctx, existing); err != nil {
			return nil, fmt.Errorf("clear failed snapshot: %w", err)
		}
	case status == "ready" || status == "active" || status == "superseded":
		same, err := s.pgStore.SnapshotHoldsFiles(ctx, existing, files)
		if err != nil {
			return nil, fmt.Errorf("compare snapshot files: %w", err)
		}
		if !same {
			return nil, fmt.Errorf("%w: snapshot %s already holds different files for commit %q", errSnapshotConflict, existing, commit)
		}
		if status == "superseded" {
			if err := s.pgStore.ReactivateSnapshot(ctx, existing); err != nil {
				return nil, fmt.Errorf("reactivate snapshot: %w", err)
			}
			status = "active"
		}
		var symbols int
		if err := s.pgStore.db.QueryRowContext(ctx,
			`SELECT symbol_count FROM code_repository_snapshots WHERE id = $1`, existing).Scan(&symbols); err != nil {
			return nil, fmt.Errorf("count snapshot symbols: %w", err)
		}
		return map[string]any{
			"snapshot_id":     existing,
			"status":          status,
			"unchanged":       true,
			"file_count":      len(files),
			"symbol_count":    symbols,
			"degraded_models": []string{},
		}, nil
	default:
		return nil, fmt.Errorf("%w: snapshot %s for commit %q is still %s", errSnapshotConflict, existing, commit, status)
	}

	snapshotID, err := s.pgStore.CreateSnapshot(ctx, repositoryID, tenant, branch, commit, extractorVersion, configurationHash)
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	result, err := s.buildSnapshot(ctx, tenant, repositoryID, snapshotID, files)
	if err != nil {
		if markErr := s.pgStore.SetSnapshotStatusScoped(context.Background(), tenant, snapshotID, "failed", err.Error()); markErr != nil {
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
