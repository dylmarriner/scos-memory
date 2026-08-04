# ADR-0004: Extension packages are signed, isolation-scoped, and fail closed

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §22 requires plugins, skills, hooks and adapters to be
treated as executable supply-chain components: signed manifests, least-
privilege isolation, dependency locks, and atomic rollback. §35 requires an
ADR covering extension governance specifically.

## Decision

`services/extension-manager` is the sole installer. Installation SHALL
fail closed (HTTP 403) when:

- The manifest fails structural validation (`validateManifest`): missing
  `schema_version`, `name`, `publisher`, `license`, `extension_types`,
  `content_hash`, `required_core_version`, or `isolation`.
- The Ed25519 signature over the unsigned manifest bytes does not verify
  against `SCOS_EXTENSION_SIGNING_PUBLIC_KEY` (`verifySignature`).
- The declared `isolation` mode does not match the mode the extension's
  types require (`determineIsolation`) — plugins/providers require
  `separate_process`, UI extensions require `wasm`, everything else is
  `in_process`.
- `required_core_version` is incompatible with the running core version
  (`compatibleVersion`).
- Any declared dependency is not installed, not enabled, or version-
  incompatible (`validateInstallManifest`'s dependency loop).

Every successful install overwrites the extension's live manifest but
appends the prior manifest to `history[package_id]`, giving `rollback` a
version to restore.

## Consequences

- No unsigned or under-specified extension can reach `enabled` state.
- Dependency version drift is caught at install time, not at hook-execution
  time.
- Rollback always restores exactly the previous manifest, not a
  reconstructed approximation.

## Compatibility

The manifest schema (`schemas/extensions/extension-manifest.schema.json`)
is additive-only; new optional fields do not break already-signed
manifests because the signature covers the manifest as submitted, not a
fixed field set frozen at ADR time.

## Migration

None required at the database level — extension state lives in
`extension_manager_state` (single-row JSON) plus the durable local store,
both write-through on every mutating call, so no separate migration step
exists for extension data.

## Rollback

`POST /v1/extensions/{package_id}/rollback` pops the most recent entry
from `history[package_id]`, re-registers its hooks/skills, and re-derives
isolation from the restored manifest. If no history entry exists, rollback
returns 503 rather than fabricating a prior state.

## Index rebuild

Not applicable — extensions have no derived search index; the capability
graph (`/v1/capabilities`) is recomputed on every read from live
`extensions` map state, not cached.

## Verification

`services/extension-manager/cmd/server/main_test.go` covers signature
rejection, dependency-unsatisfied rejection, isolation-mismatch rejection,
and rollback-without-history. `tests/failure-injection` extension category
(`extension_malicious_manifest`, `extension_invalid_signature`,
`extension_dependency_conflict`, `extension_upgrade_migration_failure`)
covers the same boundary under process-kill and container conditions.
