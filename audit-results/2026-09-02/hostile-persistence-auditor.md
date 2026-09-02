# Specialist audit artifact

Skill: hostile-persistence-auditor
Agent: Codex coordinator
Scope: migrations, schemas, snapshot/memory persistence
Files/components inspected: `database/migrations/000001_initial.sql`, `000025`–`000029`, RLS policies, `pgstore.go`, memory store
Entry points inspected: repository ensure, snapshot create/index/activate, fact/version writes
Important callers/callees: v2 handlers → `CreateSnapshot`/`IndexSnapshot`; workers → memory tables
Workflows traced: append snapshot, write file/symbol versions, activate; fact version/supersession
Trust boundaries: caller-supplied file manifest/content hash, SQL constraints, RLS role
Attacker-controlled inputs: duplicate/partial files, false hashes, branch/commit IDs
Invariants tested: atomic activation and unique snapshot identity
Findings: `pgstore.go:168-170` writes file count into symbol count; `pgstore.go:249-259` creates a new file identity each snapshot; content hash is used as symbol stable key; arbitrary file lists can become active snapshots without a complete manifest
Suspicions requiring validation: migration ordering/duplicate policy numbering and live RLS bypass
Tests/commands executed: migration/source review; Go suite pass
Coverage gaps: live migration and restore drills
Areas explicitly not reviewed: production database contents
Recommended follow-up: expected-tree manifest, stable symbol/file identity tables, corrected counts, constraints and live role tests
