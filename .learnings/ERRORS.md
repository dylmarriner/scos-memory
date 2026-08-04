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
