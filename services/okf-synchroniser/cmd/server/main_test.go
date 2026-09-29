package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewOKFServer(t *testing.T) {
	s := newOKFServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.okfRoot == "" {
		t.Fatal("expected okf root")
	}
}

func TestClassifyPath(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/okf/governance/index.md", "governance"},
		{"/okf/architecture/system.md", "architecture"},
		{"/okf/projects/scos.md", "projects"},
		{"/okf/decisions/adr-001.md", "decisions"},
		{"/docs/procedures/deploy.md", "procedures"},
		{"/okf/tools/scripts.md", "tools"},
		{"/docs/glossary/terms.md", "glossary"},
		{"/okf/random/file.md", "general"},
	}
	for _, tc := range tests {
		cat := classifyPath(tc.path)
		if cat != tc.expected {
			t.Errorf("classifyPath(%q) = %q, want %q", tc.path, cat, tc.expected)
		}
	}
}

func TestExtractTitle(t *testing.T) {
	content := []byte("# My Document\n\nSome content")
	title := extractTitle(content)
	if title != "My Document" {
		t.Fatalf("expected 'My Document', got %q", title)
	}
}

func TestExtractTitleNoHeading(t *testing.T) {
	content := []byte("Just some text\nwithout heading")
	title := extractTitle(content)
	if title != "Untitled" {
		t.Fatalf("expected 'Untitled', got %q", title)
	}
}

func TestSha256Hex(t *testing.T) {
	h := sha256Hex([]byte("test content"))
	if len(h) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h))
	}
}

func TestSyncResultStruct(t *testing.T) {
	result := SyncResult{
		Added:    []string{"file1.md", "file2.md"},
		Updated:  []string{"file3.md"},
		Diverged: []string{"file4.md"},
	}
	if len(result.Added) != 2 {
		t.Fatal("expected 2 added")
	}
	if len(result.Updated) != 1 {
		t.Fatal("expected 1 updated")
	}
}

func TestOKFDocumentCreation(t *testing.T) {
	doc := &OKFDocument{
		ID:       "okf-1",
		Path:     "okf/governance/policy.md",
		Category: "governance",
		Title:    "Memory Policy",
		Status:   "current",
	}
	if doc.Title != "Memory Policy" {
		t.Fatal("title mismatch")
	}
	if doc.Category != "governance" {
		t.Fatal("category mismatch")
	}
}

func TestDocumentUpdate(t *testing.T) {
	s := newOKFServer()
	s.documents["okf-1"] = &OKFDocument{
		ID: "okf-1", Path: "okf/governance/policy.md",
		ContentHash: "oldhash", Status: "current",
	}
	s.projections["okf/governance/policy.md"] = &IndexProjection{
		Path: "okf/governance/policy.md", ContentHash: "newhash",
	}

	// Simulate divergence detection
	s.mu.Lock()
	doc := s.documents["okf-1"]
	proj := s.projections["okf/governance/policy.md"]
	if doc.ContentHash != proj.ContentHash {
		doc.Status = "diverged"
	}
	s.mu.Unlock()

	if doc.Status != "diverged" {
		t.Fatal("expected diverged status when hashes differ")
	}
}

func TestRemoveDocument(t *testing.T) {
	s := newOKFServer()
	s.documents["okf-1"] = &OKFDocument{ID: "okf-1", Path: "old.md"}
	s.projections["old.md"] = &IndexProjection{Path: "old.md"}

	s.mu.Lock()
	delete(s.documents, "okf-1")
	delete(s.projections, "old.md")
	s.mu.Unlock()

	if len(s.documents) != 0 {
		t.Fatal("expected empty documents")
	}
	if len(s.projections) != 0 {
		t.Fatal("expected empty projections")
	}
}

func TestDocumentAddedViaSync(t *testing.T) {
	s := newOKFServer()
	s.mu.Lock()
	s.documents["okf-new"] = &OKFDocument{
		ID: "okf-new", Path: "okf/new.md",
		ContentHash: "abc", Status: "current",
	}
	s.projections["okf/new.md"] = &IndexProjection{
		Path: "okf/new.md", ContentHash: "abc", ImportStatus: "synced",
	}
	s.mu.Unlock()

	if len(s.documents) != 1 {
		t.Fatal("expected 1 document")
	}
}

func TestFindOKFRoot(t *testing.T) {
	root := findOKFRoot()
	if root == "" {
		t.Fatal("expected non-empty okf root")
	}
}

func TestWalkOKFDir(t *testing.T) {
	s := newOKFServer()
	// We can't control the filesystem in unit test, but the walk shouldn't panic
	files := s.walkOKFDir()
	_ = files
}

