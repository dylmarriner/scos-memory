# SCOS Memory Fabric — P0–P8 Engineering Roadmap

> **Canonical roadmap as of 2026-09-10.** Repository state is authoritative. This roadmap distinguishes source implementation, benchmark acceptance, merge state, and live-production activation instead of treating them as one checkbox.
>
> `docs/IMPLEMENTATION_PLAN_P0_P3.md` is historical implementation detail. This file is the canonical P0–P8 roadmap.

## P7 restricted-role standing-answer write integrity (2026-10-28; merged PR #124)

A restricted `scos_app` SQL writer could previously rewrite a freshly
materialized standing answer to arbitrary text while retaining a valid
supporting fact UUID. A second real-PostgreSQL reproduction changed the source
summary and then cleared `stale_at`, causing the old generated answer to be
served as fresh. Both were observed RED through authenticated HTTP reads on a
disposable PostgreSQL 17/pgvector instance before the fix.

Migration `000087` adds a restricted-role BEFORE-write integrity gate:
**fresh runtime cache rows must use `verified_fact_title_summary` and match
one same-tenant/namespace fact's normalized title, exact canonical summary and
confidence**. It prevents downgrading generated provenance, blocks runtime
DELETE, and invalidates all previously fresh cache rows under a migration
lock because historical direct writes cannot be retroactively verified.
Owner-curated legacy answers remain a privileged maintenance operation.
The runtime blanket-grant policy is also updated so fresh installs do not
reintroduce DELETE. Real PostgreSQL integration tests verify direct answer
forgery, provenance downgrade, stale-source resurrection, arbitrary fresh
INSERT, confidence inflation, DELETE denial, and governed HTTP refresh.
Legacy/curated test fixtures now seed via the schema owner while retaining
restricted-role HTTP/RLS read coverage.
Review identified a Go `strings.TrimSpace` versus PostgreSQL `btrim`
mismatch for boundary Unicode whitespace. Restricted-role HTTP regressions
reproduced HTTP 500 for tab/NBSP-padded titles (RED); migration `000087`
now matches Unicode White_Space trimming (GREEN). This migration does not
establish
general natural-language answer generation, provenance of the underlying
memory's original creation, fleet rollout/recovery qualification, or P7/P0-P8
completion.

