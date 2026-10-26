# P3 Git-to-SCOS indexing worklist

A deterministic, offline input generator for the existing authenticated
`POST /v2/code/index` route. This script does **not** access the network,
modify the Git checkout, or deploy to SCOS.

## Examples

First snapshot on a branch (the SCOS repository UUID must already exist):

```bash
python3 scripts/scos_git_worklist.py \
  --git-dir /path/to/checkout \
  --repository-id '<existing-repository-uuid>' \
  --branch main --commit '<commit-sha>' --initial \
  > /tmp/scos-initial-index.json
```

After the specified parent commit **has actually been indexed and activated**
for the same SCOS repository and branch:

```bash
python3 scripts/scos_git_worklist.py \
  --git-dir /path/to/checkout \
  --repository-id '<existing-repository-uuid>' \
  --branch main --parent '<already-indexed-parent-sha>' \
  --commit '<target-sha>' > /tmp/scos-incremental-index.json
```

The output is a bounded full-source JSON body for the normal authenticated SCOS index API.
Submit it through the existing trusted gateway, never to an untrusted host.
Treat the generated JSON as source code with appropriate file permissions.

## Invariants and limitations

- Enumerates the **whole target Git tree**, not the working directory or a
  partial diff. Files absent from the target manifest are deleted.
- Computes the true source SHA-256 for content_hash, not the Git object ID.
- Transmits source bytes for **all** supported files, including unchanged
  paths. The server only selects a reusable parent when the extractor version
  matches. Git ancestry cannot establish that server-side compatibility;
  sending full source allows safe re-extraction after an extractor upgrade.
  Where eligible, the server still reuses unchanged declarations and chunks.
- Never follows symlinks or submodules. Recognizes Go, Python, JavaScript,
  TypeScript and Rust extensions supported by the configured extractors.
  Unsupported grammars such as TSX are not represented as parsed code.
- Reads NUL-delimited Git trees, supports unusual UTF-8 filenames, and
  validates the parent's Git ancestry. It cannot prove that SCOS has already
  activated the corresponding parent snapshot; the caller must verify this.
- Refuses JSON bodies larger than the server's **4 MiB** request limit,
  including the final newline. It never emits an incomplete snapshot.
- Typed add/modify/delete/rename/copy persistence and large-snapshot
  streaming are separate outstanding P3 gates.

## Verification

```bash
python3 -m unittest scripts/test_scos_git_worklist.py -v
SCOS_CODE_STORE_TEST_DSN='<disposable-postgres-dsn>' \
  go test ./services/code-intelligence/cmd/server \
    -run '^TestPG(GitRemovedSymbolAbsentFromTarget|IncrementalSnapshotEquivalentToCleanRebuild)$'
```

The tests verify source provenance, atomic size failures, safe paths, deletion,
and clean versus incremental snapshot equivalence. These are small concrete
fixtures, not throughput benchmarks on real large repositories.
