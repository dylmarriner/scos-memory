# P3 stored source-hash manifest readback and offline Git comparison

**Status:** source-level development and disposable PostgreSQL verification.
No production deployment, production migration execution, or independent Git-tree attestation.

The authenticated `POST /v2/code/snapshots/manifest` route and
`scos_code_snapshot_manifest` MCP tool expose immutable stored file identities
for a specific active or superseded snapshot. This is read-only and
tenant/principal-gated. Request:

```json
{"snapshot_id":"<snapshot-uuid>","limit":100,"after_path":""}
```

The response includes `snapshot_id`, `repository_id`, `branch`,
`commit_id`, `parent_commit_id`, `files` and optional `next_after_path`.
Each file has `path`, `language`, `content_hash` (stored SHA-256 for
native source uploads). Cursor ordering is bytewise C collation and pages
contain at most 1000 files. Missing, foreign-tenant and incomplete snapshots
return 404. Historical superseded generations remain readable. Migration 000076 adds a concurrent C-collated (snapshot_id,path) keyset index.

Export **all pages** using the authenticated MCP tool or API, following
`next_after_path` until absent. Save each entire JSON response on one line
in a private local NDJSON file. Do not save tokens or headers in the file.
For example:

```bash
python3 scripts/scos_verify_snapshot_manifest.py \
  --git-dir /path/to/trusted/repository \
  --commit '<target-commit-sha>' \
  --snapshot-id '<server-snapshot-uuid>' \
  --pages-file /path/to/manifest-pages.ndjson
```

The verifier independently resolves the target Git commit, enumerates every
supported regular-file blob, hashes the actual Git blob bytes with a streaming
`git cat-file --batch` process, and compares the complete path, language,
SHA-256, ordering and page/commit/repository identity. It refuses truncated
exports, unsupported pseudo-hashes (including SCIP `scip:` fingerprints),
missing/extra paths, forged hashes and conflicting pagination metadata.
Git operations clear inherited `GIT_*` overrides; the script never contacts
SCOS or changes the repository. The verifier does not rely on working-tree
files and skips Git symlinks/submodules, as the worklist does.

**Trust boundary:** this independently detects divergence between exported
server-stored hashes and a locally trusted Git tree. It does *not* prove the
server parsed the bytes, establish a server-verified Git commit/source
relationship, cryptographically sign the export, prevent a compromised server
from lying, or validate a deployment. A metadata-only index client can still
claim a commit while supplying false hashes; a subsequent honest readback
and this verifier would expose that mismatch. For stronger attestation,
require trusted Git object ingestion or an authenticated, signed manifest
whose source bytes and commit tree were verified during indexing.

Verification (disposable PostgreSQL 17, all migrations, no live writes):

```bash
python3 -m unittest scripts/test_scos_verify_snapshot_manifest.py -v
SCOS_CODE_STORE_TEST_DSN='<disposable-Postgres-DSN>' \
SCOS_APP_TEST_DATABASE_URL='<restricted-role-DSN>' \
  go test ./services/code-intelligence/cmd/server -run '^TestPGSnapshotManifest'
go test ./services/mcp-gateway/cmd/server -run '^TestMCPSnapshotManifest'
```
