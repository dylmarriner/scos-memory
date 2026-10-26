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
  --manifest-pages-file /private/manifest.ndjson \
  --event-pages-file /private/events.ndjson
```

For a first indexed snapshot with no indexed parent, use `--initial` instead
of `--parent`. With `--parent`, the resolved parent must be a **strict**
ancestor of the target. An incomplete page export, mismatched repository or
branch, forged source hash, or missing/incorrect event fails closed.

Verification:

```bash
python3 -m unittest scripts/test_scos_verify_snapshot_bundle.py -v
python3 -m unittest scripts/test_scos_verify_snapshot_manifest.py \
  scripts/test_scos_verify_snapshot_events.py -v
```

**Trust boundary:** Both readback APIs report server-stored metadata, and
Git commit labels originally come from the indexing caller. A compromised
server or malicious indexing client can still lie. This tool proves that
the *provided exports* are mutually consistent with a trusted local Git
tree, not that the server parsed those bytes, signed the exports, or has
an attested chain of custody. It does not establish representative P3
indexing performance, production migration safety or recovery readiness.
