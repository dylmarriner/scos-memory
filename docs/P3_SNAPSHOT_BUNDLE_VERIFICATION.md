# P3 joint Git snapshot export verification

**Status:** offline development verification, not server-side Git provenance
attestation, not signed evidence, and not production deployment.

A snapshot has two independently authenticated, paginated readback endpoints:
`POST /v2/code/snapshots/manifest` and `POST /v2/code/snapshots/events`.
Independently checking each against the same local Git repository is useful,
but does not by itself establish that the two exports came from the **same
repository and branch**. Their snapshot UUIDs and Git labels can coincide
while repository IDs or branches differ.

`scripts/scos_verify_snapshot_bundle.py` closes this *offline cross-export
identity gap*: it validates both complete paginated exports, requires identical
snapshot UUID, repository UUID, branch, target commit and indexed parent commit,
streams Git blob bytes to check stored SHA-256 hashes, and verifies the event
ledger's exact transition coverage and conservative rename/copy evidence.

Export **all** pages from each endpoint using an authenticated principal.
Save each response as one NDJSON line. The verifier does not fetch pages,
store credentials, or access SCOS. Use trusted local Git object IDs:

```bash
python3 scripts/scos_verify_snapshot_bundle.py \
  --git-dir /path/to/trusted/git/repo \
  --commit '<trusted-target-sha>' \
  --parent '<trusted-indexed-parent-sha>' \
  --snapshot-id '<snapshot-uuid>' \
  --repository-id '<independently-trusted-scos-repository-uuid>' \
  --branch '<independently-trusted-scos-branch>' \
  --manifest-pages-file /private/manifest.ndjson \
  --event-pages-file /private/events.ndjson
```

For a first indexed snapshot with no indexed parent, use `--initial` instead
of `--parent`. With `--parent`, the resolved parent must be a **strict**
ancestor of the target. An incomplete page export, mismatched repository or
branch, forged source hash, or missing/incorrect event fails closed.

**Trusted external identity (2026-10-27):** The optional `--repository-id`
and `--branch` flags must be supplied together. Obtain their values from an
independent trusted inventory or operator record, **not** from either exported
JSON document. Without these flags, the verifier checks that both exports
agree but cannot detect a consistently forged repository/branch claim across
both. A noncanonical UUID, missing paired flag, or mismatched trusted identity
fails closed. The JSON reader rejects duplicate keys at any nesting depth,
including conflicting repository, branch, event or file metadata that Python's
default JSON parser would otherwise silently overwrite. The shared
export parser also rejects nonstandard `NaN`, `Infinity` and `-Infinity`
constants and exponent-overflow numbers at any nesting depth, including
unknown fields, so malformed
evidence cannot be certified by Python's permissive JSON decoder. The
standalone manifest and event verifiers use this same strict parser, and all
four Git verification suites are exercised in CI.

**Standalone verifier identity binding (2026-10-31):** The individual
`scripts/scos_verify_snapshot_manifest.py` and
`scripts/scos_verify_snapshot_events.py` CLIs also accept the paired optional
`--repository-id` and `--branch` flags. Without them, each standalone verifier
checks the exported identity's syntax and page-to-page consistency, but a
consistently forged repository or branch label could pass. With them, the
export must match an **independently trusted** repository UUID and branch
exactly; a missing partner flag, noncanonical UUID, blank branch, or mismatch
fails closed. The JSON result reports `trusted_identity_checked` to distinguish
the two modes. Nine real-Git test methods cover matching and consistently
forged exports, partial flags, malformed trusted identities, unbound-mode
reporting and NUL-containing exported branch rejection (review hardening). The two
standalone verifiers remain offline consistency checks, **not** signed
server-origin attestations or proof of actual indexed bytes.

For example, append both of these arguments to either standalone verifier:

```bash
  --repository-id '<trusted-repository-uuid>' \
  --branch '<trusted-branch>'
```

Verification:

```bash
python3 -m unittest scripts/test_scos_verify_snapshot_bundle.py -v
python3 -m unittest scripts/test_scos_verify_snapshot_manifest.py \
  scripts/test_scos_verify_snapshot_events.py -v
```

**Git object integrity:** All Git subprocesses disable local replacement refs
(`GIT_NO_REPLACE_OBJECTS=1`) as well as inherited `GIT_*` overrides.
Otherwise, a malicious `refs/replace/*` entry can make a trusted commit SHA
resolve to a different tree, allowing forged manifest bytes to pass an offline
verification. A real-Git RED/GREEN regression covers this substitution.

**Legacy graft ancestry hardening (2026-10-27):** Git's deprecated
`info/grafts` file can invent commit parents even with
`GIT_NO_REPLACE_OBJECTS=1`. The shared Git reader refuses any nonempty graft
file, resolving its path through Git so linked worktrees cannot bypass the
check. The worklist and joint bundle verifier both previously accepted an
unrelated forged parent with a grafted target commit (real-Git RED/GREEN
regressions); a linked-worktree regression verifies the common-directory
check. Inspect any graft file manually before trusting ancestry. This remains
local verification only, not server attestation.

**Trust boundary:** Both readback APIs report server-stored metadata, and
Git commit labels originally come from the indexing caller. A compromised
server or malicious indexing client can still lie. Even externally bound
repository and branch labels do not prove the server indexed the Git bytes. This tool proves that
the *provided exports* are mutually consistent with a trusted local Git
tree, not that the server parsed those bytes, signed the exports, or has
an attested chain of custody. It does not establish representative P3
indexing performance, production migration safety or recovery readiness.
