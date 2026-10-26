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
parsed, 119 reused files and 119 resolved edges at N=120. The existing
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

## Remaining acceptance

Benchmark representative multi-language repositories at 1k, 5k and 10k+
files; compare cold and warm caches, disk/DB sizes, p50/p95, changed-file
fractions, memory, extractor tiers and call-graph equivalence. Establish and
enforce quantitative latency and throughput thresholds before claiming P3
changed-content scaling. Profile the full-snapshot structural pass and
implement dependency-bounded recomputation where correctness allows.
