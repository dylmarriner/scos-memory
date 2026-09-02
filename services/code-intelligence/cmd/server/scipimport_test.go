package main

import (
	"testing"

	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

func sampleSCIPIndex() *scip.Index {
	return &scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: "file:///repo"},
		Documents: []*scip.Document{
			{
				Language:     "python",
				RelativePath: "pkg/service.py",
				Symbols: []*scip.SymbolInformation{
					{
						Symbol:        "scip-python python . . pkg/service/create_user().",
						DisplayName:   "create_user",
						Kind:          scip.SymbolInformation_Function,
						Documentation: []string{"Creates a new user record."},
						SignatureDocumentation: &scip.Signature{
							Language: "python",
							Text:     "def create_user(name: str) -> User",
						},
					},
					{
						Symbol:      "scip-python python . . pkg/service/UserService#",
						DisplayName: "UserService",
						Kind:        scip.SymbolInformation_Class,
					},
				},
			},
		},
	}
}

// sampleSCIPIndexWithRelationships mirrors the Animal/Dog example from
// Relationship's own doc comment in scip.proto: Dog#sound() both
// implements and references Animal#sound(), and Dog# implements Animal#
// without referencing it -- the two independent relationship flags a
// correct ingestion must not conflate.
func sampleSCIPIndexWithRelationships() *scip.Index {
	return &scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: "file:///repo"},
		Documents: []*scip.Document{
			{
				Language:     "typescript",
				RelativePath: "pkg/animal.ts",
				Symbols: []*scip.SymbolInformation{
					{Symbol: "scip-typescript npm . . Animal#", DisplayName: "Animal", Kind: scip.SymbolInformation_Interface},
					{Symbol: "scip-typescript npm . . Animal#sound().", DisplayName: "sound", Kind: scip.SymbolInformation_Method},
				},
			},
			{
				Language:     "typescript",
				RelativePath: "pkg/dog.ts",
				Symbols: []*scip.SymbolInformation{
					{
						Symbol: "scip-typescript npm . . Dog#", DisplayName: "Dog", Kind: scip.SymbolInformation_Class,
						Relationships: []*scip.Relationship{
							{Symbol: "scip-typescript npm . . Animal#", IsImplementation: true},
						},
					},
					{
						Symbol: "scip-typescript npm . . Dog#sound().", DisplayName: "sound", Kind: scip.SymbolInformation_Method,
						Relationships: []*scip.Relationship{
							{Symbol: "scip-typescript npm . . Animal#sound().", IsImplementation: true, IsReference: true},
						},
					},
				},
			},
		},
	}
}

// sampleSCIPIndexWithOccurrenceReferences models one file:
//
//	def helper():        line 0
//	    return 1          line 1
//
//	def run():            line 3
//	    return helper()    line 4
//
// run()'s definition occurrence declares an enclosing range spanning lines
// 3-4 (its whole body); the occurrence at line 4 referencing helper's
// symbol (SCIP's SymbolRoles has no Definition bit set there) sits inside
// that range, so insertSCIPOccurrenceReferenceEdgesLocked should attribute
// it to run() and emit a REFERENCES edge to helper(). helper() itself
// declares no enclosing range, so nothing can (or should) be attributed
// *into* helper()'s body -- irrelevant here since helper's body has no
// references of its own.
func sampleSCIPIndexWithOccurrenceReferences() *scip.Index {
	return &scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: "file:///repo"},
		Documents: []*scip.Document{
			{
				Language:     "python",
				RelativePath: "pkg/sample.py",
				Symbols: []*scip.SymbolInformation{
					{Symbol: "scip-python python . . pkg/sample/helper().", DisplayName: "helper", Kind: scip.SymbolInformation_Function},
					{Symbol: "scip-python python . . pkg/sample/run().", DisplayName: "run", Kind: scip.SymbolInformation_Function},
				},
				Occurrences: []*scip.Occurrence{
					{
						Symbol:         "scip-python python . . pkg/sample/helper().",
						SymbolRoles:    int32(scip.SymbolRole_Definition),
						Range:          []int32{0, 4, 10},
						EnclosingRange: []int32{0, 0, 1, 12},
					},
					{
						Symbol:         "scip-python python . . pkg/sample/run().",
						SymbolRoles:    int32(scip.SymbolRole_Definition),
						Range:          []int32{3, 4, 7},
						EnclosingRange: []int32{3, 0, 4, 20},
					},
					{
						// The call to helper() inside run()'s body: a plain
						// reference, no Definition role set.
						Symbol: "scip-python python . . pkg/sample/helper().",
						Range:  []int32{4, 11, 17},
					},
				},
			},
		},
	}
}

func TestParseSCIPIndexRoundTrips(t *testing.T) {
	original := sampleSCIPIndex()
	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := ParseSCIPIndex(data)
	if err != nil {
		t.Fatalf("ParseSCIPIndex: %v", err)
	}
	if len(decoded.Documents) != 1 {
		t.Fatalf("expected one document, got %d", len(decoded.Documents))
	}
	doc := decoded.Documents[0]
	if doc.RelativePath != "pkg/service.py" || doc.Language != "python" {
		t.Fatalf("unexpected document: %+v", doc)
	}
	if len(doc.Symbols) != 2 {
		t.Fatalf("expected two symbols, got %d", len(doc.Symbols))
	}
	if doc.Symbols[0].DisplayName != "create_user" {
		t.Fatalf("unexpected first symbol: %+v", doc.Symbols[0])
	}
}

func TestParseSCIPIndexRejectsGarbageBytes(t *testing.T) {
	// A lone 0xFF byte is an incomplete varint (every byte with the
	// continuation bit set expects a following byte) -- reliably invalid
	// wire-format input, unlike an arbitrary text string, which protobuf's
	// lenient unknown-field skipping can sometimes decode into an empty
	// message without erroring at all.
	if _, err := ParseSCIPIndex([]byte{0xFF}); err == nil {
		t.Fatal("expected an error decoding a truncated varint")
	}
}

func TestSignatureTextPrefersSignatureDocumentation(t *testing.T) {
	sym := &scip.SymbolInformation{
		DisplayName:            "Foo",
		SignatureDocumentation: &scip.Signature{Text: "func Foo() error"},
	}
	if got := signatureText(sym); got != "func Foo() error" {
		t.Fatalf("expected signature documentation, got %q", got)
	}
}

func TestSignatureTextFallsBackToDisplayName(t *testing.T) {
	sym := &scip.SymbolInformation{DisplayName: "Foo"}
	if got := signatureText(sym); got != "Foo" {
		t.Fatalf("expected display name fallback, got %q", got)
	}
}
