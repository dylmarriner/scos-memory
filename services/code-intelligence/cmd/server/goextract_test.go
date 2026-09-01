package main

import (
	"strings"
	"testing"
)

func findDecl(decls []GoDeclaration, name string) (GoDeclaration, bool) {
	for _, d := range decls {
		if d.Name == name {
			return d, true
		}
	}
	return GoDeclaration{}, false
}

const sampleGoSource = `// Package sample is a fixture for extractGoDeclarations.
package sample

import "context"

// UserService handles user records.
type UserService struct {
	db string
}

// MaxRetries bounds retry attempts.
const MaxRetries = 3

var defaultTimeout, fallbackTimeout = 30, 5

// CreateUser creates a new user.
func CreateUser(ctx context.Context, name string) (*UserService, error) {
	return nil, nil
}

// Close releases the service's resources.
func (s *UserService) Close() error {
	return nil
}

func (s *UserService) Name() string {
	return s.db
}
`

func TestExtractGoDeclarationsPackageName(t *testing.T) {
	pkgName, _, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if pkgName != "sample" {
		t.Fatalf("expected package name 'sample', got %q", pkgName)
	}
}

func TestExtractGoDeclarationsFindsFunction(t *testing.T) {
	_, decls, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	fn, ok := findDecl(decls, "CreateUser")
	if !ok {
		t.Fatalf("expected to find CreateUser, got %+v", decls)
	}
	if fn.Kind != "function" {
		t.Fatalf("expected kind 'function', got %q", fn.Kind)
	}
	if fn.Receiver != "" {
		t.Fatalf("expected no receiver for a plain function, got %q", fn.Receiver)
	}
	if fn.Signature != "func CreateUser(ctx context.Context, name string) (*UserService, error)" {
		t.Fatalf("unexpected signature: %q", fn.Signature)
	}
	if fn.DocComment != "CreateUser creates a new user." {
		t.Fatalf("unexpected doc comment: %q", fn.DocComment)
	}
	if fn.FullyQualifiedName("sample") != "sample.CreateUser" {
		t.Fatalf("unexpected FQN: %q", fn.FullyQualifiedName("sample"))
	}
}

func TestExtractGoDeclarationsFindsMethodWithReceiver(t *testing.T) {
	_, decls, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	m, ok := findDecl(decls, "Close")
	if !ok {
		t.Fatalf("expected to find Close, got %+v", decls)
	}
	if m.Kind != "method" {
		t.Fatalf("expected kind 'method', got %q", m.Kind)
	}
	if m.Receiver != "*UserService" {
		t.Fatalf("expected receiver '*UserService', got %q", m.Receiver)
	}
	if got, want := m.FullyQualifiedName("sample"), "sample.(*UserService).Close"; got != want {
		t.Fatalf("FQN = %q, want %q", got, want)
	}
}

func TestExtractGoDeclarationsFindsType(t *testing.T) {
	_, decls, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	ty, ok := findDecl(decls, "UserService")
	if !ok {
		t.Fatalf("expected to find UserService, got %+v", decls)
	}
	if ty.Kind != "type" {
		t.Fatalf("expected kind 'type', got %q", ty.Kind)
	}
	if ty.DocComment != "UserService handles user records." {
		t.Fatalf("unexpected doc comment: %q", ty.DocComment)
	}
}

func TestExtractGoDeclarationsFindsConst(t *testing.T) {
	_, decls, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	c, ok := findDecl(decls, "MaxRetries")
	if !ok {
		t.Fatalf("expected to find MaxRetries, got %+v", decls)
	}
	if c.Kind != "const" {
		t.Fatalf("expected kind 'const', got %q", c.Kind)
	}
}

// A multi-name var spec ("var a, b = 30, 5") must produce one declaration
// per name, both anchored to the same source span.
func TestExtractGoDeclarationsHandlesMultiNameVarSpec(t *testing.T) {
	_, decls, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	a, ok := findDecl(decls, "defaultTimeout")
	if !ok {
		t.Fatalf("expected to find defaultTimeout, got %+v", decls)
	}
	b, ok := findDecl(decls, "fallbackTimeout")
	if !ok {
		t.Fatalf("expected to find fallbackTimeout, got %+v", decls)
	}
	if a.Kind != "var" || b.Kind != "var" {
		t.Fatalf("expected both to be kind 'var', got %q and %q", a.Kind, b.Kind)
	}
	if a.ContentHash != b.ContentHash {
		t.Fatalf("expected both names in one spec to share a content hash: %q vs %q", a.ContentHash, b.ContentHash)
	}
}

// Two structurally identical functions with different bodies must not
// collide onto the same content hash -- the whole point of hashing the
// declaration span rather than just the signature.
func TestExtractGoDeclarationsContentHashReflectsBody(t *testing.T) {
	_, decls, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	closeFn, _ := findDecl(decls, "Close")
	nameFn, _ := findDecl(decls, "Name")
	if closeFn.ContentHash == nameFn.ContentHash {
		t.Fatal("expected two different method bodies to hash differently")
	}
}

// An edit to one function's body must change only that function's content
// hash, not any other declaration's -- this is the fine-grained
// (sub-file) incremental identity the whole extractor exists to provide.
func TestExtractGoDeclarationsIsolatesChangesToOneFunction(t *testing.T) {
	_, before, err := extractGoDeclarations("sample.go", sampleGoSource)
	if err != nil {
		t.Fatalf("extract before: %v", err)
	}
	edited := sampleGoSource[:strings.Index(sampleGoSource, "return nil, nil")] +
		`return &UserService{db: "changed"}, nil` +
		sampleGoSource[strings.Index(sampleGoSource, "return nil, nil")+len("return nil, nil"):]
	_, after, err := extractGoDeclarations("sample.go", edited)
	if err != nil {
		t.Fatalf("extract after: %v", err)
	}

	beforeByName := make(map[string]string, len(before))
	for _, d := range before {
		beforeByName[d.Name] = d.ContentHash
	}
	changed := 0
	for _, d := range after {
		if beforeByName[d.Name] != d.ContentHash {
			changed++
			if d.Name != "CreateUser" {
				t.Fatalf("expected only CreateUser to change, but %s also changed", d.Name)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("expected exactly one declaration to change, got %d", changed)
	}
}

func TestExtractGoDeclarationsRejectsInvalidSyntax(t *testing.T) {
	if _, _, err := extractGoDeclarations("broken.go", "package broken\nfunc ("); err == nil {
		t.Fatal("expected a syntax error for invalid Go source")
	}
}
