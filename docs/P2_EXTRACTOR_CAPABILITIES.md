# P2 extractor capability matrix

The versioned machine-readable matrix is
[`P2_EXTRACTOR_CAPABILITIES.json`](P2_EXTRACTOR_CAPABILITIES.json).
`TestP2PublishedExtractorCapabilitiesMatchRuntime` checks every advertised
extension against the actual runtime detector and exercises real declaration
parsers for all five syntax-capable languages. The workspace-scan integration
test covers `.cjs`, `.mts` and `.cts` from files on disk through declaration
extraction.

| Language | Source extensions recognized by workspace scan | Extractor | Persisted confidence |
| --- | --- | --- | --- |
| Go | `.go` | Go AST (`go/parser`) | `static_analysis` |
| Python | `.py` | Tree-sitter Python | `syntax` |
| JavaScript | `.js`, `.jsx`, `.mjs`, `.cjs` | Tree-sitter JavaScript | `syntax` when parsed |
| TypeScript | `.ts`, `.tsx`, `.mts`, `.cts` | Tree-sitter TypeScript | `syntax` when parsed |
| Rust | `.rs` | Tree-sitter Rust | `syntax` |
| SQL | `.sql` | File-level only | `heuristic` |
| YAML | `.yaml`, `.yml` | File-level only | `heuristic` |
| Configuration | `.json`, `.toml`, `.env`, `.ini`, `.cfg` | File-level only | `heuristic` |

**Separate Tier 1 semantic path:** `/v2/code/index/scip` imports
externally generated SCIP indexes, recording `deterministic` symbol evidence
and relationships. SCOS does not run the SCIP compiler/indexer for these
languages. SCIP ingestion is not proof that a compiler-grade index exists for
every language or project.

**Limits:** Go's AST parser does not perform full package type checking.
Tree-sitter called names are syntactic and are linked only when unambiguous;
they do not resolve arbitrary imports or method receivers. JSX and TSX are
recognized extensions, but no dedicated JSX/TSX grammar is configured, so
syntax extraction for those files is not qualified. Invalid/unsupported syntax
can yield file-only evidence. The offline Git worklist intentionally accepts
a narrower set of extensions than workspace scan, so this table is **not** a
claim that every extension is supported by the Git worklist.

## Validation

```bash
go test ./services/code-intelligence/cmd/server -run '^TestP2(PublishedExtractorCapabilitiesMatchRuntime|WorkspaceScanIndexesModuleSuffixes)$' -count=1
```

A disposable PostgreSQL 17 database with migrations 000001–000071 also passed
`TestPGP2ModuleSuffixesPersistSyntaxProvenance`, proving scanned module files
persist function declarations with the correct syntax confidence and extractor
provenance. The full code-intelligence PostgreSQL suite passed on the same
disposable database. No production database was used.

These are local parser/scan and PostgreSQL acceptance tests. They are not an independent
semantic accuracy benchmark, complete language fixture-repository suite,
type-resolved reference qualification or production rollout evidence.
