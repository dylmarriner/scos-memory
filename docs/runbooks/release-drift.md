# SCOS release revision drift (read-only verification)

The deployed SCOS Git checkout and GitHub's protected `main` branch are
different states. A healthy Docker deployment proves only that the currently
running containers pass their health checks, **not** that the release checkout
or running images match the current source branch.

On 2026-11-01, the Kubuntux release checkout
`/run/media/kubuntux/backup-dev-ext4/scos/scos-memory-release` was at
`a697a7e6e4fd9aaf310a8b49daec9aa7252968e2` whereas the live
`origin/main` Git reference was
`42c0fd995dcc21186ef0c65f81fdf378f24c24e1`.
PostgreSQL and the health-checked Compose containers were healthy, but
that did not eliminate release revision drift. Five later PRs were blocked
by the independent review requirement.

## Read-only check

Run the **new source-tree script** on the deployment host, from a checkout
that already contains `deploy_drift_doctor.py` (for example a reviewed
development worktree). Do **not** run the relative script path from the
stale release checkout, where this script may not exist. The `--repo` path
must point to the actual deployed checkout, which remains untouched:

```bash
cd /path/to/scos-memory-source
python3 deploy/scripts/deploy_drift_doctor.py \
  --repo /run/media/kubuntux/backup-dev-ext4/scos/scos-memory-release
```

The doctor uses only four read-only Git commands: repository-root check,
`rev-parse HEAD`, tracked-file `status --porcelain` with
`GIT_OPTIONAL_LOCKS=0` to prevent optional index refresh writes, and a live
`ls-remote --heads origin refs/heads/main` query. No `fetch`, `pull`,
`checkout`, `reset`, Docker operation, network mutation or credentials
are issued or printed. Untracked files are intentionally excluded from
the dirty flag because a production checkout may contain local-only
deployment data.

JSON fields: `release_sha`, `target_sha`, `source_ref`,
`matches_target`, `clean` and `status`. Exit codes:

- `0`: `aligned` (identical SHAs, no tracked changes).
- `1`: `drifted`, `modified`, or `drifted_modified`.
- `2`: `unknown` (invalid repository, unavailable Git remote, or
  untrusted output). No fallback to stale local `origin/main`.

A SHA mismatch proves the revisions differ but does **not** alone prove
ancestry, distance, successful migrations, image parity or runtime health.
Run the existing image/health/DB checks separately and obtain the required
owner-approved, reviewed and signed promotion before any production change.
Do not use this checker as an automatic deployment action or a substitute
for the production acceptance gates.