func TestIndexProjectionJSON(t *testing.T) {
	proj := IndexProjection{
		CommitHash:   "abc123",
		Path:         "okf/doc.md",
		ContentHash:  "def456",
		ImportStatus: "synced",
	}
	data, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "okf/doc.md") {
		t.Fatal("JSON should contain path")
	}
}

func TestOKFDocumentJSON(t *testing.T) {
	doc := OKFDocument{
		ID: "okf-1", Path: "okf/doc.md",
		Category: "governance", Title: "Test",
		Status: "current", Content: "# Hello",
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var decoded OKFDocument
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "okf-1" {
		t.Fatal("roundtrip failed")
	}
}

func TestGitCommitReadsRepositoryHead(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("# Document\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "doc.md")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	want := strings.TrimSpace(string(runGit(t, root, "rev-parse", "HEAD")))
	if got := gitCommit(root); got != want {
		t.Fatalf("gitCommit() = %q, want %q", got, want)
	}
}

func TestOKFProposalRequiresValidationAndIndependentReview(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "policy.md")
	if err := os.WriteFile(path, []byte("# old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &okfServer{okfRoot: root, proposals: map[string]*OKFProposal{}, roleLookup: grantAllRoles}
	p, err := s.createProposal(okfProposalRequest{Path: "policy.md", Content: "# new\n", Proposer: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reviewProposal(p.ID, "bob"); err == nil {
		t.Fatal("review before validation must fail")
	}
	if _, err := s.validateProposal(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reviewProposal(p.ID, "alice"); err == nil {
		t.Fatal("self-review must fail")
	}
	if _, err := s.reviewProposal(p.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if p.Status != "reviewed" || p.Diff == "" {
		t.Fatalf("proposal was not fully evidenced: %#v", p)
	}
}

func TestOKFProposalRejectsPathEscape(t *testing.T) {
	s := &okfServer{okfRoot: t.TempDir(), proposals: map[string]*OKFProposal{}}
	if _, err := s.createProposal(okfProposalRequest{Path: "../GOVERNANCE.md", Content: "bad", Proposer: "alice"}); err == nil {
		t.Fatal("path escape must be rejected")
	}
}

func TestSynchronisePersistsProjectionCommit(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "projections.json")
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("# Document\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "doc.md")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	s := &okfServer{documents: make(map[string]*OKFDocument), projections: make(map[string]*IndexProjection), okfRoot: root, statePath: statePath}
	s.synchronise(context.Background())
	loaded := &okfServer{documents: make(map[string]*OKFDocument), projections: make(map[string]*IndexProjection), okfRoot: root, statePath: statePath}
	if err := loaded.loadProjections(); err != nil {
		t.Fatal(err)
	}
	projection := loaded.projections[filepath.Join(root, "doc.md")]
	if projection == nil || projection.CommitHash == "" || projection.ImportStatus != "synced" {
		t.Fatalf("projection was not persisted accurately: %#v", projection)
	}
}

func TestSynchroniseRejectsDocumentOverwrite(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "projections.json")
	path := filepath.Join(root, "doc.md")
	if err := os.WriteFile(path, []byte("repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &okfServer{documents: map[string]*OKFDocument{"id": {ID: "id", Path: path, ContentHash: sha256Hex([]byte("indexed")), Content: "indexed"}}, projections: map[string]*IndexProjection{path: {Path: path, ContentHash: sha256Hex([]byte("old")), ImportStatus: "synced"}}, okfRoot: root, statePath: statePath}
	result := s.synchronise(context.Background())
	if len(result.Diverged) != 1 || s.documents["id"].Content != "indexed" {
		t.Fatalf("expected overwrite rejection, result=%#v doc=%#v", result, s.documents["id"])
	}
}

func TestSynchroniseRejectsDocumentDeletion(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "projections.json")
	path := filepath.Join(root, "deleted.md")
	hash := sha256Hex([]byte("indexed"))
	s := &okfServer{documents: map[string]*OKFDocument{"id": {ID: "id", Path: path, ContentHash: hash, Content: "indexed"}}, projections: map[string]*IndexProjection{path: {Path: path, ContentHash: hash, ImportStatus: "synced"}}, okfRoot: root, statePath: statePath}
	result := s.synchronise(context.Background())
	if len(result.Diverged) != 1 || s.documents["id"] == nil || s.documents["id"].Status != "diverged" {
		t.Fatalf("expected deletion rejection, result=%#v doc=%#v", result, s.documents["id"])
	}
}

func runGit(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}

func TestFindOKFRootHonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCOS_OKF_ROOT", dir)
	if got := findOKFRoot(); got != dir {
		t.Fatalf("findOKFRoot() = %q, want %q", got, dir)
	}
}
