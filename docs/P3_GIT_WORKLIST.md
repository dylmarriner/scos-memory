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

The output is a bounded JSON body for the normal authenticated SCOS index API.
An incremental request marks unchanged paths with `reuse_parent: true` and
supplies the exact `parent_commit_id`. The server checks that the active parent
uses the same extractor/configuration and has matching source evidence. If it
returns HTTP 409, **do not retry the same delta unchanged**. Rebuild a
full-source request instead:

```bash
python3 scripts/scos_git_worklist.py \
  --git-dir /path/to/checkout \
  --repository-id '<existing-repository-uuid>' \
  --branch main --parent '<already-indexed-parent-sha>' \
  --commit '<target-sha>' --full-source \
  > /tmp/scos-full-source-retry.json
```

The full-source mode is also appropriate after changing the server extractor
or configuration. Both modes retain the complete target manifest.
Submit it through the existing trusted gateway, never to an untrusted host.
Treat the generated JSON as source code with appropriate file permissions.

## Invariants and limitations

- Enumerates the **whole target Git tree**, not the working directory or a
  partial diff. Files absent from the target manifest are deleted.
- Computes the true source SHA-256 for content_hash, not the Git object ID.
- Default incremental mode sends changed source and marks unchanged same-path
  blobs with `reuse_parent: true`, omitting their bytes. The server requires
  the exact `parent_commit_id` to be active, matching extractor version,
  configuration, file path, language, source SHA-256 and persisted file-level
  source chunk before it may copy declarations and evidence. Otherwise the
  entire delta fails with HTTP 409; no partial snapshot is activated.
- `--full-source` sends all source bytes, including unchanged files. This is
  the safe fallback after a 409 or an extractor upgrade, without trusting Git
  ancestry as proof of server-side parent compatibility. Metadata-only legacy
  clients without `reuse_parent` retain their existing Tier 0 behavior.
  Empty files can reuse their exact SHA-256 zero-byte identity even though no
  source chunk exists. Unchanged whitespace-only files that fit the request
  cap are sent as source instead of attempting invalid chunk reuse. Files
  exceeding the cap remain unsupported by the bounded endpoint; this is not
  large-repository streaming.
  Older servers reject the new `reuse_parent` field rather than silently
  accepting a potentially incomplete source-omitting delta.
- Never follows symlinks or submodules. Recognizes Go, Python, JavaScript,
  TypeScript and Rust extensions supported by the configured extractors.
  Unsupported grammars such as TSX are not represented as parsed code.
- Reads NUL-delimited Git trees, supports unusual UTF-8 filenames, and
  validates the parent's Git ancestry. It cannot prove that SCOS has already
  activated the corresponding parent snapshot; the caller must verify this.
- Refuses JSON bodies larger than the server's **4 MiB** request limit,
  including the final newline. It never emits an incomplete snapshot.
- Native server-derived typed add/modify/delete/rename/copy persistence is
  implemented (migration 000072) and exposed by the authenticated snapshot
  event API. The offline Git sidecar is **not** trusted as server evidence;
  end-to-end Git tree/source attestation and large-snapshot streaming remain
  outstanding P3 gates.

Git subprocesses also force `GIT_NO_REPLACE_OBJECTS=1`: repository-local
`refs/replace/*` must never substitute forged source under a trusted commit
SHA. A real-Git regression reproduced that substitution before the fix.

Git subprocesses clear inherited `GIT_*` environment overrides before reading the explicitly selected repository, including the verifier, worklist, object streaming and ancestry checks. Real-Git fixtures assert that an ambient foreign `GIT_DIR`/`GIT_WORK_TREE` cannot redirect the results.

## Verification

```bash
python3 -m unittest scripts/test_scos_git_worklist.py -v
SCOS_CODE_STORE_TEST_DSN='<disposable-postgres-dsn>' \
  go test ./services/code-intelligence/cmd/server \
    -run '^(TestPG(GitRemovedSymbolAbsentFromTarget|IncrementalSnapshotEquivalentToCleanRebuild|DeltaReuse)|TestHandlePGIndexDelta)'
```

The tests verify source provenance, atomic size failures, safe paths, deletion,
clean versus incremental snapshot equivalence, and fail-closed parent
compatibility (extractor/configuration/commit/path/hash/language/source evidence).
These are small concrete fixtures, not throughput benchmarks on real large repositories.

## Offline event sidecar (not a server attestation)

Use the same Git worklist arguments with `--events-only` to output deterministic
Git add/modify/delete/rename/copy events without changing the existing index
request schema. The sidecar is for local inspection and validation only. Git
copy detection is heuristic and can report additions instead; it is not proof
of persisted events or full P3 completion. The sidecar includes the
caller-declared SCOS `repository_id` and `branch` beside the resolved target
and parent commit IDs, allowing local consumers to cross-check identity.
Both output modes reject invalid SCOS identities, empty parent revisions,
identical parent/target commits, and non-ancestor parents. These values remain
**caller declarations**, not server-signed or server-verified Git provenance.


## Verify exported snapshot events against trusted Git trees (offline)

`scripts/scos_verify_snapshot_events.py` checks a **complete export** of the
authenticated `POST /v2/code/snapshots/events` pages against the committed
parent and target Git trees. Save each JSON response page as one NDJSON line
in cursor order, following `next_after_path` until it is absent. Never put
authentication tokens into this file. For example:

```bash
python3 scripts/scos_verify_snapshot_events.py \
  --git-dir /path/to/checkout \
  --parent '<already-indexed-parent-sha>' --commit '<target-sha>' \
  --snapshot-id '<server-snapshot-uuid>' \
  --pages-file /path/to/complete-export.ndjson
```

Use `--initial` instead of `--parent` for a first snapshot. The verifier
checks exact supported-file path coverage, modifications, deletions, source
fingerprint continuity for renames/copies, unique-source provenance, evidence
classes, snapshot ID, ordered pages and complete cursor termination. It
accepts conservative add/delete classification when Git rename heuristics
and server-derived evidence disagree. It fails closed on missing or forged
events and never contacts SCOS, modifies Git or deploys code.

**Limit:** this is offline consistency checking, **not server-side Git
attestation**. The event API now returns authenticated server-recorded
repository, branch, target and parent commit identifiers; the verifier
requires these to match the selected Git commits and stay identical on every
page. The index caller originally supplied those commit identifiers, however,
and no server-verified Git blob/source hash manifest is attested. A malicious
index caller could claim the correct commit while supplying different source
bytes. That integrity gap and server-enforced provenance remain P3 work.