**Exact-head merge and merged-main verification:** [PR #124](https://github.com/dylmarriner/scos-memory/pull/124)
head `41501531e9dc242dc7da74936c6d79dc84d530f7` was
squash-merged using its exact expected head SHA into
`ab1d634265c10efa526a11387ab916ca9a0bdfc1`.
The substantive Codex review finding (Unicode boundary normalization)
was reproduced RED, corrected and its thread resolved before merge.
Kilo review and all required PR-head checks passed; CodeRabbit and
Claude did not perform a substantive review due to usage limits.
Independent merged-main verification on AMD passed the complete Go
suite (`go test ./...`) and `go vet ./...`; the canonical §41
definition-of-done checker still correctly reports FAIL.
Merged-main [CI](https://github.com/dylmarriner/scos-memory/actions/runs/40634770031)
passed 5/5 jobs and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40634770055)
passed 9/9 jobs, both at the exact merged-main SHA.
These results do not qualify a live migration, production cache
invalidation/recovery, or general P7 answer generation.

## P7 deterministic single-fact standing-answer refresh (2026-10-28; merged PR #122)

A restricted-role HTTP refresh path now derives an exact-title cache entry
from a current memory fact's canonical summary, never from caller-supplied
answer prose. It holds a source SHARE lock, enforces namespace candidate-write
permission, validates supporting evidence through the existing trigger and
security-invoker current-evidence view, and refuses to overwrite curated or
foreign-source answers. The new migration `000086` invalidates cached answers
on source title/summary changes without a version bump and marks all pre-upgrade
fresh entries stale because earlier text drift was not tracked. Real PostgreSQL
RED/GREEN regressions cover that stale-answer path, cross-tenant isolation,
read-only membership, revocation, collisions and UUID canonicalization.
PR #122 review uncovered two additional defects: a stale generated same-title
entry could not be replaced by a new eligible fact, and unresolved P7 code
revalidation was surfaced as HTTP 500 instead of a deterministic 409.
Both failures were reproduced against a separate disposable PostgreSQL 17
instance before correction and passed five repeated RED/GREEN-focused runs
after correction. The fix still rejects fresh different-source collisions and
stale curated answers, and requires governed still_valid review decisions
before refreshing code-affected evidence. Full tagged memory-intelligence and
retrieval-orchestrator integration suites passed after the review correction.

This is **not** a complete governed answer-generation pipeline: exact-title
materialization does not infer answers to arbitrary questions, existing direct
SQL standing-cache writes are not yet fenced into this governed endpoint,
retrieval integration is absent, and production migration locks, invalidation
blast radius, rollout and recovery require separate approval and qualification.
See `docs/P7_DETERMINISTIC_STANDING_ANSWER_REFRESH.md`.

**Source and merged-main evidence:** PR [#122](https://github.com/dylmarriner/scos-memory/pull/122)
head `9d22b7c04eb777beca118a1a76e30df668996cd3` passed 5/5 CI and
9/9 P4 integration jobs after the two substantive review findings were
reproduced and fixed. Both review threads were resolved and Kilo passed.
The PR was squash-merged using its exact expected head SHA into
`e82978012fe3ad8b7784f2b039aebb8556742dfa`. Independent merged-main
verification on AMD passed 51 Go packages, `go vet ./...`, and both full
tagged memory-intelligence and retrieval-orchestrator PostgreSQL suites.
Merged-main [CI](https://github.com/dylmarriner/scos-memory/actions/runs/40630189957)
passed 5/5 and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40630189981)
passed 9/9. The disposable database included migration 000086 and the
restricted runtime role. Local Python without the database connection
passed 94 tests but had six fixture setup errors; GitHub's Python CI
passed. None of this qualifies the live migration, general P7 answer
generation, retrieval integration, or production recovery.

## §22 retrieval/context lifecycle hook coverage (2026-10-28; merged PR #117)

The autonomous control plane already dispatched turn, model, tool and
checkpoint hooks, but its actual memory retrieval and context-optimisation
stages skipped the four §22.6 boundaries `on_before_retrieval`,
`on_after_retrieval`, `on_before_context_compile`, and
`on_after_context_compile`. An isolated real-HTTP execution regression
recorded calls to the extension-manager dispatch endpoint and the actual
retrieval/context service paths. Before the change it failed with the missing
hook events (RED), and a second regression demonstrated that a failed
pre-retrieval hook did not stop the upstream retrieval call (RED).

The source now dispatches those hooks in stage order. Pre-hook failure
prevents the corresponding upstream call, while post-hook failure degrades
the stage and does not release retrieved evidence to downstream context
compilation. Post-hooks carry the actual stage status, including upstream
failure or invalid/unvalidated compiled capsules, without copying sensitive
retrieval/context content into extension payloads. An invalid-capsule
regression also reproduced a falsely successful post-hook status before
validation was moved ahead of dispatch (RED/GREEN). These are source-level
stage and HTTP integration tests; they do not prove installed extension
execution, full §22.6 hook coverage (including turn-received, code retrieval,
agent/session and memory mutation hooks), fleet-wide hook telemetry,
production deployment or governance §41 completion.

PR [#117](https://github.com/dylmarriner/scos-memory/pull/117) merged with
expected head `a300ef086320a8c226e07045fcb29f797f0a4639` into
`main` commit `eb47ae4a4bdf76962442329c86c329420e7c0329`. The PR-head
CI/P4 checks passed, and Codex, CodeRabbit and Kilo reported no actionable
review findings; Claude review was unavailable without extra credits.
Independent verification on the exact merged-main SHA passed 51 Go packages,
`go vet`, three complete race-enabled ACP package repetitions and 90 Python
regressions. Merged-main P4 integration
[run 40616073909](https://github.com/dylmarriner/scos-memory/actions/runs/40616073909)
passed 9/9 jobs. Merged-main CI
[run 40616074156](https://github.com/dylmarriner/scos-memory/actions/runs/40616074156)
passed 5/5 jobs on the exact merged-main SHA.
The governance §41 DoD checker still fails with PARTIAL; no production
rollout, live migration or paid-resource activation was performed.

## P5 memory-event history TRUNCATE guard (2026-10-28; merged PR #106)

Migration `000082` protects the append-only memory-state ledger and its purge
audit against row-level UPDATE/DELETE, but PostgreSQL `TRUNCATE` does not fire
row-level DELETE triggers. Real PostgreSQL 17 tests demonstrated that both
history tables could be truncated by their migration owner without the
reasoned, counted purge workflow (RED). New migration `000083` installs
statement-level `BEFORE TRUNCATE` rejection triggers on both tables, so
accidental direct or `TRUNCATE tenants CASCADE` attempts fail with SQLSTATE `55006` (GREEN,
five repeated runs). Existing privileged per-tenant audited deletion is not
removed. A fresh disposable database with 83 migrations and restricted-role
policies passed the full retrieval integration package, 51-package Go suite,
90 Python tests, vet and focused race tests. This does
**not** implement historical system-time retrieval, prove production recovery,
or close §41. PR #106 merged with expected head
`b2fdfbaea119b4fd3226cdbf0462297080af3253` into main
`f0d0b8b39260dedf744293de9775e246298b53e9`. The exact merged-main
SHA passed [CI](https://github.com/dylmarriner/scos-memory/actions/runs/40588180860)
(5/5 jobs) and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40588180972)
(9/9 jobs); independent merged-main Go tests passed 51 packages, vet and
90 Python regressions passed. No live migration or deployment occurred. See
`docs/P5_MEMORY_SYSTEM_EVENTS.md`.

## ACP governed downstream error isolation (2026-10-28, merged PR #104)

The autonomous control plane previously forwarded untrusted downstream HTTP
error bodies and raw transport errors into durable lifecycle stage details and
tool-error hook payloads. An actual HTTP regression reproduced a downstream
database connection string in the returned error and a service URL username in
a transport failure (RED). The outbound client now preserves upstream HTTP
status without exposing response bodies and preserves context cancellation
semantics without forwarding credential-bearing transport URLs. Short error
bodies are drained to retain connection reuse. A CodeRabbit review found
that cancellation during response-body reads was being disguised as HTTP
status or a generic read error. A synchronized real HTTP response-body
regression failed for both success and error status paths (RED), then passed
20 focused repetitions and three race-enabled repetitions after preserving
context cancellation from both reads (GREEN). This protects the ACP ingress
and tool-governance boundary, but does not establish governance section 41
completion, historical system-time retrieval, or production activation.
PR #104 merged into main at `e35c4fda8f7c93ad06d43b610b0bfdc7cb4a5721`;
merged-main CI (5 jobs) and P4 integration (9 jobs) passed. This does not
prove a live rollout or recovery qualification.

## P4 retrieval resource-budget enforcement (2026-10-28, development)

Authenticated `POST /v1/retrieve` accepted arbitrarily large `max_results`
values, multiplying PostgreSQL candidate work across retrieval lanes and
potentially overwhelming the optional code service and reranker. It also
accepted unbounded `token_budget` values. A real authenticated HTTP/mux
regression now requires HTTP 400 **before any database access** for negative
or oversized limits. `max_results` is capped at 100 and `token_budget` at
32768; zero retains the documented defaults of 20 and 4000. Boundary tests
cover the accepted maxima and defaults. These limits are deliberately explicit
rather than silently truncating caller requests, so clients can correct them.
This is P4 resource-exhaustion hardening, **not** representative P0 performance
acceptance, P5 historical-system-time support, governance §41 closure or
production rollout. PR review, CI and merged-main qualification remain separate.

## P5 code-intelligence degradation diagnostic isolation (2026-10-28, development)

The authenticated retrieval orchestrator's optional code-intelligence lane
previously copied up to 4096 bytes of an upstream HTTP error body into its
returned error and logged the complete error. A real HTTP 503 response carrying
a PostgreSQL connection URI leaked the internal role, database and host into
application logs (RED). The client now preserves the numeric upstream HTTP
status without copying untrusted response bodies, while boundedly discarding up to
4096 bytes to preserve connection reuse for short errors; the orchestrator logs
only the Go error type. The real HTTP regression exercises both the client
and authenticated retrieval handler, with a real pgx connection-failure path
rather than a simulated database. The regression also checks that transport
errors containing credentials in a request URL do not enter logs. Mutating
type-only logging back to raw error logging reproduced the transport URL
leak (RED), and restoring the fix passed (GREEN). Five focused test
repetitions, three race-enabled repetitions, the full uncached Go suite,
`go vet ./...`, and seven DoD/P6 evidence regressions passed on AMD.
A follow-up review found the initial change disabled HTTP keep-alive on
short errors: a real two-request HTTP regression failed with two connections
(RED). Bounded draining restored one reused connection (GREEN), and the
status-only error assertion was strengthened to reject *any* leaked body text.
Local Python consolidation tests could not run because the host Python 3.14
environment lacks `pydantic`; GitHub CI must independently qualify the
complete Python suite. The §41 DoD checker still correctly FAILS on PARTIAL.
This is a bounded P5
information-disclosure correction, **not** completion of the historical
system-time ledger, governance §41, or the P0–P8 acceptance gates. Source
merge, merged-main CI, and production rollout remain distinct gates. No live
migration, deployment, or paid resources were used.

## P5 namespace lookup failure response isolation (2026-10-28, development)

The authenticated `POST /v1/retrieve` namespace-authorization path previously
returned raw PostgreSQL driver errors as HTTP 500 bodies. A real pgx connection
failure exposed the internal database username, database name, host and port,
and dial error to a service-token caller (RED). The handler now returns a
stable `retrieval temporarily unavailable` HTTP 500 while logging only the
error type, not the connection string (GREEN). The regression exercises the
real authenticated mux and a real pgx pool with an unreachable loopback
endpoint, not a simulated database error; five focused runs passed. A
follow-up CodeRabbit nitpick was addressed by capturing the real standard
logger in the regression: mutating `%T` back to `%v` exposed the internal DSN
(RED), and restoring type-only logging passed five race-enabled runs (GREEN).
The full uncached Go suite, `go vet ./...`, and Python unittest suite passed
on AMD before the follow-up test-only change.

This closes **one P5 error-disclosure path**, not the historical system-time
ledger or §41 governance gate. The runtime audit still correctly FAILS on
historical retrieval HTTP 501, and the DoD checker still FAILS on §41 PARTIAL.
No production rollout, migration, or paid resources were used. PR and
merged-main CI evidence are separate acceptance requirements.

## P5 single-document retrieval request integrity (2026-10-28, development)

`POST /v1/retrieve` previously accepted a valid first JSON request while
silently discarding a second JSON object, `null`, or malformed trailing bytes.
The real handler regression reproduced three HTTP 501 responses instead of
HTTP 400 for those invalid requests (RED); the fix requires EOF after the
first decoded request (GREEN). Ordinary single-object requests and trailing
whitespace still reach the deliberate historical system-time HTTP 501 gate.
The authenticated static-token/mux path is covered too. Five repeated focused
runs, the full uncached Go suite (51 passing packages) and `go vet ./...`
passed on AMD.

The governance-runtime regression previously **incorrectly asserted that no
HTTP 501 exists**, despite the explicit P5 historical belief-ledger refusal.
Its test now expects the one known retrieval-orchestrator finding; all five
runtime-audit unit tests and four definition-of-done evidence tests pass.
The actual runtime audit still correctly reports **FAIL (one HTTP 501)** and
the §41 definition-of-done gate still correctly reports **FAIL (PARTIAL)**.
This is bounded request parsing and evidence-integrity hardening, **not**
bitemporal retrieval implementation, §41 closure, live deployment, or P0–P8
acceptance. Merge and merged-main CI evidence must be recorded separately.

## P5 authenticated identity consistency (2026-10-28, source candidate)

The static service-token fallback in retrieval previously accepted an
authenticated gateway tenant/principal header pair while silently ignoring
conflicting `tenant_id` or `principal_id` JSON selectors. This contradicted
the signed-claims path and the documented identity contract. A real
authenticated HTTP middleware regression reproduced three accepted
mismatches (204 instead of 403) before the fix. The handler now rejects
any conflicting non-empty body identity with HTTP 403, before database
access; matching and omitted body identities still resolve to the trusted
gateway headers. Focused middleware/HTTP regressions passed five repeats;
the full uncached Go suite, `go vet ./...` and diff checks passed on AMD.
This is a bounded P5 tenant-identity consistency fix, **not** evidence of
historical system-time retrieval, full P5 acceptance or live deployment.
PR review, exact-head CI and merged-main verification are separate gates.

## P6 merged-source isolation checkpoint

**Source merged (PR #92 and PR #93), checked 2026-10-28:** Merged `main`
contains `database/migrations/000079_code_provenance_force_rls.sql`
(four provenance tables) and `000080_derived_code_force_rls.sql`
(five derived-source/graph tables). Both enable and FORCE tenant RLS;
restricted-role PostgreSQL qualification is required in the P4 CI workflow.
The merged-main CI and P4 workflows passed on commit
`8e13cf60f9a85cbe6f64be41fca8c2a1af9764eb` on 2026-10-27.
**Live production deployment is not established** for either migration:
merging source and passing disposable-database tests do not apply policies
to the live database. Startup recovery/maintenance qualification, governance
§41, server-attested provenance, representative performance and P0–P8
acceptance remain open. Dated pre-merge development notes below describe
historical checkpoints, not the current merge state.


> **Live/merge evidence update, 2026-10-25 NZ:** PR #29 merged on 2026-09-10. PR #48 (backup recovery, memory-history redaction, model/runtime and Docker CVE fixes) and PR #44 (x/crypto 0.58.0) are merged; latest main CI passed on commits `5d8d969` and `f840bea`. The Kubuntux SCOS backup manager was repaired after its SSHFS bind mount became stale; a new logical backup, physical backup, scratch PITR recovery, recovered Bitwarden decryption key, and OKF Git mirror all verified through SCOS's live APIs. The live backup plan reached **12/12** passing checks at 2026-10-24 22:03 UTC. **Release verification update (2026-10-25):** Kubuntux now runs the newly built shared image at source SHA `a697a7e6e4fd9aaf310a8b49daec9aa7252968e2` across **22 healthy Go services**, and the separate model-runtime and changed consolidation-worker images were rebuilt and deployed. The other workers, PostgreSQL and GPU BGE services were unchanged and remained running. All 30 project containers were running with no unhealthy healthchecked container in the post-rollout scan. The active image before rollout was saved as `scos-fabric:rollback-4f9e643` and its data was retained; the release worktree lives at `/run/media/kubuntux/backup-dev-ext4/scos/scos-memory-release`. Original developer checkout/branch was left untouched. The operator corrected a stale Docker bind to the SSHFS off-host backups directory, added mount-aware startup/rebind scripts with backups, preserved existing model-runtime TLS volume driver options, and passed real logical and physical backup, integrity, PITR, Bitwarden secret-recovery, and OKF Git mirror operations. The SCOS backup plan API reported **12/12 passing** after the rollout; restore-test cadence reported no overdue tests. **This is live deployment and backup evidence, not proof of full P0–P8 completion.** P0 quality baselines and P5–P8 acceptance remain open. The current documentation-only commit may post-date the deployed build SHA.

## Goal

Build SCOS into a governed persistent-memory + code-intelligence substrate for AI agents that can answer:

- what do we know?
- why do we believe it?
- what evidence supports it?
- what code implements it?
- when was it true?
- what changed?
- what becomes stale or affected if something changes?

The target is one evidence system connecting durable memory, immutable code snapshots, retrieval, temporal history, graph structure, agent execution and curated OKF knowledge without collapsing their authority boundaries.

---

# Current production and model baseline

The live deployment remains the Linode host documented in `docs/runbooks/production-access.md`.

```text
Internet / agent
      │ HTTPS + public SCOS API key
      ▼
    Nginx
      ▼
cloud-control-plane
      │ validates customer key
      │ issues short-lived internal claims
      ▼
 mcp-gateway / governed services
      │
      ├── PostgreSQL + pgvector
      ├── OKF Git store
      ├── workers
      ├── DeepSeek-backed reasoning/synthesis paths
      └── internal model-runtime
               ├── BGE-M3
               ├── BGE-reranker-v2-m3
               └── BGE-Code-v1
```

## Model responsibilities

DeepSeek is already part of SCOS and remains the generative/reasoning layer. BGE is specialist retrieval infrastructure and does not replace DeepSeek.

```text
DeepSeek
├── reasoning
├── extraction
├── summarisation
├── synthesis
├── consolidation
├── OKF judgement
└── agent/code reasoning

BGE specialist retrieval layer
├── BGE-M3
│   └── memory/document dense embeddings
├── BGE-reranker-v2-m3
│   └── query-aware candidate reranking
└── BGE-Code-v1
    └── semantic code retrieval
```

Operational rules:

- Production SCOS runs on the Linode host, not the Raspberry Pi.
- Nginx remains the public ingress.
- Customer `scos_live_...` credentials terminate at `cloud-control-plane`.
- Internal model services do not receive customer API keys.
- `model-runtime` and raw inference backends do not receive public Nginx routes.
- BGE activation remains explicit so an ordinary deploy cannot unexpectedly cold-load large models.
- DeepSeek remains independent from the BGE retrieval runtime.
- PostgreSQL and OKF authority boundaries remain unchanged by model choice.

---

# Non-negotiable architecture rules

1. **PostgreSQL is authoritative** for operational history, memory, code snapshots, evidence links, retrieval records and governance state.
2. **OKF Git is authoritative** for reviewed curated knowledge. Model output never silently rewrites approved knowledge.
3. **Derived indexes are rebuildable.** Embeddings, FTS projections, graph scores, standing answers and caches are projections, not authority.
4. **Embedding spaces are explicit.** Role, provider, model, revision, dimensions and generation identify a vector space.
5. **No cross-model vector comparisons.** BGE-small, BGE-M3, BGE-Code, BGE-VL or later spaces never mix in one distance operation.
6. **Raw evidence survives learned ranking.** Reranking may reorder candidates but cannot overwrite raw similarity, source rank, RRF contribution, provenance or governance scores.
7. **Failure degrades honestly.** An unavailable embedder/reranker contributes zero semantic signal; SCOS never fabricates semantic candidates.
8. **Tenant/RLS scope is mandatory.** Every new tenant-owned table and query path preserves tenant/principal isolation.
9. **Code evidence is versioned.** Historical correctness is tied to immutable snapshot/symbol versions.
10. **Time is first-class.** Real-world validity time and SCOS belief/system time remain separate.
11. **Benchmarks gate superiority claims.** New models/rankers/planners are measured against frozen baselines.
12. **Branches are not completion.** Work is complete only when merged to `main` and verified there, or explicitly abandoned.

---

# Phase status

| Phase | Objective | Current status |
| --- | --- | --- |
| **P0** | Benchmark and evidence baseline | **PARTIAL; P4 smoke corpus added** |
| **P1** | Immutable snapshots + durable code store | **SUBSTANTIALLY COMPLETE** |
| **P2** | SCIP + AST / Tree-sitter extraction | **SUBSTANTIALLY COMPLETE** |
| **P3** | Incremental Git-aware indexing | **SUBSTANTIALLY COMPLETE; hardening remains** |
| **P4** | Hybrid memory + code retrieval | **MERGED TO MAIN (PR #29 on 2026-09-10); frozen P0/live performance qualification still pending** |
| **P5** | Temporal evidence | **PARTIAL** |
| **P6** | Unified memory + code graph | **PARTIAL** |
| **P7** | Stale-memory revalidation | **PARTIAL: revalidation, standing-answer invalidation, read-time view and authenticated read-only HTTP serving merged (PRs #64, #111, #112, #114); governed writer, retrieval integration and acceptance remain** |
| **P8** | Impact intelligence | **CODE IMPACT + GOVERNED MEMORY LINKS PARTIAL; FULL CROSS-DOMAIN ACCEPTANCE OPEN** |

---

## P0 exponent-overflow evidence regression (2026-10-27)

Two subprocess regression tests (four cases) first reproduced acceptance of
valid-JSON exponent-overflow numbers (`1e999` and `-1e999`) in otherwise
unused nested fields of the frozen fixture and recorded baseline. Python's
JSON parser converts these to non-finite floats unless `parse_float` is
constrained. The shared P0 lexical smoke evidence decoder now rejects the
non-finite results before baseline recording or verification. The generator
SHA-256 in the checked-in measured baseline was refreshed; rankings, labels,
quality metrics and zero forbidden exposure were not changed. This is only
**offline P0/P4 evidence integrity**. Realistic live retrieval, model-space,
latency, tenant isolation and production acceptance remain unverified.

## P0 frozen benchmark evidence integrity (2026-10-27)

The isolated lexical smoke quality gate now rejects duplicate JSON keys at
any nesting level in the fixture and measured baseline, including conflicting
relevance/forbidden labels and retrieved evidence IDs. It also rejects
non-standard NaN/Infinity JSON constants. Three new regression tests with
four RED-before-fix failure cases became GREEN; the original 7-case measured
rankings, metrics and zero forbidden exposure were preserved while the
generator digest was requalified. This strengthens **only** offline smoke
evidence integrity. Representative live P0/P4 retrieval, tenant and temporal
isolation, latency, model-space qualification and production gates remain open.

## Definition-of-done evidence pointer correction (2026-10-27)

The §41 checker previously named two **nonexistent** test functions and reported
missing code evidence despite existing real integration tests. It now points to
`TestPGIncrementalSnapshotEquivalentToCleanRebuild` (live PostgreSQL clean
rebuild vs incremental symbol/chunk/edge equivalence) and
`TestBackupAndDrillsAgainstPostgreSQL` (integration-tagged encrypted backup,
real archive inspection, key-table and full-schema restore drills). A
regression suite checks both pointers and that absent evidence still fails.
Three disposable PostgreSQL 17 code-index tests passed against all 95
migration/policy SQL files. The backup integration test was also executed
inside an isolated **TLS-enabled** PostgreSQL 17 container after applying all
95 SQL files: encrypted backup integrity passed (1,045 archive entries and
113 fingerprinted tables), monthly restore matched 12 key tables, and
quarterly full restore matched all 113 tables plus the schema catalog.
The test correctly reported that WAL archiving was off. The disposable
container was stopped. This is a **test evidence correction**, not a new
index-rebuild capability, production backup recovery, off-host redundancy,
or PITR validation.
The DoD gate still fails on §41 governance status, and the wider P0–P8
acceptance gates remain open.

## P3 export JSON strictness and CI regression coverage (2026-10-27)

Two real-Git bundle tests exposed ten RED-before-fix cases: the offline
manifest/event verifier accepted nonstandard `NaN`, `Infinity`, and
`-Infinity` constants and exponent-overflow numbers (`1e999`, `-1e999`)
hidden inside otherwise unused nested export fields.
The shared strict JSON decoder now rejects all five, including unknown
fields, while retaining recursive duplicate-key rejection. The same decoder
is used by standalone manifest and event verification. CI now explicitly
runs the manifest, event and joint-bundle Python suites alongside the Git
worklist suite. This is **offline evidence integrity only**, not server-side
Git provenance attestation, P3 scaling acceptance, or production readiness.

# P0 — Benchmark and Evidence Baseline

**Objective:** make every retrieval/indexing/model change measurable.

## Already present

- `packages/benchmark-harness/`
- `docs/adr/0010-superiority-benchmark-methodology.md`
- code-intelligence baseline fixtures and integration tests
- audit artifacts separated from current implementation truth
- `benchmarks/p4/retrieval_fixture.json`
- `benchmarks/p4/test_fixture.py`
- `benchmarks/p4/README.md`
- `benchmarks/p4/offline_gate.py`, `lexical_baseline.json`, and CI regression tests (isolated lexical smoke only; not full P0 qualification)

The P4 fixture is deliberately a deterministic smoke/regression corpus. It is not presented as proof that one model stack is superior.

**P0 offline quality-gate evidence (2026-10-26, review branch):**
An isolated runner executes the existing real deterministic lexical retriever
against all seven frozen P4 fixture queries, records ranked IDs, per-case
Recall@5/10/20, Precision@5/10/20, MRR, nDCG@5/10/20, and forbidden-at-5
exposure, and compares against a checked-in measured baseline. Six initial
subprocess/integration tests were RED before implementation and GREEN after.
CI now fails closed on corpus drift, missing cases, quality regression and
increased forbidden exposure; the baseline records fixture digest, engine,
version, checked-out base Git SHA, generator digest and deployment class. This is *only* an offline lexical smoke
regression gate. It is not BGE, FTS, RRF, tenant-isolation or live P0 quality
qualification. Full/live model baselines, realistic corpora, latency/cost,
and independent approval remain outstanding. Do not count this as P0 complete.

**P0 forbidden-evidence hardening (2026-10-26; PR #57):**
Three new RED-before-fix regression tests demonstrated that the lexical gate
could accept a nonzero forbidden-at-5 baseline, accept an equally contaminated
current result after rebaselining, and record a contaminated baseline outright.
The gate now rejects nonzero forbidden exposure in both current and baseline
results and refuses unsafe baseline recording; the measured seven-case corpus
remains zero-exposure. This protects only explicitly forbidden IDs in the
isolated smoke fixture, not tenant authorization, full temporal filtering,
realistic corpora or live retrieval. Review, merge and deployment gates remain.

**P0 measured-baseline integrity regression (2026-10-26; PR #57 review branch):**
Five new subprocess tests reproduced (RED) acceptance of corrupted baseline
evidence: lowered per-case scores, altered ranked IDs, out-of-corpus IDs,
altered aggregate quality and altered forbidden totals. The offline gate now
recomputes stored metrics from frozen relevance labels and the recorded ranking,
validates document IDs and duplicates, and verifies aggregate/forbidden totals
before any quality comparison. The five cases pass (GREEN), with measured
rankings, retrieval metrics, and forbidden exposure unchanged. This is only
integrity checking of the seven-case lexical smoke baseline; realistic P0
benchmarks, model comparisons, tenant isolation and live qualification remain
unverified. No production changes or authorization to merge/deploy.

**P0 forbidden-depth regression (2026-10-26; PR #57 review branch):**
The isolated lexical smoke gate previously checked forbidden evidence only in
the top five despite measuring retrieval at k=10 and k=20. A synthetic
six-document fixture demonstrated RED: a forbidden document ranked sixth and
baseline recording incorrectly succeeded. Four new tests cover rank-six
exposure, measured counts at k=5/10/20, contaminated rebaselining, and
inconsistent baseline counts. The gate now records and validates per-case and
aggregate forbidden exposure at all three depths, refuses to record any
nonzero exposure, and rejects contaminated current or baseline results.
Existing seven-case rankings and quality metrics are unchanged; the baseline
adds the two deeper-depth counters and refreshes generator provenance.
This remains isolated lexical smoke evidence, not live tenant/temporal
authorization, P0 qualification, or owner authorization to merge/deploy.


**P0 deterministic ranking-integrity regression (2026-10-26; PR #57 review branch):**
Two new regression tests reproduced (RED) a baseline-integrity gap: swapping
non-relevant retrieved IDs left all recorded quality metrics unchanged and
passed the gate, including when the changed baseline was loaded from disk.
The gate now requires exact ranked-ID equality for every deterministic lexical
case (GREEN). An intentional engine/ranking change therefore requires an
explicitly reviewed new baseline; no automatic rebaselining occurs. The
fixture, measured rankings, metrics and forbidden counts are unchanged;
only generator/base-commit provenance is refreshed. This remains a seven-case
offline lexical smoke check, not a deployed retrieval, tenant-isolation,
performance, or P0 completion claim. No merge or deployment authorized.

**P0 shared-harness provenance verification (2026-10-26; PR #57 review branch):**
Three RED-before-fix regression tests demonstrated that the lexical benchmark
omitted its imported `benchmarks/harness.py` implementation from provenance,
and accepted missing/forged harness digests when rankings were unchanged.
The measured baseline now records the shared-harness SHA-256, and the gate
rejects missing or mismatched fingerprints (GREEN). Existing seven-case
rankings, quality metrics, and forbidden-at-5/10/20 totals are unchanged.
This closes a narrow offline reproducibility gap only; deployed BGE/FTS/RRF,
latency/cost, tenant/temporal correctness, independent approval and deployment
gates remain outstanding. No merge or deployment authorized.

## Remaining

- [ ] Expand the versioned memory-retrieval corpus for paraphrase, long-history noise, conflicts, supersession, temporal queries and scope isolation.
- [ ] Expand code retrieval with real repositories, natural-language intent, cross-file relations, rename continuity and graph navigation.
- [ ] Add historical impact cases from real commits.
- [ ] Freeze FTS-only, vector-only, RRF, lexical-code, structural-code and current ChangeImpact baselines.
- [ ] Record Recall@5/10/20, Precision@K, MRR, nDCG@K, p50/p95 latency, candidate counts and inference cost.
- [ ] Attach git SHA, schema version, model/revision/dimensions/generation, corpus version and deployment class to results.
- [ ] Run before/after P4 quality measurements with actual BGE backends.
- [ ] Add deterministic CI regression thresholds plus separately invoked full/live benchmark runs.

## Completion gate

No superiority/performance claim for P4+ is accepted without reproducible before/after evidence.

---

# P1 — Immutable Snapshots and Durable Code Store

**Objective:** make repository state durable, reproducible and queryable at immutable snapshot boundaries.

## Already present

- `database/migrations/000025_code_intelligence_persistence.sql`
- durable repositories, snapshots, files, symbols, symbol versions and edges
- `parent_snapshot_id` lineage
- transactional snapshot activation
- branch/commit/extractor/configuration snapshot identity
- rename continuity through stable content identity
- durable v2 handlers in code-intelligence

## Remaining hardening

- [ ] Return explicit snapshot identity/provenance from every historical code query/impact surface.
- [ ] Add explicit snapshot selection where callers still imply the active branch snapshot.
- [ ] Verify retention/GC cannot remove code evidence referenced by memory-code links or audit records.
- [x] Add interrupted-index/activation recovery tests around real PostgreSQL transactions (including OS-process termination during file indexing, snapshot activation and historical reactivation; see evidence below).

**P1/P5/P8 historical impact selection (2026-10-26; review branch, not merged):**
The governed `POST /v2/code/impact` and `scos_code_impact` MCP surfaces
now accept `as_of_commit` or `snapshot_id` with an explicit `branch`.
The selected immutable generation is returned as `snapshot_id`, `commit_id`,
`branch` and `historical`. A shared bounded impact traversal rejects
cross-generation caller edges. Historical lookup rejects queued/failed,
cross-repository/tenant and mismatched-branch snapshots, and requires
`snapshot_id` when a commit has multiple extractor generations.
The historical impact still joins **currently visible and valid** governed
memory facts, explicitly labelled `knowledge_scope=current_visible_facts`;
this is not historical memory as-of-time filtering. A real PostgreSQL
integration test covers superseded-versus-current caller graphs, forged
cross-generation edges, memory namespace authorization, ambiguous commits,
invalid selection and queued generations. A separate RED→GREEN PostgreSQL
test proves that path queries matching at least 100 symbols fail closed
(HTTP 422) rather than returning a silently truncated impact graph. A gateway
forwarding test checks historical selectors and authenticated identity.
**P8 impact depth-budget regression (2026-10-26; PR #58 review branch):**
A real PostgreSQL regression reproduced an active impact request accepting
`max_depth=9` and returning HTTP 200 despite the API lacking a hard traversal
limit. Both current and historical impact now reject depth above 8 with
HTTP 422, and store entry points reject excessive depth even without HTTP.
The test checks the 8-hop boundary remains accepted.

**P8 impact node-budget regression (2026-10-26; PR #58 review branch):**
A real PostgreSQL test first reproduced current and historical one-hop impact
queries returning a 521-node graph without a total-node budget (RED). The
datastore now caps each snapshot-scoped caller query at 513 distinct rows and
fails closed when the full traversal exceeds 512 unique nodes, including seed
matches and cumulative two-hop expansion. HTTP returns 422 instead of a
silently partial blast radius. The test covers current and historical impact,
wide seed sets, one-hop and two-hop overflow, HTTP 422, and the exact 512-node
success boundary. Ten consecutive PostgreSQL regression runs, the uncached
full Go suite, and go vet passed locally. This closes the unbounded node-count
gap, **not** edge-density/SQL-work budgets, graph completeness, or cross-domain
impact qualification. Review, merge, deployed acceptance and recovery gates
remain outstanding.

**P8 impact edge-budget regression (2026-10-26; PR #58 review branch):**
A PostgreSQL 17 integration fixture with 65/66 visited nodes and 4096/4225
real CALLS edges demonstrated that depth and node budgets alone still allow
dense graphs to overwhelm the PageRank adjacency load (RED). Ranking adjacency
now uses SQL LIMIT 4097 and rejects any result exceeding 4096 CALLS edges
without returning a partial graph (GREEN). Current and immutable historical
queries both enforce the limit; HTTP returns 422. Exact-boundary and overflow
fixtures assert their persisted edge counts and verify the behavior. This
closes the unbounded ranking-edge transfer gap, **not** database query-work
budgets, latency/scale qualification, or full P8 acceptance. No production
deployment is authorized by this evidence.

**Not P1/P5/P8 completion:**
other active-only APIs, temporal memory semantics, retention/GC acceptance, production
deployment/recovery, independent review and merge remain open.

**P1 recovery hardening evidence (2026-10-25; merged via PR #55):**A new real-PostgreSQL regression suite exercises atomic rollback when indexing
fails after an earlier file insert, verifies that no partial file/symbol rows
or active-pointer changes survive, and retries the same immutable key after
explicitly marking/deleting the failed generation. It also covers startup
cleanup of aged queued generations without touching active state, refuses
reactivation of queued/failed/nonexistent snapshots, preserves idempotence
for already-active branches, and requires the workspace scan API to return
HTTP 202 (not a misleading HTTP 200 `unchanged=true`) for queued/indexing
snapshots. Both faulty responses were observed RED before implementation.
These are isolated integration results, **not** proof of process-kill,
concurrent-index, deployed recovery or production rollback readiness; the
P1 acceptance gate remains open pending those checks and review/merge.

**Additional P1 ownership/immutability hardening (2026-10-25; merged via PR #55):**
Real PostgreSQL 17 tests reproduced four independent failures before fixes:
`IndexSnapshot` accepted a different tenant's repository ID and persisted
foreign-owned file/symbol versions under the target snapshot; an already-active
snapshot could be indexed again with an empty file list, corrupting its stored
file count; a failed snapshot could be indexed and promoted to active; and
`CreateSnapshot` accepted a tenant ID that did not own its repository.
`IndexSnapshot` now locks and verifies the snapshot's repository and pending
status before any writes, and `CreateSnapshot` derives the tenant from an
ownership-constrained repository lookup. Additional concurrent-writer tests
exercise two index attempts on the same generation against real PostgreSQL.
These checks protect one-generation ownership and immutability but **do not**
establish cross-generation activation serialization, forced process termination,
full recovery, production readiness or completion of the P1 gate.

**P1 late-worker status race regression (2026-10-25; merged via PR #55):**
CodeRabbit identified a race where a late indexing/diff error could overwrite a
superseded, reactivatable generation as failed. A real PostgreSQL 17 test first
reproduced this corruption (RED), then passed (GREEN) after constraining
`SetSnapshotStatus` to pending states only. A second regression confirms
queued generations can still transition to failed for recovery. All 63
migrations applied to an isolated disposable database. This does not establish
process-kill or production recovery acceptance.

**P1 cross-generation activation race (2026-10-25; merged via PR #55):**
A deterministic real-PostgreSQL 17 integration test held the shared repository
row lock until two independent same-branch indexing transactions were
simultaneously blocked in `activate_code_snapshot`. Before the fix, both
committed as `active` (RED), violating the immutable snapshot/branch
activation invariant. Append-only migration
`database/migrations/000066_serialize_code_snapshot_activation.sql`
locks the shared repository row *before* superseding old generations, so
concurrent activations serialize and observe the previous commit (GREEN).
The test verifies exactly one active snapshot, pointer consistency, and
historical reactivation of the superseded generation. Migration number 000065
was introduced by PR #52, which merged on 2026-10-25; 000066 remains
distinct. This evidence covers cross-generation activation concurrency only; forced process termination,
full release/recovery drills, independent review, merge, and remaining P1
hardening are still required.

**P1 activation lock-order review regression (2026-10-25; merged via PR #55):**
A subsequent CodeRabbit major finding identified a repository/snapshot lock
inversion: the migration acquired the snapshot lock before the repository
lock, and file indexing, SCIP indexing and historical reactivation did the
same. A deterministic PostgreSQL 17 test held the repository row while
each of those three real application paths waited, then attempted to lock
the target snapshot. All three failed RED with PostgreSQL deadlock SQLSTATE
`40P01`. The migration now reads immutable repository ownership without a
snapshot lock, locks the repository first, and then revalidates the snapshot
under its row lock. The three Go paths share this same lock order before
any snapshot writes. All three contention scenarios now pass GREEN.
The existing cross-generation activation test was updated to observe the
earlier repository-lock wait as well as the activation-function wait.
A separate real-PostgreSQL SCIP regression verifies that foreign repository
IDs cannot inject symbols and active SCIP generations cannot be reindexed.
These tests do not substitute for forced process-termination, release/recovery
drills, independent approval, merge, or production deployment validation.

**P1 actual process-termination recovery regression (2026-10-25; merged via PR #55):**
The new `TestPGSnapshotProcessTerminationRollbackAndRecovery` launches the real
Go `IndexSnapshot` path in a separate operating-system process against disposable
PostgreSQL 17. A test-owned symbol row lock deterministically pauses the worker
*after* it has inserted the first file in its uncommitted transaction; the
parent confirms the exact blocked PostgreSQL backend via `pg_blocking_pids`,
then forcibly kills the worker. The test verifies that neither file nor symbol
versions from the killed transaction persist, the previously active snapshot
remains active, stale-generation startup recovery marks the queued generation
failed, and a replacement generation can index and activate both files. This
is a real process kill and database rollback, not a simulated indexing error.
It does **not** prove interruption during the activation function itself,
production restart behavior, restore/rollback drills, or complete P1 acceptance.

**P1 activation-boundary SIGKILL acceptance (2026-10-26; isolated AMD review branch):**
Two additional real PostgreSQL 17 integration tests launch the actual Go
indexing or historical-reactivation path in a separate OS process. A
test-owned row lock on the *previously active* generation forces the worker
to wait **inside** `activate_code_snapshot`, after the indexer has written
its files and marked the new generation ready within its uncommitted
transaction. `pg_stat_activity` and `pg_blocking_pids` identify the exact
blocked backend and owning lock before the worker is forcibly killed.
The tests verify that no partial versions, ready/active status, or repository
pointer changes survive the killed transaction; the historical reactivation
case verifies that neither old nor current immutable generation changes.
Startup stale-generation recovery, replacement indexing and a clean
historical reactivation retry are exercised. Both tests passed 20 consecutive
runs against disposable PostgreSQL 17. This completes the *isolated
PostgreSQL interruption-test* checklist item, **not** P1 release acceptance.
No live restart/rollback or production deployment is implied.

**Merge/CI reconciliation (verified 2026-10-26):** PRs #52, #53, #54 and
#55 are merged on GitHub, and `main` at `9969f8d` passed CI and P4 Hybrid
Retrieval on 2026-10-25 (runs 40187523312 and 40187523364). Earlier
paragraphs' merge/review caveats record the limitations at the time each
test was first introduced, not the present PR state. Independent approval
for this new test-only change and a separately owner-authorized deployment
remain outstanding. P1 provenance, retention/GC, deployed recovery and
the P0–P8 acceptance gates remain open.

---

# P2 — SCIP + AST / Tree-sitter Extraction

**Objective:** extract high-confidence language-aware symbols and structural relationships.

## Already present

- Go-native extraction
- SCIP ingestion
- Tree-sitter fallback extraction
- symbol doc comments
- extraction confidence classes
- conservative called-name scope handling
- call/reference/implementation edge generation
- ambiguity avoidance rather than invented edges

## Remaining

- [x] Publish a language × extractor-tier capability matrix (2026-10-26): `docs/P2_EXTRACTOR_CAPABILITIES.md` and machine-readable JSON enumerate source-index tiers, recognized extensions, confidence classes, SCIP separation, and JSX/TSX limitations. Runtime contract tests exercise all five source parsers and every advertised extension; workspace-scan integration covers previously missing .cjs/.mts/.cts. This is source-parser qualification, not semantic/type-resolution or production proof.
**P2 regression evidence (2026-10-26, development branch):** A RED runtime-matrix test found `.cjs`, `.mts`, and `.cts` were accepted by the offline Git worklist but not recognized by workspace scan. The runtime detector now recognizes them; GREEN tests cover the real workspace scan and Tree-sitter extraction. A real PostgreSQL 17 integration test with all 71 migrations verifies persisted function/extractor/confidence evidence, and the complete code-intelligence PostgreSQL suite passed. The published matrix explicitly distinguishes SCIP imports, AST, syntax, and file-only evidence. Pending independent review/merge and merged-main CI.

- [ ] Improve type-resolved references where syntax-only Tree-sitter confidence is insufficient.
- [ ] Maintain deterministic fixture repositories per supported language.
- [ ] Keep authoritative SCIP facts distinct from heuristic extraction.

---

# P3 — Incremental Git-Aware Indexing

**P3 event-sidecar precondition consistency (2026-10-27, isolated development worktree):** Four real-Git RED-before-fix regression assertions showed `--events-only` accepted an explicit empty parent as an initial snapshot, accepted identical parent/target commits, and ignored invalid repository UUIDs and blank branches. Both index-payload and event-sidecar modes now share identity and strict ancestry validation (including the graft-checked Git reader); the sidecar includes caller-declared repository/branch IDs for local correlation. The 81 Python Git verification fixtures pass. This is offline input integrity only, **not** server-attested Git provenance, representative P3 throughput, production rollout or P0–P8 acceptance.


**Objective:** update code intelligence proportionally to changed content while preserving immutable history.

## Already present

`pgstore.go` already:

- records parent snapshots;
- compares parent/target file content hashes;
- skips re-parsing unchanged files;
- copies unchanged declarations and chunks forward;
- activates snapshots transactionally;
- preserves identity through unchanged-content renames.

## Remaining

- [x] Git-aware **offline parent→target commit payload driver**: `scripts/scos_git_worklist.py` enumerates immutable target Git trees, transfers source for safe extractor fallback, retains the entire target manifest for deletions, enforces the 4 MiB API bound, and verifies Git ancestry. `docs/P3_GIT_WORKLIST.md` records preconditions and limitations; local Git fixtures, backend evidence-chunk equivalence and deleted-symbol integration tests pass. This is not yet a remote uploader, typed persisted Git events, or large-repository streaming.
- [x] **P3 guarded delta reuse (2026-10-26, merged PR #68):** the Git worklist now explicitly marks omitted unchanged source (`reuse_parent`) and supplies `parent_commit_id`; a `--full-source` retry mode preserves safe extractor-upgrade behavior from merged PR #67. The server rejects source-omitting requests unless the exact expected commit is the active compatible extractor/configuration parent and each reused file matches path/hash/language and has persisted source evidence. PostgreSQL 17 RED/GREEN regression tests cover incompatible extractor/configuration, missing path, changed hash/language, wrong commit and metadata-only parent; failed deltas preserve active snapshots and roll back partial file versions. HTTP 400/409 acceptance and valid reuse are verified. P3 streaming and typed event gates remain open.
- [x] **P3 PR #68 source-less-file review correction (2026-10-26, merged):** exact zero-byte SHA-256 is the only source-less file permitted for guarded parent reuse. Unchanged nonempty ASCII/Unicode whitespace files under the request limit are sent with source rather than falsely requesting chunk reuse. Eight real-Git Python fixtures pass, including Unicode whitespace, and a PostgreSQL 17 integration test on all 70 migrations verifies empty-file acceptance, whitespace conflict, and unchanged active snapshot on rejection. Merged-main CI and P4 Hybrid Retrieval passed on b23a219 (runs 40264072198 and 40264072197); no production deployment evidence.
- [ ] Explicit add/modify/delete/rename/copy event metadata, including verified Git provenance and agent-facing access.
  - [x] **P3 trusted export-identity and duplicate-key hardening (2026-10-27, review branch):** the offline joint snapshot verifier now optionally binds both authenticated exports to an independently trusted SCOS repository UUID and branch, rejecting consistently forged identities across both exports. Both export readers reject duplicate JSON object keys recursively instead of silently accepting the last value. Eight new real-Git regression cases were RED before implementation and GREEN afterward (including positive trusted binding, malformed UUID, missing paired flags, nested event duplicates); the full Python Git verifier suite passes. This is **offline validation only**, not signed server-attested provenance, deployment, large-repository acceptance or P0–P8 completion. See `docs/P3_SNAPSHOT_BUNDLE_VERIFICATION.md`.
  - [x] **P3 legacy Git graft ancestry rejection (2026-10-27, development):** discovered that `.git/info/grafts` can rewrite Git parent ancestry even when replacement objects are disabled. Two real-Git RED-before-fix tests showed a non-ancestor parent accepted as trusted by both the indexing worklist and the joint snapshot verifier. The shared Git reader now fails closed on nonempty graft files resolved through `git rev-parse --git-path` (including linked worktrees); a third regression checks legitimate linked worktrees and harmless empty graft files. Full Python Git suite passes. This prevents a **local trust-boundary bypass**, but does not attest server Git ingestion, establish P3 performance acceptance, or authorize production rollout.
  - [x] **P3 Git replacement-ref integrity hardening (2026-10-27, development):** all offline Git worklist, manifest, event and joint-bundle verification subprocesses now explicitly disable Git replacement objects. A local `refs/replace/*` entry previously allowed a forged tree to be read under the original trusted commit SHA, causing the joint verifier to accept fabricated source hashes and the worklist to emit forged bytes. Two real-Git regression fixtures were reproduced RED before the fix and pass GREEN afterward. This protects local Git object identity but **does not** attest server indexing, close the P3 provenance gate, or prove production readiness.
  - [x] Joint offline snapshot manifest/event bundle consistency (2026-10-27 development): `scripts/scos_verify_snapshot_bundle.py` checks both independently authenticated complete exports against the **same** snapshot/repository/branch/target/strict-parent binding, actual Git blob SHA-256 bytes and exact event transition coverage. Eleven real-Git regression fixtures cover valid multi-page exports, cross-export repository/branch/parent drift, forged hash/evidence, missing events, truncated pages, strict ancestry and initial snapshots; see `docs/P3_SNAPSHOT_BUNDLE_VERIFICATION.md`. **No server-attested provenance or production deployment claim.**
  - [x] Optional trusted parent binding for P3 stored-source manifest verifier (2026-10-27 development): `--parent` checks the server-recorded parent commit against a locally resolved trusted Git object and verifies parent ancestry. Four real-Git RED/GREEN fixtures cover valid ancestry, forged/missing parent metadata, unrelated histories, identical parent/target and empty revision. Optional offline consistency hardening only, **not** server-side attestation or a P3 completion claim.
  - [x] P3 authenticated stored-source manifest readback and independent Git-byte comparison (2026-10-27 development): `POST /v2/code/snapshots/manifest` and `scos_code_snapshot_manifest` expose tenant-scoped, paginated, immutable file paths, languages, stored SHA-256 hashes and snapshot commit labels. `scripts/scos_verify_snapshot_manifest.py` compares a complete authenticated export with streaming hashes of the actual committed Git blobs and rejects forged content, missing/extra paths, incomplete pages and identity drift. Migration 000076 adds an online C-collated pagination index. RED-before-implementation Go HTTP/gateway tests and Python fixtures, including a restricted PostgreSQL role, validate the slice; see `docs/P3_SNAPSHOT_MANIFEST_READBACK.md`. **This is independent readback verification, not server-attested Git provenance, signed evidence, or proof that parsers consumed those bytes.** Production rollout and recovery gates remain open.
  - [x] Authenticated server-recorded commit binding for event pages (2026-10-26 development): event responses include repository, branch, target commit and parent commit from persisted snapshots; the offline Git verifier rejects mismatched/missing IDs and cross-page repository/branch drift. PostgreSQL 17 tenant-isolated API tests, restricted-role HTTP test, uncached full Go suite, go vet and 34 real-Git Python worklist/verifier fixtures pass on a disposable 74-migration database. The commit IDs are index-caller claims, **not** server-attested Git tree/source hashes, so this does not close the P3 provenance gate.
  - [x] P3 offline Git-vs-ledger consistency verifier (development, 2026-10-26): `scripts/scos_verify_snapshot_events.py` checks complete authenticated event exports against committed parent/target trees, exact path coverage, immutable Git blob identities for rename/copy provenance, evidence classes and cursor completeness. Fourteen real-Git fixtures include missing deletion, forged/ambiguous origin, changed copy source, unchanged modify, duplicate path, pagination, snapshot mismatch, and an ambient GIT_DIR/GIT_WORK_TREE repository-redirection regression. This does **not** prove the server snapshot UUID is bound to the supplied Git commits; server-attested provenance remains open.
  - [x] P3 authenticated event retrieval (merged PR #72, 2026-10-26): `POST /v2/code/snapshots/events` and `scos_code_snapshot_events` MCP tool expose immutable tenant-scoped server-derived events with bounded keyset pagination, provenance and historical access. RED/GREEN HTTP/DB and gateway tests verify identity, pagination, missing/incomplete snapshots, and delete/modify evidence; review hardening adds transaction-local tenant RLS enforcement and C-collated cursor index (migration 000073). No production deployment; Git sidecar reconciliation remains open.
  - [x] P3 durable server-derived event ledger (migration 000072): native snapshot indexing atomically persists five conservative file-change classes with tenant RLS; regression covers rename/copy ambiguity, failed-index rollback and restricted-role isolation. See docs/P3_SNAPSHOT_CHANGE_LEDGER.md. Trusted Git-event reconciliation remains open; the authenticated retrieval API is covered by the separate P3 item above.
  - [x] Offline Git event sidecar via `scripts/scos_git_worklist.py --events-only` (2026-10-26 development branch): deterministic add/modify/delete/rename/copy classification from immutable Git trees, with a real Git rename/delete/add regression. This is **not** server-side durable persistence or the authenticated index API contract; those gates remain open.
- [x] Verify deleted symbols cannot leak into active target snapshots (2026-10-26, PostgreSQL 17 full-suite evidence): `TestPGGitRemovedSymbolAbsentFromTarget` verifies removed declarations are absent from target snapshot rows while historical rows remain; `TestPGDeletedFileRemovesItsSymbols` verifies active path/FQN queries no longer return deleted paths; `TestPGDenseQueryUsesActiveSnapshotOnly` rejects superseded embedding hits. This does not substitute for production query-path monitoring.
- [ ] Reduce unnecessary whole-snapshot structural recomputation where dependency boundaries permit it.
  - [x] P3 dependency-bounded native CALLS reuse (merged PR #78, 2026-10-26): on a changed manifest, copy only verified unaffected function/method scopes and re-resolve changed scopes against the full target declaration set. Qualified callers are conservatively invalidated; resolver-version mismatch, non-native edges, cross-scope copied edges and changed declaration identities fail closed to full resolution. PostgreSQL 17 RED/GREEN tests verify independent/qualified callers, deletion isolation, duplicate-content stable identity, legacy/non-native rejection and 128-file four-package clean-rebuild equivalence (93 copied edges, 31 freshly resolved, 1 parsed and 127 reused files). Repeated 128/1024-file single-change benchmarks show no defensible wall-clock speedup (1024-file median 3296 ms partial vs 3239 ms full); the acceptance performance gate stays open. No production rollout.
  - [x] P3 identical-manifest CALLS graph reuse (merged PR #77, 2026-10-26): an unchanged complete source manifest now copies compatible parent call edges transactionally rather than running the whole-snapshot call resolver. Exact declaration identity and graph-provenance guards fail closed to full resolution on duplicate-content symbol reassignment or non-native parent edges. Migration 000075 records copied/resolved call-edge counts and resolver-version provenance, rejecting pre-upgrade graphs. Four PostgreSQL 17 integration regressions (two observed RED/GREEN), full disposable-database Go suite and 128/1024-file repeated benchmarks pass. This does **not** optimize changed-content structural dependencies, remove immutable snapshot writes, or close P3 performance acceptance.
  - [x] Qualified-call candidate resolution indexed by function name and directory suffix (development, 2026-10-26): deterministic ambiguity-preserving regression and 3,000 same-name-function synthetic benchmark show ~253.6 ms prior vs ~4.0 ms indexed pure resolution on AMD; seven disposable PostgreSQL 17 call-edge/equivalence integration regressions pass; no claim of end-to-end or dependency-bounded speedup. See `docs/P3_INCREMENTAL_SCALING.md`.
  - [x] P3 bounded batched call-edge persistence (development, 2026-10-26): pure ambiguity-preserving call resolution plus <=200-row parameterized inserts, reducing per-edge SQL round trips while preserving transactional activation. PostgreSQL 17 cross-batch regression at 202 files, full integration suite and 32/128/256/1024 synthetic Go scaling measurements documented in `docs/P3_INCREMENTAL_SCALING.md`. Whole-snapshot structural recomputation and representative acceptance remain open.
  - [x] P3 indexed bulk unchanged-declaration copy (development, 2026-10-26): copy persisted declaration versions/chunks per unchanged file in bulk, add online `code_symbol_versions(file_version_id)` index (migration 000074). Real PostgreSQL regression validates 120-file one-change metrics, 119 reused files and 119 call edges. This removes per-declaration SQL round trips but **does not** yet avoid whole-snapshot structural recomputation.
- [x] Persist per-snapshot parsed/reused/unparsed file counts, parsed/reused declaration counts, elapsed indexing time, extractor-tier counts and cache-hit ratio (migration 000068, verified PostgreSQL 17 full suite 2026-10-26). `TestPGIndexSnapshotPersistsAtomicWorkTelemetry` verifies values and atomic rollback; these are persisted counters, not yet operational dashboards.
- [x] **P3 merged-main source and CI verification (2026-10-26):** PR #77 merged at 05e377a179da58104dacf8f7ac043fb8365cd279 and PR #78 merged at ba7ea5770f13acca59d170a5f2d950359b9aa4f6. Both merges passed main CI and P4 Hybrid Retrieval. On ba7ea57, an isolated AMD detached main worktree passed the full uncached Go suite against disposable PostgreSQL 17 with all 74 migration files, go vet, and 29 Python Git-worklist/reconciliation fixtures. Production migration, rollout and recovery validation remain unperformed.
- [ ] Benchmark incremental indexing against clean rebuild equivalence and changed-content scaling. Reproducible PostgreSQL 17 benchmarks now compare clean and one-changed-file snapshots at 32/128/256 files (three single-iteration runs; `docs/P3_INCREMENTAL_SCALING.md`). The 128-file incremental case remains slower than clean in the optimized fixture; representative 1k–10k+ multi-language throughput, resource budgets and acceptance thresholds remain unmeasured. Small-fixture symbol/chunk/edge equivalence and 120-file changed-content reuse are verified.

**Review hardening evidence (2026-10-25, PR #54, pending review/merge):**
The snapshot delta now distinguishes unchanged-source copies from unambiguous
renames and preserves same-path modifications when file hashes collide.
A PostgreSQL 17 regression additionally covers SCIP-imported snapshots:
first document ingestion, unchanged re-index, changed declaration documentation,
and moved document paths. SCIP's persisted symbol hashes represent symbol
identity rather than source bytes, so SCIP-only deltas use a deterministic
per-document declaration fingerprint and report moves conservatively as
additions/removals, not proven file renames. The regression was observed RED
before the fix and GREEN against a disposable, fully migrated database.
This closes a review finding, **not** the remaining P3 Git-diff driver,
telemetry, equivalence, or scaling acceptance gates.

---

# P4 — Hybrid Memory + Code Retrieval

**Objective:** combine the merged model runtime and durable retrieval foundations into one correct, explainable hybrid retrieval system.

## P4.1 — Model-runtime integration

**Status: MERGED via PR #28. Production activation/pinning remains operational work.**

- [x] BGE-M3 text endpoint.
- [x] BGE reranker endpoint.
- [x] BGE-Code endpoint.
- [x] Internal TLS/auth boundary.
- [x] Hosted Compose model overlay.
- [x] Explicit `SCOS_MODEL_RUNTIME_ENABLED` activation gate.
- [x] Focused runtime/worker/Compose CI definition.
- [x] Strict required-backend flag parsing.
- [x] TEI model/revision propagation.
- [ ] Pin validated production model revisions instead of mutable `main`.
- [ ] Measure live Linode memory/CPU headroom before enabling simultaneous profiles.
- [ ] Verify live model activation on the Linode after explicit rollout.

## P4.2 — Formal embedding spaces

**Status: IMPLEMENTED ON PR #29.**

- [x] Shared identity: role + provider + model + revision + dimensions + generation.
- [x] `memory_embeddings.embedding_space_id` with legacy backfill.
- [x] Generation-safe `(memory_version_id, embedding_space_id, chunk_number)` projection identity.
- [x] Worker resolves/creates the active space and retains historical spaces.
- [x] Memory vector SQL resolves and filters the exact active text space before pgvector distance operations.
- [x] Durable `code_embeddings` use the same space registry.
- [x] Code dense queries filter active snapshot + exact code space.
- [x] Mixed-space and superseded-snapshot regression coverage on real PostgreSQL/pgvector.

## P4.3 — Honest semantic degradation

**Status: IMPLEMENTED ON PR #29.**

- [x] Removed fake `vectorFallback` semantic behavior.
- [x] Query-embedding failure yields zero semantic candidates.
- [x] Vector-search failure yields zero semantic candidates.
- [x] Exact/FTS/relation/session retrieval continues independently.
- [x] Degraded model/source state is recorded rather than disguised as zero relevance.
- [x] Tests protect against relabelling unrelated high-confidence memories as semantic matches.

## P4.4 — Query-aware BGE reranking

**Status: IMPLEMENTED ON PR #29.**

- [x] Retrieval-orchestrator calls governed `model-runtime /v1/rerank`.
- [x] RRF + dedup create a bounded candidate window.
- [x] Query + candidate text are sent to the reranker.
- [x] Raw lexical score is preserved.
- [x] Raw cosine similarity is preserved.
- [x] RRF score is separate.
- [x] Reranker score is separate.
- [x] Authority/confidence governance weighting is applied after relevance reranking.
- [x] Reranker failure explicitly falls back to RRF + governance.
- [x] Model/revision/planner/degraded metadata is persisted with retrieval runs.
- [x] The previous SemanticScore/RRF overwrite bug is removed.

## P4.5 — Dense code retrieval with BGE-Code

**Status: IMPLEMENTED ON PR #29.**

- [x] Durable code embeddings reference immutable `code_symbol_versions` and explicit embedding spaces.
- [x] `code_embeddings` has tenant RLS + FORCE RLS from creation.
- [x] Deterministic embedding documents contain FQN/path/kind/signature/docs/useful persisted source content.
- [x] Content hashes support idempotent re-embedding.
- [x] New/changed active snapshots can backfill BGE-Code embeddings without invalidating the durable lexical snapshot when the model is unavailable.
- [x] Exact path/FQN remains a precision anchor and bypasses learned ranking.
- [x] Non-exact queries combine lexical + BGE-Code dense retrieval.
- [x] Persisted structural/SCIP edges participate as an independent retrieval signal.
- [x] Bounded personalized PageRank contributes graph/centrality signal.
- [x] Lexical, dense, structural/graph and fused score provenance remain distinct.
- [x] Active-snapshot, mixed-space and tenant-isolation behavior is integration-tested with PostgreSQL/pgvector.

## P4.6 — Deterministic query planning

**Status: IMPLEMENTED ON PR #29.**

Planner classes:

```text
exact/navigation
memory
code
mixed
temporal
```

- [x] Planner controls lane eligibility and candidate budgets.
- [x] Planner contains no tenant/principal/security scope and cannot widen authorization.
- [x] Exact/navigation avoids speculative model fanout.
- [x] Memory mode uses memory-oriented lexical/vector/relation/session lanes.
- [x] Optional durable `repository_id` + `branch` enables the code lane for repo-aware code/mixed requests.
- [x] Code evidence is returned separately from memory sections and selected immutable symbol-version IDs are recorded with the retrieval run.
- [x] Code-intelligence failure degrades only the code lane.
- [x] Standing-answer short-circuiting cannot suppress explicitly requested repo-aware code evidence.
- [x] `scos_retrieve` exposes optional durable repository context.
- [x] `scos_code_query` has a backward-compatible v1/v2 bridge; P4-aware callers use durable repository context while legacy clients remain functional.
- [x] Temporal classification prepares future P5 work without inventing temporal semantics.
- [ ] Compare planner cost/quality against an all-source live baseline under P0 before claiming efficiency gains.

## P4 source verification performed

The implementation has been exercised locally with:

- migration contract tests;
- retrieval gofmt/vet/unit tests;
- code-intelligence gofmt/vet/unit tests;
- all migrations applied from zero to PostgreSQL 17 + pgvector;
- real-PostgreSQL retrieval SQL tests;
- real-PostgreSQL code-intelligence integration tests;
- tenant RLS/FORCE-RLS tests using a non-superuser role;
- active-snapshot and mixed-space dense retrieval tests;
- exact-model-bypass and degraded-model handler tests;
- embeddings-worker tests in an isolated Python environment;
- P0/P4 frozen fixture structural tests;
- merged Compose structural validation.

GitHub Actions has also experienced runner-launch failures where jobs are created with no executed steps. Those are infrastructure failures and are not counted as product test failures or successful verification.

## P4 completion/acceptance gate

- [x] Normal memory vector retrieval cannot compare mixed embedding spaces.
- [x] Embedding outages produce zero semantic candidates instead of fake evidence.
- [x] Raw semantic similarity is preserved independently from RRF/reranker/final scores.
- [x] BGE reranker is consumed with explicit fallback behavior.
- [x] BGE-Code embeddings are durable and tied to immutable symbol versions.
- [x] Natural-language code retrieval fuses lexical, dense, structural/SCIP and graph/PageRank signals.
- [x] Query planning exists and cannot affect authorization boundaries.
- [x] A deterministic P0/P4 smoke corpus exists.
- [ ] Full P0 before/after quality, latency and cost evidence is recorded with actual model backends.
- [x] PR #29 merged to `main` on 2026-09-10; the resulting source was re-read and later CI runs verified it.
- [ ] Live Linode activation/capacity/model-pinning verification is complete before claiming BGE profiles are production-active.

---

# P5 — Temporal Evidence

**Objective:** answer historical and supersession questions correctly across memory and code.

## Already present

- memory validity/observation/record-time fields
- append-only memory versions and correction/supersession history
- code snapshots tied to branch + commit
- parent snapshot lineage
- retrieval-run history

## Remaining

- [ ] Formalize valid time vs SCOS belief/system time.
- [ ] Add explicit system-time invalidation/supersession timestamps where missing.
- [x] `as_of_valid_time` filters current-system-belief memory validity (HTTP and PostgreSQL integration tests; **not** historical approval/status reconstruction).
- [ ] `as_of_system_time` memory filter requires a trustworthy historical belief ledger; the endpoint deliberately returns HTTP 501.
- [x] **P5 forward-only event inspection (2026-10-28, PR #119 merged):** authenticated `GET /v1/memory-state-events` exposes bounded, keyset-paginated, tenant/principal-FORCE-RLS-scoped memory-state transitions without memory content. A RED/GREEN regression rejects malformed selectors before database access; a real restricted-role PostgreSQL HTTP test covers insertion/update events, pagination, tenant/nonmember denial, membership revocation and session cleanup. Event ID is **not** commit order, and observed time is **not** commit time. See `docs/P5_MEMORY_STATE_EVENT_READBACK.md`. PR #119 head `dbeb058942d247ba65d5502f08957fd93b2bea3c` passed exact-head CI/P4 and Kilo review with no findings; Codex reported no findings on its initial commit, while CodeRabbit/Claude reviews were unavailable due to usage limits. It was squash-merged at `9ef2b438a9be833f725e7fac75aa82fda3c5e1cd`. Merged-main [CI](https://github.com/dylmarriner/scos-memory/actions/runs/40619890844) (5/5) and [P4 integration](https://github.com/dylmarriner/scos-memory/actions/runs/40619890968) (9/9) passed. Independent AMD `go test ./...` and `go vet ./...` passed. The local system Python lacks pydantic, so the full Python suite was not independently rerun; GitHub CI's Python job passed. Production qualification remains open. The historical system-time 501 guard remains.

- [x] **P5 forward-only memory-state transition evidence (2026-10-28, PR #103 merged into main):** migration `000082_memory_system_events.sql` records a migration-time baseline plus transactional INSERT/UPDATE state transitions for memory identity, namespace, status, selected version/supersession, validity and sensitivity. Events contain no memory content, are append-only, FORCE-RLS protected, and use owner-only trigger insertion. A write-conflicting migration lock closes the baseline/trigger race; a privileged reasoned purge with durable audit is required before tenant deletion. Real PostgreSQL regressions check restricted-role tenant isolation, event forgery/mutation denial, transactional status changes, unrelated metadata non-events, runtime purge denial, audited tenant offboarding, rollback integrity and connection-context cleanup. A lock-order regression failed before the fix; a PostgreSQL concurrent-insert test confirms the migration lock blocks writes. The review hardening passed 51 Go packages, go vet, 83 Python tests and two focused race-enabled PostgreSQL passes on a disposable database. An additional real PostgreSQL regression confirms migration-owner FORCE-RLS prerequisite policies from 000064/000071; the forged-event test requires a complete row and SQLSTATE 42501, with a temporary permissive-policy mutation producing the expected RED failure. **Not an as-of query:** `observed_at` and transaction IDs do not establish commit order; no earlier-than-migration history exists, and hard-delete/offboarding capture, redaction, approvals and replay semantics still require design and recovery qualification. The HTTP `as_of_system_time` 501 gate remains.
- [ ] Add explicit `as_of_commit` / snapshot selection to code paths still assuming active state.
- [x] **P5 unified historical code lane (2026-10-28 development candidate):** `scos_retrieve` and `POST /v1/retrieve` accept `as_of_commit` and/or `snapshot_id` with explicit repository/branch. The planner must run the code lane even for memory-classified queries, and forwards selectors to the existing immutable snapshot readback rather than the active index. Historical code responses are rejected unless the requested commit/snapshot, branch, and every symbol-version's provenance match; 404 and ambiguous 409 remain safe and actionable, while service/provenance failures fail closed rather than silently returning current code. The selected code symbol versions, immutable selectors, repository and branch are persisted in `retrieval_runs`. Real PostgreSQL 17/pgvector and downstream HTTP tests validate the integration; see `docs/P5_UNIFIED_HISTORICAL_CODE_READBACK.md`. **This does not reconstruct historical memory belief state, close the P5 acceptance gate, attest Git bytes, or establish production deployment.**
- [x] **P5 historical repository-provenance binding (2026-10-28, source candidate):** a RED-before-fix cross-service regression showed historical `scos_retrieve` accepted missing or foreign `repository_id` response metadata even when the requested repository differed. The code service now returns repository identity from the tenant-scoped immutable snapshot lookup; the orchestrator verifies the requested UUID identity for every historical result, including empty results, and fails closed on older/untrusted downstream responses. Codex identified that PostgreSQL canonical UUID text differs in case from valid caller UUIDs; a second RED/GREEN regression now verifies parsed UUID equivalence while rejecting foreign UUIDs. Real PostgreSQL 17 integration and authenticated HTTP regressions cover the contract. This does **not** attest the Git tree, implement historical memory belief reconstruction, prove live rollout, or close P5 acceptance. See `docs/P5_UNIFIED_HISTORICAL_CODE_READBACK.md`.
- [ ] Ensure memory↔code evidence references concrete historical symbol versions.
- [ ] Add temporal contradiction/supersession tests and P0 cases.
- [x] **P5 conflict endpoint-visibility regression (2026-10-28, merged PR #95):** reproduced cross-namespace/tenant target-UUID disclosure in contradiction readback and source-only `memory_relations` RLS on disposable PostgreSQL 17. Added two-endpoint read/write RLS policy upgrade (migration `000081`), per-lane opposite-endpoint tenant/namespace/request-and-namespace-sensitivity/status/valid-time filtering, owner-safe memory-intelligence GET/reflection filtering, and PostgreSQL regressions across seven retrieval lanes, public HTTP, owner-role and restricted-role SQL reads/writes and connection cleanup. Merged source (commit `d01427945762acf07ce7ea39c08de88ee53a7103`) passed P4 merged-main CI; historical belief semantics and production rollout remain separate gates. See `docs/P5_CONFLICT_VISIBILITY.md`.

**P5 review evidence (2026-10-26, PR #60, unmerged):** Corrected the
successful empty-result path in `POST /v1/retrieve` so principals with no
readable namespaces still receive a durable retrieval-run audit with query hash,
identity, `as_of_valid_time`, and `temporal_scope`. The pre-fix real PostgreSQL
integration test failed with an empty `retrieval_run_id`; after the fix it
passes and checks the stored `retrieval_runs.filters` and empty evidence.
Verified on isolated AMD using disposable PostgreSQL 17/pgvector with 65
migrations: full uncached Go suite (51 packages), `go vet` for the retrieval
service, ten repeated focused integration runs, focused race detector and
diff validation passed. The migration-owner integration test did not assert
restricted-role behavior on this specific empty-result branch. Independent
review, merge, main-branch CI, historical system-time semantics and deployment
verification remain open; this evidence is **not** full P5 acceptance.

**P5 restricted-role acceptance expansion (2026-10-26, PR #60, unmerged):**
Added a real PostgreSQL integration test for the empty valid-time HTTP path
using the non-superuser, non-BYPASSRLS `scos_app` runtime role. It checks
successful audit creation, persisted tenant/principal/valid-time/scope, own
principal visibility, cross-principal and cross-tenant denial, and absence of
session-context leakage after connection reuse. The P4 PostgreSQL CI job now
applies the repository RLS policies after all migrations, provisions the
restricted role, and **requires** its DSN so the acceptance test cannot silently
skip. A RED test with the required role DSN absent failed as intended; the
GREEN test passed against an isolated PostgreSQL 17/pgvector instance after
65 migrations and 20 policies. This is branch-local test evidence; CI and
independent review of the new commit remain required, as do system-time history,
temporal contradiction/supersession cases, merge, and deployment verification.

---

# P6 — Unified Memory + Code Graph

**Objective:** operationalize the existing bridge between governed memory and immutable code evidence.

## Already present

`database/migrations/000033_memory_code_links.sql` provides the durable bridge from `memory_items` to immutable `code_symbol_versions` with link type, source and confidence.

P4 additionally allows repo-aware retrieval to return code evidence beside memory evidence without collapsing the two authority domains.

## Partial P6 evidence: bounded memory → code readback (2026-10-27)

The memory-intelligence service now supports tenant/namespace-RLS-scoped,
keyset-paginated `GET /v1/evidence-links` for an accessible fact (max 100 links
per page). Every returned edge identifies the concrete immutable symbol
version, repository, snapshot, recorded commit label, path, relationship,
source and confidence. The governed `scos_memory_code_links` MCP tool exposes
this readback without inventing transitive edges. RED-before-fix HTTP and
MCP tests and real PostgreSQL 17/pgvector integration tests cover pagination,
invalid selectors, zero links, tenant/namespace denial and provenance. The
CI PostgreSQL job now exercises memory-intelligence integration tests.
A follow-up TDD integrity gate found two real cross-repository symbol-version
failures (foreign symbol accepted at link creation; drifted foreign symbol
returned by readback). Both failed on the prior branch and passed after
repository-chain validation in the writer, reader and SQL link trigger
(migration 000077). The direct-SQL guard is tested for SQLSTATE 23514. A separate RED→GREEN
regression also removed raw SQL-driver error disclosure from the existing
evidence-link POST handler's HTTP 500 responses.
A further real-PostgreSQL RED→GREEN regression (migration 000078) binds each
snapshot's `tenant_id` to its repository using a validated composite foreign
key, including protection against later snapshot/repository tenant updates.
The migration was also replayed against deliberately inconsistent disposable
legacy data: validation failed closed, then succeeded after test-only cleanup,
and reapplying it remained idempotent. This guards one previously accepted
cross-tenant provenance forgery but does
not enable RLS or establish complete symbol-version chain consistency.
A restricted-role direct-SQL check on disposable PostgreSQL 17 proved the
outstanding exposure: `scos_app` with no tenant GUC read 439 repository rows
across 430 test tenants; all four provenance tables still had RLS disabled.
At that pre-migration checkpoint, the P6 PR was blocked on this security finding even though the narrower
snapshot FK migration and HTTP endpoint tests pass.
**Historical isolation gate (before PR #92 merged):** legacy code repository, snapshot, symbol and symbol-version
tables lacked independent forced RLS; code-indexer/runtime tenant-context
changes and real-PostgreSQL qualification are required before enabling it. A first TDD-scoped
code-indexer increment (2026-10-27) moves repository registration, snapshot
creation and pending/active snapshot discovery into transaction-local tenant
context and rejects malformed tenant
UUIDs. A disposable PostgreSQL 17 test with isolated forced-RLS shadow tables
and the non-BYPASSRLS runtime role reproduced a SQLSTATE 42501 denial before
the change and passed afterward, including foreign-tenant and connection-leak
checks. A second scoped increment moves all five HTTP repository-ownership
reads and the exact snapshot-key lookup into tenant-local transactions,
with RED→GREEN forced-RLS regression for own/foreign tenants, invalid
identity and connection-context isolation. Fresh disposable PostgreSQL
17/pgvector (97 migration/policy scripts) passed the scoped test and the
full code-intelligence integration package; uncached Go and vet passed.
A third TDD increment scopes immutable historical snapshot resolution and
symbol-version readback to **one transaction**. With FORCE RLS on all four
isolated code-provenance shadow tables, the baseline failed RED with
`historical snapshot not found` for the authorized tenant; the scoped
implementation passed GREEN, denied the foreign tenant and left zero
symbol/symbol-version rows visible without context. On a separate disposable
PostgreSQL 17/pgvector instance with 97 migration/policy files, the complete
code-intelligence integration package, uncached `go test ./...`, and
`go vet ./...` passed. No production policies were changed.
A fourth RED→GREEN increment adds tenant-scoped active-snapshot exact
path/FQN and lexical symbol queries and wires both HTTP query paths to them.
The real PostgreSQL shadow-schema test exercises all four forced-RLS
provenance tables, authorized exact/lexical hits, foreign-tenant empty results
and no context-free symbol/version visibility. Legacy owner-level query
helpers remain for existing callers/tests; impact, indexing and recovery
paths were not yet qualified for the then-planned four-table policy migration.
A fifth RED→GREEN increment scopes the snapshot-change-event API's
snapshot-identity lookup and event readback to the same tenant-local
transaction. The restricted-role, four-table forced-RLS shadow-schema
regression first returned 404 for an authorized snapshot, then passed with
own-tenant HTTP 200 and foreign-tenant HTTP 404. Fresh disposable PostgreSQL
17/pgvector applied 97 migration/policy scripts; full code-intelligence
integration, uncached Go tests, vet and diff checks passed. This is
preparatory evidence, not a production RLS migration or a P6 completion gate.
A sixth TDD increment scopes snapshot status transitions, failed-snapshot
cleanup and idempotent snapshot-manifest comparison in the scan/index paths. The regression was RED
before the new method existed and GREEN with restricted `scos_app` against
forced-RLS shadow tables: an authorized transition succeeds, a foreign
tenant cannot update or delete the snapshot, malformed tenant IDs fail
closed, and pooled connections retain no tenant context. A separate real-schema integration
test verifies the production `code_snapshot_status` enum, actual indexed
file manifest, foreign-tenant denial and empty-manifest mismatch with the
restricted role. A disposable PostgreSQL 17/pgvector database applied
97 migration/policy scripts; the complete code-intelligence integration
package, uncached Go suite, vet and diff checks passed. This does not
scope indexing, activation, deletion, recovery or impact traversal.
A seventh RED→GREEN TDD increment scopes snapshot reactivation in both
scan and synchronous index-retry paths: `ReactivateSnapshotScoped` validates
ownership and runs locking/activation within one transaction-local tenant
context. The restricted `scos_app` forced-RLS shadow-schema test proves
own-tenant active retry, foreign-tenant denial and invalid-ID rejection; a
real-schema integration test covers superseded-generation reactivation and
foreign-tenant denial. The method was absent (compile RED) before implementation.
Disposable PostgreSQL 17/pgvector applied all 97 migration/policy scripts;
code-intelligence integration, uncached full Go tests, vet and diff checks
passed. This does **not** qualify unscoped indexing, recovery or impact paths.
An eighth RED→GREEN increment scopes the snapshot-diff HTTP endpoint and
the scan/indexing response's diff calculation to the authenticated tenant.
Snapshot ownership, parent lineage and both generations' symbol reads now
run on a single transaction-local RLS connection. A real PostgreSQL 17
restricted-role test with forced-RLS shadow tables first returned HTTP 404
for an authorized snapshot; after the fix it returned the expected added
file, preserved a modified-file diff against its parent, denied foreign
and missing snapshots with identical 404 responses, and rejected malformed
snapshot UUIDs and trailing JSON objects. The legacy owner-level diff
helper remains for internal tests only. The full code-intelligence
integration package, uncached Go suite and Go vet passed on a disposable
97-migration/policy PostgreSQL 17/pgvector instance. No production RLS
policy was enabled or live data migrated.
A ninth RED→GREEN increment closes two unscoped snapshot-symbol-count
reads in the synchronous indexing response and idempotent retry path.
The restricted-role forced-RLS regression first failed an authorized retry
with "count snapshot symbols: sql: no rows in result set". Both paths now
use a tenant-scoped transaction, and the regression verifies the authorized
count, foreign-tenant denial and malformed-tenant rejection. No production
policy or live database was changed.
At that intermediate checkpoint this was **preparatory only**: remaining
code-indexer paths and production migrations were unqualified, and PR #92 was
draft. The migration subsequently merged as PR #92; live rollout remains unverified.
**P6/P8 current + historical impact tenant scoping (2026-10-27, PR #92, merged source):** current impact now resolves seed symbols,
traverses reverse-CALLS edges and ranks evidence through one transaction
carrying the authenticated tenant's RLS context. Historical impact
likewise resolves its immutable generation and traverses/ranks within
one transaction, preserving ambiguous-generation and selector guards.
A restricted-role PostgreSQL 17 shadow-schema regression with forced RLS
on five code/edge tables verifies real owner-visible caller evidence,
historical evidence, foreign-tenant denial and invalid selectors. The
new test failed RED at compile time before the scoped API existed, then
passed GREEN; 97 migration/policy scripts and the complete code-indexer
integration suite passed in a disposable database. No production RLS
policies were enabled; file/SCIP indexing is scoped in a later increment,
while recovery and remaining legacy paths remain blockers.

**P6 file/SCIP indexing under tenant-local transactions (2026-10-27,
PR #92, merged source):** production scan and SCIP handlers now
invoke scoped indexers. Snapshot/repository ownership, symbol/version
writes, change-event ledger, status transitions and activation share one
authenticated tenant-local transaction. A real restricted-role PostgreSQL
test first failed RED at compile time, then passed GREEN for actual Go
file and SCIP indexing, foreign-tenant denial and connection-context
cleanup. The existing event-ledger test fixture now uses the same scoped
runtime method. In an isolated PostgreSQL 17 database with all 97
migration/policy scripts, candidate forced-RLS policies on all four
code-provenance tables denied direct context-free reads (0/0/0/0 rows)
and permitted tenant-scoped reads. The opt-in
`SCOS_EXPECT_CODE_PROVENANCE_RLS=1` integration gate and the full
code-intelligence package passed under those **disposable-only**
policies, as did the uncached full Go suite, Go vet and tagged
memory-intelligence PostgreSQL integration tests.
A subsequent RED→GREEN end-to-end regression caught the SCIP handler's
context-free failed-generation cleanup: an authorized retry returned HTTP
500 under forced RLS because the old deletion affected zero rows.
The handler now calls `DeleteFailedSnapshotScoped`; the same restricted-role
retry returns HTTP 200 and a new immutable snapshot.
**P6 dense embedding maintenance tenant scoping (2026-10-27,
PR #92, merged source):** authenticated code-embedding
maintenance now resolves active snapshots, loads symbol documents and
persists missing chunks inside transaction-local tenant context.
RED-before-implementation and GREEN-after-implementation regression
coverage exercises owner visibility, cross-tenant denial, invalid
tenant IDs, idempotent chunk writes and active snapshot selection
under forced RLS with `scos_app`; a separate real-schema restricted-role
integration test covers the same production methods. Disposable
PostgreSQL 17 applied 77 migrations and 20 policies; candidate
four-table FORCE RLS was applied only to that disposable database, and
full code-intelligence plus tagged memory-intelligence integration
suites passed with the forced-RLS qualification gate. The uncached
full Go suite and Go vet passed separately. Production
forced RLS on four legacy provenance tables was not yet merged at that checkpoint;
recovery paths, remaining callers and complete P0–P8 acceptance were still open.

**P6 snapshot list/detail readback tenant scoping (2026-10-27,
PR #92, merged source):** both GET snapshot routes now use the same
transaction-local tenant context as other code provenance reads, with
repository and snapshot tenant predicates. A restricted-role PostgreSQL
17 forced-RLS regression was RED for the authorized list (HTTP 200
empty) before the fix, then GREEN for authorized list/detail,
cross-tenant isolation and sanitized invalid-context errors. The
dedicated disposable database applied 97 migration/policy files;
the complete code-intelligence integration suite, uncached full Go
suite, Go vet and diff checks passed. A second pass on the same
disposable database applied candidate FORCE RLS policies to all four
provenance tables; the opt-in restricted-role qualification, complete
code-intelligence integration package and tagged memory-intelligence
PostgreSQL integration all passed. No production policy activation
or main-branch qualification is implied.

**P4/P6 structural code retrieval scoping (2026-10-27, PR #92, merged source):** the HTTP hybrid query's structural graph lane now
holds the authenticated tenant context through active snapshot selection,
neighbor expansion, edge loading and PageRank. Neighbor versions must
match the resolved snapshot. A restricted-role forced-RLS shadow-schema
regression was RED before the scoped API and GREEN for owner-visible
caller evidence, centrality and foreign-tenant denial. On disposable
PostgreSQL 17 with candidate four-table forced RLS, the complete
code-intelligence suite, uncached Go suite, Go vet and diff checks
passed. Legacy owner-level helper remains for internal test callers.
This does not enable production RLS or close P4/P6 acceptance.

**P6 code settings RLS hardening (2026-10-27, PR #92, merged source):**
The per-principal durable-store toggle was another legacy table with no
row-level security. New policy 000020_code_user_settings_rls.sql enables
and forces tenant isolation for its table. IsUserEnabled and
SetUserEnabled now run SQL under transaction-local authenticated
tenant context, including the common requireEnabled route guard.
Restricted scos_app real-PostgreSQL regressions were RED before the fix
(owner write rejected by candidate FORCE RLS; production catalog lacked
RLS) and GREEN afterward. They prove owner write/read, other-tenant
isolation, no context-free direct reads, invalid UUID rejection, pooled
connection cleanup, and production policy catalog state. An additional
RED-to-GREEN HTTP regression prevents SQL/driver errors leaking through
settings GET/PUT and the route guard. A fresh disposable PostgreSQL 17/pgvector database applied all 98
migration/policy scripts, then passed the full code-intelligence
integration package, uncached Go suite (51 tested packages), Go vet and
diff checks. This is a **source-only policy migration**, not a live
migration or production rollout. The four legacy code-provenance tables now have committed FORCE RLS
source migration 000079 (merged via PR #92), with derived-table migration
000080 merged via PR #93. Live rollout and recovery qualification remain open.

**Merged source, no verified live rollout:** migrations 000079 and 000080
are on `main`, but the live production database has not been verified to
run them. Background recovery and remaining legacy operations require
production rollout/recovery qualification before completion can be claimed.

See `docs/P6_MEMORY_CODE_READBACK.md`. The **one-hop** reverse code→memory evidence page and governed MCP tool are now implemented with principal-scoped RLS and immutable symbol-version provenance (2026-10-28 development). **Partial only:** multi-hop traversal, ranking, inference, server-attested Git provenance and production qualification remain open.

## Remaining

- [ ] Complete governed API/MCP tooling for explicit memory↔symbol-version links.
- [ ] Validate tenant/scope ownership at link creation.
- [ ] Record actor/operation/turn evidence where schema metadata needs expansion.
- [ ] Infer high-confidence links from deterministic signals before model inference.
- [x] Add bounded **one-hop** memory→code and code→memory evidence readback (immutable symbol versions, tenant/namespace RLS, link-UUID cursor, 100-row cap).
- [x] Bound and expose **two-hop** code-version → RLS-visible memory fact → immutable code-version paths through governed `scos_code_memory_paths` (10×10 caps, UUID-keyset pagination, original evidence IDs and provenance). HTTP identity and malformed-upstream regressions pass locally; uppercase/mixed-case UUID evidence identity now has RED/GREEN regression coverage (5 focused repetitions, 2 race repetitions, full uncached Go suite and `go vet`). See `docs/P6_MEMORY_CODE_READBACK.md`.
- [ ] Add general multi-hop graph expansion, impact traversal and measured large-graph budgets.
- [ ] Use graph proximity as a ranking feature, never an authority override.
- [ ] Expose explainable provenance paths.
- [ ] Allow OKF proposals to reference concrete memory/code evidence without mutating approved OKF state automatically.

---

# P7 — Stale-Memory Revalidation

**Objective:** detect when code changes make linked memory/knowledge potentially stale and revalidate without silent rewriting.

## Prerequisites already present

- immutable snapshots
- incremental change detection
- symbol-version lineage
- memory version/supersession system
- `memory_code_links`

## Implemented / review evidence (not final acceptance)

- [x] Emit durable snapshot-activation revalidation events for linked changed/deleted code.
- [x] Deterministic continuity classification with changed/deleted/ambiguous and safe-rename evidence (merged PR #63 additionally guards ambiguous copied successors).
- [x] Governed human review decisions: **merged to main in PR #64 (2026-10-26, merge commit `30d0618`).** The P7 decision slice adds an append-only, one-decision-per-event review record, namespace reviewer authorization (read + promote/administer), a rationale requirement, a restricted-role RLS policy, and a POST decision endpoint. It projects review outcomes separately from the original event status. **This is an assessment record only:** no fact, version, standing answer or OKF knowledge is automatically changed. A future correction/reopen mechanism remains necessary.

**P7 governed review decision TDD (2026-10-26; isolated AMD):**
A real PostgreSQL 17 integration test failed RED with HTTP 405 for the missing review operation. The implementation passed GREEN for authorized decision creation, read-only/outsider denial, invalid outcome/rationale rejection, duplicate-decision conflict, immutable source-event/fact preservation, and restricted `scos_app` role read/write and update denial. Codex review identified a valid Unicode JSON body-limit defect: a 2000-character escaped non-BMP rationale was RED (HTTP 400), then GREEN after increasing the bounded request limit to 32 KiB (enough for 2000 JSON-escaped surrogate pairs). Migration `000070_memory_revalidation_review_decisions.sql` and the role-grant policy protect the append-only evidence. This is branch-local evidence only until CI, independent review and merge.

**P7 review-finding remediation (2026-10-26; historical branch evidence, merged in PR #64):** CodeRabbit correctly identified that append-only review decisions obstruct parent event/tenant deletion. Migration `000071_revalidation_privileged_purge.sql` now adds a migration-owner-only, transactionally audited decision purge and an immutable tenant-independent ledger. The decision trigger rejects ordinary DELETE/UPDATE; `scos_app` cannot call the purge, delete decisions, or read the ledger. Real PostgreSQL tests first failed RED because the purge function was missing, then passed GREEN for restricted-role denial, transaction rollback, exact purge count and immutable audit evidence. `docs/P7_REVALIDATION_PURGE_RUNBOOK.md` documents the explicit-approval operator workflow. **Remaining review gate:** independent assessment of the privilege boundary, CI on the pushed commit, and validation of full tenant-offboarding dependencies outside P7; this function is not a general tenant-delete implementation. Distinct unsupported-outcome, short-rationale and >2000-character rationale cases were also added. No production tenant data was purged.

**P7 draft PR #64 main-sync verification (2026-10-26; isolated AMD):**
Reconciled the draft branch with main (including merged P7 ambiguous-copy guard
and P8 graph impact changes), preserving both roadmap evidence streams.
On a newly provisioned disposable PostgreSQL 17 database, all migrations
000001–000071 and all RLS policies applied successfully. Three repetitions
of focused governed-review/ambiguous-copy PostgreSQL tests passed with
mandatory restricted-role checks; the full uncached Go suite passed
(51 packages), as did `go vet ./...`. An earlier attempt against an older
throwaway database failed the ambiguous-copy test because that database
lacked migration 000069; fresh sequential migrations resolved the fixture
mismatch. This is **review-branch evidence**, not merged-main or production
qualification. PR #64 was draft at this checkpoint. It was subsequently marked ready, passed
CI and P4 workflows on head `59cd28e`, and merged as `30d0618` on
2026-10-26. No production purge or deployment is authorized.

**P7 review-evidence provenance (2026-10-26; isolated AMD review branch):**
After PR #64 merged, a real PostgreSQL 17 regression first failed because the
revalidation GET endpoint exposed reviewer/outcome/rationale but omitted the
immutable review-decision UUID and decision creation time. The change projects
`review_decision_id` and UTC RFC3339Nano `review_decided_at` only for decided
events, preserving the existing event fields and RLS visibility. Integration
checks verify the POST receipt ID matches both owner and restricted `scos_app`
GET responses, the timestamp parses, and undecided events omit both fields.
Five focused PostgreSQL 17 repetitions passed with forced RLS, as did the
full uncached 51-package Go suite, `go vet ./...`, and the focused race test.
This is review provenance, **not** automatic fact/OKF application, decision
correction, production rollout, or P7 completion.

## Remaining

- [x] Emit durable revalidation events when linked symbols change/delete (migration 000064; snapshot activation).
- [x] Resolve continuity into unchanged / renamed / changed / deleted / ambiguous states (unchanged produces no event; ambiguous requires review).
- [x] Use deterministic classification first; see duplicate-copy hardening evidence below.
- [ ] Qualify continuity classification against labelled corpora and remaining edge cases.
- [ ] For changed/ambiguous cases, use existing reasoning models to create a proposal, not an authoritative mutation.
- [ ] Outcomes: still-valid, needs-review, superseded, invalidated, unresolved.
- [ ] Create governed memory versions/supersession relations only when approved.
- [ ] Create OKF review proposals with old/new code evidence.
- [ ] Invalidate standing answers depending on stale/superseded evidence.
  - [x] **Database-level P7 standing-answer invalidation (2026-10-28, migration 000084, merged PR #111):** typed tenant-bound dependencies, conservative stale marking for legacy rows, atomic invalidation on fact status/version/effective-date/deletion and P7 code `needs_review` events, guarded refreshes requiring governed still-valid reviews, runtime privilege denial, and real PostgreSQL restricted-role and concurrent-refresh race tests (RED before the row lock, GREEN after), plus TEMP-table shadowing reproduction (RED before schema-qualification, GREEN after). Exact-head CI and P4 workflows passed; merge commit `6030318ac61e367844fec061688e773e096b71de` passed merged-main CI and P4 workflows. See `docs/P7_STANDING_ANSWER_FRESHNESS.md`.
  - [x] **Read-time freshness projection merged (2026-10-28, migration 000085, PR #112):** PostgreSQL `security_invoker`/`security_barrier` view rechecks all supporting evidence on each read, including wall-clock expiry, tenant/namespace visibility, status and P7 review restrictions. Real PostgreSQL tests first failed because the view was missing (RED), then passed after implementation (GREEN) for live-to-expired transition without cache mutation (including a long-lived transaction RED with `now()`, GREEN with statement time), cross-tenant denial, membership revocation and malformed legacy exclusion. PR #112 merged at `50f2252d242edb8cb795fe6f274322887610f89c`; exact-head and merged-main CI/P4 integration passed, with 51 Go packages, vet and the tagged PostgreSQL suite independently passing on AMD. Production rollout remains unapproved. The parent checkbox stays open because no end-to-end cache writer/serving path, stale-answer benchmark, or full P7 acceptance has been verified.

- [ ] Measure false-positive/false-negative outcomes.

**P7 ambiguous-copy revalidation regression (2026-10-26; PR review branch,
not merged):** A real PostgreSQL 17 integration fixture linked an active
memory fact to a single original file, then replaced that file with two
identical-content copies in a new immutable snapshot. RED: the activation
trigger classified the transition as `renamed:still_valid`, even though
two successors made continuity ambiguous. Migration
`000069_revalidation_ambiguous_copy_guard.sql` adds a new-snapshot
content-duplicate check before treating a stable-symbol reuse as a safe
rename. GREEN: the event is `ambiguous:needs_review`; no memory fact is
rewritten. The focused revalidation tests passed ten consecutive runs
against a disposable migrated database, the full uncached Go suite and
`go vet ./...` passed, and focused race-detector tests passed twice.
This does not complete the human review/proposal lifecycle, standing-answer
invalidation, false-positive measurement, independent review, merge, or
deployment/recovery gates.

---

# P8 — Impact Intelligence

**Objective:** answer “what is affected if this changes?” across code and knowledge.

## Already present

Code-side `ChangeImpact` already provides reverse call traversal, direct/transitive dependents, bounded depth and personalized PageRank. P4 code search now also consumes persisted structural/SCIP graph signal during retrieval.

**P8 explainable reverse-CALLS evidence (2026-10-26; PR #58 review branch, not merged):**
A new real-PostgreSQL regression reproduced the missing multi-hop provenance:
linked memory facts on transitive callers exposed only a direct symbol-to-memory
link, with no proof of the caller chain from the queried seed. The implementation
now publishes `code_evidence_paths` keyed by immutable symbol-version ID and
builds each path from actual snapshot-bounded CALLS edges in reverse traversal
order (matched callee → caller → ...). `KnowledgeDependents[].EvidencePath`
appends the governed memory link to the same concrete chain. Shortest paths
are deterministic across edge-row/map ordering and cycles; missing paths fail
closed, and the existing 512-node/4096-edge caps still apply. Real PostgreSQL
tests cover current and historical code snapshots, 3-hop memory links and HTTP
serialization; unit tests cover tie-breaking, cycles and unreachable symbols.
This is **partial P8 path evidence only**, not P7 revalidation, relation-type
coverage, production qualification, or permission to merge/deploy.

## Remaining

- [ ] Expand impact relationships beyond CALLS where extraction confidence permits: references/types, implementation/inheritance, tests, configuration ownership and generated links.
- [ ] Preserve relation confidence/provenance.
- [ ] Include snapshot/commit provenance in every impact result.
- [ ] Traverse changed symbols through `memory_code_links` into affected facts, decisions, procedures, standing answers and curated-knowledge references.
- [ ] Keep code impact and knowledge impact categories distinct.
- [ ] Return explainable paths for every affected object.
- [ ] Add governed what-if impact planning before edits.
- [ ] Compare predicted blast radius with later edits/test failures/commits and feed results into P0.

---

# Cross-phase supporting work

## §32 backup freshness telemetry (2026-10-28, merged PR #105)

The backup manager already emitted `scos.backup.age` only when a caller
requested `GET /v1/backups`, while `scos.backup.restore_status` was emitted
when a drill was recorded. A healthy service with no API polling therefore
produced no backup-age observations, even when backups became stale. This is
a source-level observability gap, not evidence that backups themselves failed.

An independent periodic reporter now starts when the observability collector
is configured, regardless of whether the optional backup/restore scheduler
is enabled. It emits immediately and every 15 minutes by default (override
with `SCOS_BACKUP_AGE_METRIC_INTERVAL`). The reporter enumerates provider manifests (HMAC-authenticated for
encrypted providers), verifies the newest candidate's actual archive, and falls back
to older candidates if verification fails; it does not decrypt every old
archive on each heartbeat. It emits no fabricated zero-age metric when no
backup verifies, and it stops on context cancellation. The existing
on-demand age gauge and recorded restore-drill outcome metric remain.

TDD evidence: new real HTTP collector regression tests failed to compile
before the reporter existed (RED), then passed after implementation (GREEN).
Tests cover no-client-polling delivery, periodic observations/cancellation,
no-backup and tampered-archive refusal, bounded full-archive reads and
restore-status delivery. This is a **source candidate**, not proof of
production collector delivery, missing-backup alert configuration, recovery
qualification, complete §32 service coverage, or governance §41 completion.
No live backup, restore, production deployment or paid resource was invoked.

## Safety and secret handling

**CI supply-chain gate hardening (2026-10-26; review branch, not merged):**
A successful GitHub CI run on PR #58 still contained a `govulncheck`
exit code 3: the CI runner used Go 1.26.9, affected by a `crypto/x509`
certificate verification vulnerability fixed in Go 1.26.10. The security
step's `continue-on-error` masked that finding. A separate review branch pins
Go 1.26.10 across GitHub workflows and the module toolchain, removes the
scan's false-green override, and adds CI regression tests for these contracts.
The Docker build stage already uses patched Go 1.27.3. This is a pending
security-gate correction, **not** evidence of an authorized production rebuild,
merge or deployment.


- Add write-time secret/PII scanning before conversational facts or broadly promoted OKF material persists sensitive values.
- Keep credentials in security-gate/credential-vault boundaries rather than ordinary memory.

## Standing-answer completion

The standing-answer table and database safeguards exist. A merged read-only
HTTP serving endpoint queries the restricted-role evidence-current
projection, but no answer-generation/refresh writer or retrieval-orchestrator
integration exists. Merged source is not a deployed cache service:

- [x] Typed evidence dependency tracking and transactional invalidation (migration 000084, merged PR #111).
- [x] Restricted-role, read-time evidence/clock freshness projection (migration 000085, merged PR #112; not deployed).
- [ ] Governed writer and refresh generation with a real retrieval/answer pipeline.
- [x] Authorized read-only serving endpoint merged (2026-10-28, PR #114): `GET /v1/standing-answers` uses authenticated tenant/principal RLS, bounded normalized exact keys and the security-invoker current-evidence view. RED/GREEN unit and real PostgreSQL HTTP tests cover tenant denial, membership/fact revocation and clock expiry. Exact-head CI/P4 passed at `5b7d3029dc56c6f3ea0b74ec004578382b794d31`; merged-main CI/P4 passed at `d243e4fff571b2ad848e9eff10649cb4f40db939`, with 51 Go packages, vet and restricted-role P7 PostgreSQL tests independently passing on AMD. Production rollout and retrieval-orchestrator integration remain open.
- [x] Restricted-role P7 HTTP benchmark fixture (source candidate, 2026-10-28): integration-tagged fresh, revoked-fact and clock-expired read cases exercise the actual handler and security-invoker view. A mutation replacing the view with the raw table fails the revocation and expiry regressions. On a disposable PostgreSQL 17/pgvector database on AMD, 20 measured requests per case took approximately 3.0 ms/op fresh, 1.6 ms/op revoked and 1.5 ms/op clock-expired. These are local diagnostic samples, not representative latency acceptance.
- [x] **P7 standing-answer malformed-query rejection (2026-10-28, source candidate):** TDD exposed that `URL.Query()` silently drops invalid percent-encoded parameters, allowing a request with a malformed selector to continue to identity resolution instead of returning HTTP 400. The handler now strictly parses the raw query and rejects the entire malformed request before any database access. Two regression inputs failed RED with HTTP 401 before the fix and passed GREEN with HTTP 400 afterward; five focused repetitions and two race-enabled repetitions passed. This is request-integrity hardening, not governed answer generation, full P7 acceptance or deployment.
- [ ] Representative multi-tenant end-to-end expiry/revocation and stale-answer latency qualification, including production rollout and recovery evidence.

## Agent execution-trace consolidation

Capture real execution lessons:

```text
attempt -> tool calls -> errors -> fix -> verification -> outcome
```

Consolidate these into evidence-backed procedures/negative memories without treating every raw tool event as authoritative memory.

## Multimodal retrieval

BGE-VL remains later work after text/code retrieval is benchmarked and stable:

- extracted PDF/document text -> BGE-M3 text space;
- screenshots/page renders/diagrams -> separate visual space;
- preserve page/artifact provenance;
- never compare visual/text/code vectors unless a model explicitly defines one shared space and SCOS stores that identity.

## Cross-repository federation

Do not build federation before single-repository P4–P8 quality is proven. Repository identity remains an explicit graph/snapshot boundary.

---

# Current execution order

Following PR #29's 2026-09-10 merge and subsequent `main` verification, the practical sequence is:

1. **P0:** run full before/after P4 model-quality, latency and cost benchmarks; pin production-ready model revisions.
2. **P6:** operationalize memory↔code graph traversal and provenance.
3. **P5:** complete bitemporal/as-of semantics using connected evidence.
4. **P7:** stale-memory/OKF/standing-answer revalidation.
5. **P8:** extend code impact into cross-domain impact intelligence.
6. **P1–P3 hardening:** execute remaining items when they block or materially improve later phases.

P0 remains active throughout P5–P8 as the measurement layer rather than a one-time preliminary phase.

---

# Release gates

No phase is complete because code exists on a branch.

For each phase:

- [ ] focused unit tests pass;
- [ ] relevant PostgreSQL integration tests pass;
- [ ] RLS/tenant isolation is exercised for new tenant-owned tables/queries;
- [ ] migrations are append-only and operationally safe;
- [ ] retrieval/model performance claims publish P0 evidence;
- [ ] production topology remains consistent with `docs/runbooks/production-access.md`;
- [ ] source proof, CI proof and live-deployment proof are labelled separately;
- [ ] implementation is merged to `main`;
- [ ] `main` is re-read after merge to verify expected state actually landed.

## Immediate milestone

**P4 source is merged. Complete its full P0 reproducible acceptance programme, qualify current live BGE endpoints/performance and then advance the cross-domain P5–P8 acceptance cases.**

DeepSeek remains SCOS's reasoning/synthesis layer. BGE remains the specialist retrieval layer beneath it. They are complementary parts of the same stack.
