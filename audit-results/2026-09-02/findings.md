# Findings — 2026-09-02

| ID | Severity | Status | Evidence |
|---|---|---|---|
| CI-001 | P0 | Confirmed | `pgstore.go:168-170` assigns `len(files)` to both counters |
| CI-002 | High | Confirmed | `pgstore.go:249-275` recreates file rows and content-hash identities per snapshot |
| CI-003 | High | Confirmed | `pghandlers.go:177-235` accepts arbitrary non-empty manifest and activates it |
| CI-004 | High | Confirmed limitation | Tree-sitter bare-name same-module edges are not compiler resolution |
| GF-001 | Medium/High | Plausible | `graphify.go:218-252` cache miss has no inter-process lock/atomic child-output proof |
| AUTH-001 | High | Unproven/high concern | no FORCE RLS or deployed NOBYPASSRLS role found |
| GF-002 | Medium | Confirmed | Graphify edge confidence is collapsed to two memory relation types |
