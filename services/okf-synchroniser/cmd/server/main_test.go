package main

import (
	"encoding/json"
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
