# P3 incremental indexing: reproducible scaling evidence

**Status: performance gate remains open.** A real PostgreSQL 17 fixture now
exercises a one-file change across 32, 128, and 256 Go source files and
compares it with clean indexing. This is **not** a representative large
repository, a statistically rigorous production benchmark, or proof of
proportional end-to-end indexing.

## Reproduce safely

Use a disposable, migrated PostgreSQL database only. The benchmark creates
repositories, snapshots and tenants and **must never target production**.

```bash
SCOS_CODE_STORE_TEST_DSN='<disposable-postgres-dsn>' \
  go test ./services/code-intelligence/cmd/server \
  -run '^$' -bench '^BenchmarkPGIndexSnapshotScaling$' \
  -benchtime=1x -count=3

SCOS_CODE_STORE_TEST_DSN='<disposable-postgres-dsn>' \
  go test ./services/code-intelligence/cmd/server \
  -run '^(TestPGIndexSnapshotChangedContentScaling|TestPGIncrementalSnapshotEquivalentToCleanRebuild|TestPGIncrementalCopyHasFileVersionLookupIndex)$'
```

Fixtures contain N files, one Go function per file and a chain of N-1 calls.
For incremental mode, a fully indexed active parent is created **outside the
timer**, then exactly one file changes and the remaining N-1 use the guarded
source-omitting parent-reuse path. Clean mode indexes the complete target into
a new repository. The timer measures `IndexSnapshot` only, not parent
preparation, repository creation or snapshot allocation. One timed iteration
per sub-benchmark, repeated three times. The integration test asserts 1
parsed, 119 reused files and 119 resolved edges at N=120. A separate
40-declaration regression checks complete declaration/chunk provenance after
bulk copy. The existing
clean-versus-incremental integration test compares symbols, chunks and edges.

## Measured development results (2026-10-26)

Disposable local PostgreSQL 17 container on AMD Linux; medians of three
single-iteration runs in milliseconds. Results are environment-dependent,
and the test database accumulated rows between runs.

| Files | Clean before index | Incremental before index | Clean after index + bulk copy | Incremental after index + bulk copy |
| ---: | ---: | ---: | ---: | ---: |
| 32 | 122.9 | 239.1 | 129.0 | 112.6 |
| 128 | 481.3 | 951.4 | 436.7 | 516.9 |
| 256 | 942.4 | 2629.7 | 977.1 | 909.8 |

The first measurements were from the existing per-declaration copy path.
The optimized measurements use identical target content in both modes;
the original baseline had a trivially different first function, so treat
the before/after comparison as directional, not a controlled A/B test.
An intermediate bulk-copy experiment without a `file_version_id` index
regressed (at 128 files, 1574.5 ms incremental). The new
`code_symbol_versions(file_version_id)` index, built online by migration
000074, removes historical-table scans and makes bulk copying competitive
with clean indexing. The copy path now inserts unchanged declaration
versions and chunks in bulk per file, preserving their original identity,
extractor evidence and chunk hashes.

**Important:** At 128 files, incremental indexing remains slower than clean
indexing in the optimized fixture; the 256-file case is faster but noisy.
End-to-end work still writes a full immutable target
snapshot and recomputes its call edges. The parser work scales with changed
content, but total writes and structural recomputation still scale with the
manifest. The P3 performance acceptance gate therefore remains **open**.

## Bounded call-edge insert experiment (2026-10-26, development branch)

The structural pass previously issued one PostgreSQL statement per resolved
call edge. It now resolves exactly the same ambiguity-constrained graph in
memory and inserts edges in batches of at most 200 rows (601 bind arguments).
All rows still belong to the snapshot's activation transaction. This removes
SQL round trips, **not** whole-snapshot graph resolution or full-snapshot
writes. An isolated PostgreSQL 17 container was migrated through 000074;
the full Go integration suite, vet, Python Git fixtures, and a real 202-file
cross-batch regression passed. The regression checks edge count, uniqueness,
extractor and confidence provenance. Small-fixture clean/incremental graph
equivalence also passed.

Sequential single-iteration benchmarks against the **same disposable database**
on AMD, three iterations per case, median milliseconds. The baseline was
measured after the batched version, not in randomized order; accumulating
database history and warm-cache effects mean the differences are directional
rather than a controlled performance guarantee:

