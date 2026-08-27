# ARCHIVED — merged into scos-memory-fabric

This repository is no longer maintained. As of 2026-08-27 its work has been
absorbed into **/media/kubuntux/DEVELOPMENT1/scos-memory-fabric**, which is
now the single line of development.

## Why this existed

Both repositories share the ancestor `d40ac4f` (2026-08-04) and then
diverged. This one spent its divergence on authorization — a scope layer
over every mutating route, tenant-ownership binding, and a hostile audit run
to completion with seven findings fixed. The fabric spent its divergence on
persistence, migrations, CI, the MCP gateway and agent integrations.

Keeping both alive meant every fix had to be made twice, and it wasn't: the
AUDIT-007 authorization fix committed here never reached the fabric, and the
fabric shipped a confirmed tenant-spoofing hole this repository did not have.

## Where the work went

| From here | Landed in fabric as |
|---|---|
| `packages/service-auth` scope model | grafted onto `packages/auth` (`HasScope`, `RequireScope`, `Scopes`/`JTI` claims) |
| AUDIT-007 recovery/backup scopes | `recovery:admin` / `backups:admin` gates on both services |
| AUDIT-006 tenant-from-verified-identity | the shape of the mcp-gateway F-01 fix |
| `audit-results/` synthesis set | `audit-results/from-scos-memory/` |
| `docs/authentication.md` | `docs/authentication-scos-memory.md` |
| `packages/service-auth/*.go` | `packages/auth/reference-serviceauth/` (as `.txt`, for the port still outstanding) |

Merge commit: `c8843f2` in scos-memory-fabric.
Comparative audit: `audit-results/comparative-divergence-audit.md` in fabric.

## What was NOT carried across

The per-service Dockerfiles here were superseded by the fabric's shared
`deploy/Dockerfile`. The in-memory service implementations were superseded
by the fabric's Postgres-backed stores — that was this repository's main
weakness and the fabric's main strength.

## Backup

A full archive of this repository including git history was taken before
merging:

    /media/kubuntux/DEVELOPMENT1/.scos-merge-backup-20260827-224058/scos-memory-full.tar.gz

The directory is left in place rather than renamed so existing session
history and tooling paths keep resolving. It is safe to move or delete once
you have confirmed the merge.
