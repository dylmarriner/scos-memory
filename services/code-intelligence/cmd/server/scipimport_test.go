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