| Files | Prior per-edge clean | Prior per-edge incremental | Batched clean | Batched incremental |
| ---: | ---: | ---: | ---: | ---: |
| 32 | 92.7 | 101.4 | 121.2 | 119.0 |
| 128 | 460.1 | 531.5 | 443.5 | 368.3 |
| 256 | 878.9 | 828.6 | 809.6 | 826.8 |

A further 1024-file **Go-only** benchmark (three runs, one changed file)
recorded median clean 3463.8 ms and incremental 3244.9 ms, with ~18.8 MiB
versus ~15.3 MiB allocated per operation. This is one synthetic package,
not a representative multi-language repository. The 32-file incremental
case regressed in the directional comparison, and 256 was effectively
unchanged; **no general speedup claim or P3 performance sign-off follows**.
A bounded dependency-aware structural recomputation design, realistic
1k–10k+ multi-language tests, and explicit latency budgets remain necessary.

## Remaining acceptance

Benchmark representative multi-language repositories at 1k, 5k and 10k+
files; compare cold and warm caches, disk/DB sizes, p50/p95, changed-file
fractions, memory, extractor tiers and call-graph equivalence. Establish and
enforce quantitative latency and throughput thresholds before claiming P3
changed-content scaling. Profile the full-snapshot structural pass and
implement dependency-bounded recomputation where correctness allows.


## Qualified-call candidate index (2026-10-26, development)

The syntax-derived call resolver now indexes package-level function
candidates by **name and repository directory**, rather than scanning every
same-name function for each qualified import-path call. It enumerates only
slash-boundary suffixes of the import path, preserving the existing
`importPathNamesDir` resolution and ambiguity rules. A deterministic
regression checks nested directory suffixes, same-name ambiguity, root
exclusion, method exclusion and trailing slashes against the original
reference predicate. This is an algorithmic improvement to the structural
pass, **not dependency-bounded incremental recomputation**.

A synthetic 3,000-function fixture with the same function name in different
packages, one qualified call per function, measured on AMD Ryzen 5 2600
(`go test -run '^$' -bench '^BenchmarkResolveCallEdgesQualifiedSameNameFanout$'
-benchtime=3x -count=1`):

| Resolver | ns/op (3 iterations) | B/op | Resolved edges |
| --- | ---: | ---: | ---: |
| Prior same-name scan (main 74df657) | 253,608,996 | 3,370,018 | 3,000 |
| Name + directory suffix index | 4,031,684 | 2,349,602 | 3,000 |

Seven real PostgreSQL 17 integration regressions passed on a pre-existing disposable
P3 database (cross-batch edges, clean/incremental equivalence, unambiguous
and ambiguous calls, unchanged-file cross-file edges, cross-package
resolution, and nested-directory suffix ambiguity). The nested-directory
regression also ran inside the disposable PostgreSQL container against a
compiled Go test binary using local socket authentication. The full uncached Go suite, Go vet and 29 Git Python fixtures
also passed on AMD. No live migration or production deployment occurred.

These are microbenchmarks, not representative indexing latency, not randomized
and not a claim of full P3 scaling acceptance. Full snapshot graph construction
and persistence still occur for each immutable snapshot.


## Identical-manifest CALLS graph reuse (2026-10-26 development branch)

A conservative incremental fast path now copies native CALLS edges from the
compatible parent inside the new snapshot's indexing/activation transaction,
**only** when the complete target path/hash/language manifest matches its
parent. It additionally compares every declaration's stable symbol ID, path,
FQN, kind, scope, called-name evidence, extractor, hash, signature and range.
A duplicate-content reorder that swaps symbol identities, a changed file,
or a parent containing non-native graph edges forces a fresh resolver pass.
All copied edges are remapped to **target snapshot symbol-version UUIDs**,
preserving immutable history and edge confidence/extractor metadata.
Migration 000075 adds atomic `copied_call_edge_count` and
`resolved_call_edge_count` metrics and a versioned `call_resolver_version`
provenance column. Parent graphs indexed by an older resolver version
(including pre-migration `legacy` rows) are never copied; they are
re-resolved. The `nativeCallResolverVersion` constant must be incremented
whenever call-resolution semantics change independently of the extractor.

