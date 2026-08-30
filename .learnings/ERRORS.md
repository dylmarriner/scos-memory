# Errors

Command failures and integration errors.

---

## [ERR-20260727-002] invalid-go-test-package-path

**Logged**: 2026-07-27
**Priority**: low
**Status**: resolved
**Area**: tests

### Summary
The focused Go test command included a directory with no Go files.

### Error
`no Go files in services/context-optimisation`

### Context
- The service source is under `services/context-optimisation/cmd/server`.

### Resolution
Use the service's actual command directory in focused test invocations.

---

## [ERR-20260727-001] invalid-unittest-module-name

**Logged**: 2026-07-27
**Priority**: low
**Status**: resolved
**Area**: tests

### Summary
Direct unittest module selection cannot address the hyphenated `tests/failure-injection` directory.

### Error
`ModuleNotFoundError: No module named 'tests.failure-injection'`

### Context
- Attempted to run both focused test files as dotted unittest modules.
- The failure-injection tests must be run with discovery rooted at their directory or by invoking the runner directly.

### Resolution
Used `python3 -m unittest discover -s tests/failure-injection -p 'test_*.py' -v`.

---

## [ERR-20260804-001] unused-io-import-after-hook-decoder-hardening

**Logged**: 2026-08-04
**Priority**: low
**Status**: resolved
**Area**: tests

### Summary
Extension-manager tests initially failed to compile after replacing the hook dispatch decoder.

### Error
`services/extension-manager/cmd/server/main.go:15:2: "io" imported and not used`

### Resolution
Removed the obsolete `io` import after switching to `http.MaxBytesReader`.

---

## [ERR-20260830-001] graphify-root-not-configured

**Logged**: 2026-08-30
**Priority**: medium
**Status**: resolved (workaround); server-side gap still open
**Area**: config

### Summary
SCOS graphify rejected an explicit repository path because the graphify service has no configured allowed root.

### Error
`repo_path was supplied but GRAPHIFY_ALLOWED_ROOT is not configured; refusing to graph an unbounded path`

### Resolution
Confirmed: omitting `repo_path` and letting graphify use its configured default
repo (`GRAPHIFY_REPO_PATH`) works — `scos_graphify_query` without `repo_path`
successfully returned a 3746-node graph for this repo.

### Deeper issue (still open)
`services/graphify-provider/cmd/server/main.go` (checked into this repo, only
commit `f4f1108`) does not implement `repo_path` or `GRAPHIFY_ALLOWED_ROOT` at
all — `handleQuery`/`handlePath`/`handleExplain` only ever decode `q`/`from,to`/
`node` and always run against the fixed `s.repoPath`. The deployed provider
that produced this error is running code with a `repo_path`-per-request
feature (and its validation) that was never committed to this repo — i.e. the
live fleet has drifted ahead of `main` for this service. Whoever added
per-request `repo_path` support server-side should push that commit, or the
docs (`docs/agent-connection-guide.md`) and the `scos_graphify_query` tool
description should stop advertising `repo_path` as usable until it's merged
back.

### Suggested Fix
Configure `GRAPHIFY_ALLOWED_ROOT` for the graphify provider, or use its configured default repository by omitting `repo_path`.

---

## [ERR-20260830-002] graphify-ambiguous-node

**Logged**: 2026-08-30
**Priority**: low
**Status**: resolved
**Area**: tooling

### Summary
Graphify explain rejected a short node name because two graph nodes shared the same symbol name in different files.

### Error
`Ambiguous: 'graphifyServer' matches 2 nodes in different files.`

### Resolution
Retry the explain operation with the repo-relative path or full node ID.