Two test-first PostgreSQL 17 regressions were observed RED before their
implementations and GREEN afterward: missing resolved-edge telemetry and
stale resolver-version provenance. Four integration test functions cover
identical/reordered complete manifests, deletion of a call target followed
by restoration, duplicate-content identity reassignment, non-native
parent-edge rejection, and legacy resolver-version rejection. All 75 migrations
also applied successfully to a fresh disposable PostgreSQL 17 database,
followed by the four targeted integration tests. The full Go suite against an isolated,
fully migrated PostgreSQL 17 database, `go vet ./...`, and 29 Git Python
fixtures passed. No production service or paid cloud resources were used.

Single-iteration `BenchmarkPGIndexSnapshotIdenticalManifest` on the same
local disposable PostgreSQL 17 container, three sequential repeats per case:

| Source files | Native CALLS edges copied | Elapsed times (ms) | Median (ms) |
| ---: | ---: | --- | ---: |
| 128 | 127 | 604.2, 422.1, 413.5 | 422.1 |
| 1024 | 1023 | 3262.3, 3322.9, 3304.1 | 3304.1 |

The benchmark excludes parent indexing and snapshot allocation, but includes
immutable target file/declaration/chunk writes and call-edge copying. The
1024-file case still takes approximately 3.3 seconds: **this is not an
end-to-end proportional indexing speedup**. No randomized baseline A/B,
multi-language workload, 5k–10k+ repository, resource budget or production
p50/p95 evidence is available. The P3 changed-content dependency-bounded
recomputation and performance acceptance gates remain open.


## Changed-manifest dependency-bounded CALLS reuse (2026-10-26)

The incremental indexer now avoids re-resolving native CALLS edges for
**unchanged, independent scopes**, even when other scopes changed. It first
collects changed/deleted file scopes from *both* snapshots; all qualified
callers are conservatively invalidated because a cross-package target may
have appeared, disappeared or become ambiguous. A single transaction checks
parent resolver provenance, native-only edges, complete declaration identity
and graph-relevant fingerprints in copied scopes, and absence of edges from
copied scopes into dirty scopes. Any mismatch falls back to full resolution.

PostgreSQL 17 integration tests use a disposable database with all 74
migration files. An observed RED fixture initially reported 0 copied and 2
resolved edges for an independent-package change; after implementation the
same fixture reports 1 copied and 1 resolved. Additional fixtures verify
qualified cross-package invalidation, deleted-scope isolation, legacy
resolver rejection, non-native parent edge rejection, and stable
duplicate-content identities. A 128-file, four-package test with one
changed file reports **93 copied CALLS edges, 31 resolved CALLS edges,
1 parsed file, 127 reused files**, and matches a fresh clean rebuild's
qualified source/target edge set exactly. These are work counters and
equivalence evidence, **not** an end-to-end speedup or a multi-language
throughput acceptance result.

Repeated single-iteration PostgreSQL 17 indexing benchmarks on the AMD
Ryzen 5 2600 compared identical one-changed-file four-package workloads.
Parent indexing, fixture creation and snapshot allocation were excluded.
The control forced a legacy parent resolver version to disable graph reuse
while preserving unchanged-file declaration reuse. Three repeats per case:

| Files | Partial graph copy median | Full graph resolution median | Interpretation |
| ---: | ---: | ---: | --- |
| 128 | 409.8 ms | 415.4 ms | Within run-to-run variation |
| 1,024 | 3,296.2 ms | 3,238.9 ms | No end-to-end improvement demonstrated |

The 1,024-file partial case allocated about 14.3 MB versus 15.3 MB for
full resolution. This work avoids unnecessary call resolution and reduces
allocations in this fixture, but snapshot/database write costs still
dominate and measured wall time does **not** establish a speedup. The
comparison is a controlled development fixture, not production throughput.

The remaining acceptance work includes 1k–10k+ representative multi-language
fixtures, repeated wall-clock/resource benchmarks, defined regression budgets,
trusted Git-commit provenance and production rollout/recovery evidence.
