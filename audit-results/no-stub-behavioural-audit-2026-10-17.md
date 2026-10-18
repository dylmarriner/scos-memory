# No-stub behavioural audit — 2026-10-17

Scope: every service, worker, relay and script on branch `ccr-23c39072-slw54l` (PR #45).
Method: four hostile reviewers (security, memory pipeline, services/workflows, MCP surface) traced
each feature from its entry point to its effect and flagged code that reports success without doing
the work, constant or fabricated values, dead checks, and workflows that cannot reach their end state.
Each finding was verified against the code before it was fixed. Every fix has a unit or integration
test that fails on the old behaviour.

## Security and tenancy

| Finding | Fix | Commit |
|---|---|---|
| The shell risk classifier used keyword lists, and unknown commands graded `read` | Lexer plus a policy-driven classifier shared by Go and Python. Unknown programs are graded write. 91 shared cases | 0bf85c1 |
| execution-proxy results, restores and rewrite rules were visible across tenants | Scoped to the caller's tenant. A rewrite that raises the risk grade is dropped | 571f4e8 |
| Any token could change extension-manager state | `extensions:admin` scope required, agent scope enforced, health taken from real deliveries | 807fa29 |
| memory-gateway `POST /v1/operations` bypassed tool-governance | Removed, along with its MCP tool | 89d6f46 |
| security-gate classified content by keywords | Classified with the secret scanner plus regexes. Findings are returned | 6337961 |
| Observability let clients forge platform history and see all tenants' data | Client ingest is stamped with the tenant, skew is bounded, lists are tenant-scoped | 8ff445c |
| Retrieval's session lane ignored the sensitivity ceiling | The ceiling is applied, and only the requested session is returned | db236ea |
| `scos_status` returned every tenant's pre-warm counts and error text | Statistics are kept per tenant | 95c0871 |
| `cloud_provision` wrote the bearer token world-readable and printed it on error | Written 0600 and atomically, directory 0700, errors name fields only | 5321597 |
| A long graphify ingest failed when the caller's 5-minute token expired | The job re-signs the caller's exact claims, never past the job deadline | 7dc78a8 |

## Memory pipeline

| Finding | Fix | Commit |
|---|---|---|
| Synced turns never reached consolidation | `turn.completed` event and an outbox job are written on sync and pre-compress | ffe773d |
| Deduplication merged negations into the facts they contradict; extraction used invented confidences | Polarity-aware propositions (`statements.py`) and honest extraction rules. Synthesis goes to review | 86de026 |
| Contradiction detection, `superseded_by`, reflect scope and the fleet map were inert | Implemented on real data | 1e4b75a |
| Retrieval hid contradictions and missing models; dead cache | Contradictions are shown, missing models reported as degraded, the dead cache removed | 1f210ab |
| A batch the embeddings worker could not embed counted as an idle cycle; wrong-dimension vectors were skipped silently | The cycle fails, rolls back and is reported in `/health/ready`; the no-op purge shim is removed | 07143f9 |

## Services and workflows

| Finding | Fix | Commit |
|---|---|---|
| A12: an agent in compatibility-gateway stayed "active" forever | Status is derived from `last_seen`. Envelope pushes refresh it | 203335a |
| A13: capability learning always recorded quality 0 | Stages report how complete their output was. Retrieval is graded by its missing lanes and models | 203335a |
| A14: graphify AMBIGUOUS edges were stored as inferred | Stored as `ambiguous_with` | 7dc78a8 |
| B12: OKF proposal "diff" was not a diff | A unified diff produced by git | 938d657 |
| B13: the protected merge checked an env string and committed on any branch | Branch checked, other staged changes refused, only the proposal's path committed, branch moved with a compare-and-swap, file restored on failure | 938d657 |
| B14: OKF sync never reported `Removed`, and divergence used stale memory | Indexed content is re-read before comparing. Clean deletions are reported as removed | 938d657 |
| OKF document IDs collided with existing rows | Random IDs | e8bc4dd |
| W9: acceptance always exited 0, and the router was tuned to the frozen queries | Non-zero exit when a domain fails. 36 held-out routing cases (suite 3.2.0 records which ones were seen) | 77949ef |
| W10: the governance runtime audit's auth check could never fire | Matches every way a service starts serving. Exceptions are named with reasons | 5f3c1c5 |
| Handoffs never reached "completed" | `POST /v1/handoffs/complete` and `scos_handoff_complete`, migration 000061 | 9a9ca10 |
| A team invite's capacity lock ran outside a transaction | One transaction. Outstanding invites count toward capacity. 60-way concurrency test | 99a2992 |
| tool-governance reconcile bypassed the state machine, and "reconciled" was unreachable | Outcomes computed from `validTransitions` | 3b0ab7d |
| Observability targets were bare constants | Each objective is reported beside its 24-hour measurement, or marked unmeasured with the reason | 963a777 |
| Code-intel snapshots were labelled with a caller-chosen `extractor_version` | The server sets it to the extractor that ran | 11f3329 |

## MCP surface

| Finding | Fix | Commit |
|---|---|---|
| A fixed idempotency key per argument set collapsed distinct calls | A fresh key per call unless the caller names one | d166ddc |
| Unescaped path arguments | Every path argument is escaped as one segment | d166ddc |
| Schemas were out of step with the services | Schemas corrected | d166ddc |
| MCP15: list tools returned every retained point | Default 100 entries, maximum 1000, filters passed through. Numeric arguments now reach query strings | 95c0871 |
| The stdio bridges crashed on JSON-RPC batches and dropped parse errors | Batches are relayed member by member. Errors -32700/-32600 are returned | 840aba5 |

## Open items

- **`packages/hermes-memory-provider`:** an unused file-backed duplicate of the `hermes-provider` service. Deleted with the owner's approval (2026-10-18). The code index and acceptance results were regenerated afterwards.
- **Other packages nothing imports:** `agent-adapter-sdk`, `hook-sdk`, `skill-sdk`, `context-contracts`, `extension-contracts`, `routing-contracts`, `language-adapters` and `benchmark-harness`. Nothing in the repository imports them, but they are tested public SDK and contract packages, and the code-intelligence acceptance corpus looks some of them up. They are kept.
- **Bitwarden drill (`secret_manager_recovery`):** done (2026-10-18). See "Key recovery from the self-hosted vault" below.
- **execution-proxy legacy records with no tenant:** these are left unreadable on purpose, so that access fails closed. The owner decided (2026-10-18) to keep them fail-closed.

## Verification

- **Go:** `go vet ./...` and `go test ./...` pass.
- **Go integration:** `go test -tags=integration` passes against Postgres 16 with the migrations and policies applied. The exceptions are suites that require a TLS database URL, which the CI database provides.
- **Python:** `pytest tests` passes (86 tests). Worker suites for consolidation and embeddings also pass.

## Live deployment (kubuntux, 2026-10-17)

The fabric was redeployed from this branch, and the results below were verified through MCP via the stdio bridge.

**Checks that passed:**
- **Tools:** 156 tools are listed.
- **`scos_status`:** scoped to the tenant.
- **`scos_observability_targets`:** reports the measured p95 retrieval latency, 583 ms against the 750 ms objective, met.
- **Lists:** `scos_logs_list` honours `limit`.
- **Memory:** `scos_remember` followed by `scos_recall` returns the stored fact.
- **Backup coverage:** 11 of 12 covered at the time. The twelfth, `secret_manager_recovery`, was covered on 2026-10-18 (below).

**Fixed during the deployment:**
- **Relation lane:** the deployment showed the relation lane failing with `column "content" does not exist`. Fixed, and covered by an integration test (16c3e30).
- **`tunnel-client`:** it crash-looped on placeholder credentials. It now starts only with `COMPOSE_PROFILES=tunnel` (16c3e30).

**Retrieval models:**
- **Before:** they had never been enabled on kubuntux, so recall ran on full-text matching only and reported `text_embedding` and `reranker` as degraded.
- **Text model enabled:** bge-m3 now runs through `model-runtime` on CPU, using the `text-model` profile. Its caches are on the backup drive. It has `--max-batch-tokens 4096` and a 5 GB memory cap, because TEI's default warm-up crash-looped it on this 15 GB host.
- **Re-embedding:** the embeddings worker activated the 1024-dimension space and re-embedded all 36 memory versions.
- **Recall:** now returns `fulltext+semantic` hits.
- **Reranker:** a first attempt with bge-reranker beside bge-m3 exhausted host memory. A smaller cross-encoder now runs (see "Retrieval models on kubuntux: complete" below).

## Live dashboard audit (kubuntux)

Every tab of the deployed dashboard (overview, memory, facts, agents, connect, system, flow) was opened in headless Chrome over the DevTools protocol, screenshotted, and checked for page errors, failed requests and the data each tab loaded.

**Problems found and fixed (62a2877, 59ea60a):**
- **Six panels stuck on "waiting for … projection":** their Go upstreams encode an empty list as `null`. The projection now turns a list view's `null` into `[]`, so empty lists render as empty.
- **Empty OKF panel:** okf-synchroniser only indexed when `/v1/sync` was called. It now syncs at startup and every `SCOS_OKF_SYNC_INTERVAL` (default 5m), and the 10 seeded documents are indexed.
- **Favicon:** the `favicon.ico` request returned 401. An inline icon replaces it, and the CSP now allows `img-src data:`.
- **Versions:** memory-intelligence reported a hardcoded `0.1.0`. Images are now built with `SOURCE_SHA` and `BUILD_TIME`, and every service, memory-intelligence included, reports the commit it was built from.

**Re-audit after redeploying aa23e9f:**
- No page errors and no failed requests.
- No null views.
- The agents tab shows its real empty state ("no agents registered yet").
- The flow tab shows live request-flow data: a 16-node service graph, throughput, 5xx ratio, p95, and history over 5 minutes to 90 days.
- The facts, memory and system tabs show the live records.
- The only random values in the dashboard source are the starting coordinates of the force-directed graph layout. No data is mocked.

## Review findings (Codex, PR #45)

Seven of the eight findings were confirmed and fixed. Each fix has a test.

| Finding | Commit |
|---|---|
| Shell programs named by expansion (`$x`, `$(…)`) graded only "write" | a9decd5 |
| Bridge crashed on array `params` | a9decd5 |
| OKF startup committed unreviewed dirty state | 77ab0a0 |
| Physical backups silently omitted tablespaces | 3f1eea2 |
| Migration export could disagree with its dump | 3f1eea2 |
| Object store never repaired damaged copies, and could not recover a retained version | 0b9b5cd |

The eighth finding, execution-proxy legacy records that have no tenant, is a deliberate fail-closed. It is answered on the PR and left for the owner to decide.

## Acceptance benchmark

Regenerated with every domain measured (aa23e9f), on the service binaries with a disposable PostgreSQL. Every threshold passes:

| Domain | Result |
|---|---|
| Context optimisation | 67% token reduction; instruction, value and evidence retention 1.0 |
| Code intelligence | Exact lookup, recall and precision 1.0 |
| Execution proxy | 95% token reduction; destructive commands blocked; failures and raw output preserved |
| Routing, frozen cases | Precision 1.0, recall 1.0 |
| Routing, held-out cases | Precision 0.98, recall 1.0 |

## Retrieval models on kubuntux: complete

The reranker now runs too. cross-encoder/ms-marco-MiniLM-L-6-v2 (22M parameters) uses 276 MiB under a 768 MB cap, with no restarts and about 4.6 GB of host memory left. bge-reranker-v2-m3 and bge-reranker-base were both tried first and did not fit beside bge-m3 on this 15 GB host. model-runtime and retrieval record the model name, so results carry the reranker that ranked them.

`scos_recall` through MCP now reports `degraded_models: []` and `degraded_sources: []`. Hits come from the full-text and semantic lanes and are reranked by the model.

## Key recovery from the self-hosted vault (kubuntux, 2026-10-18)

The owner chose a self-hosted Bitwarden. Vaultwarden 1.37.4 was already running on rabuntu, but Vaultwarden has no Secrets Manager, so `bws` cannot read from it. Self-hosted Secrets Manager needs a Bitwarden Enterprise licence.

**The drill now reads a vault item (f3c90a8).**
- It signs in with the account's API key.
- It derives the master key on the host with PBKDF2 or Argon2id, checked against Bitwarden SDK known answers.
- It unwraps the account key and decrypts the one item, checking every MAC.

**A dedicated vault account holds only the backup key.**
- The drill runs every day.
- The live drill recovered the key and used it to authenticate and decrypt backup `backup-1792290515881991323`.

**The plan needed a longer timeout (c894119).**
- `scos_backup_plan` re-verifies the newest backup and its object-store copy on every call, and that had outgrown the gateway's 15-second client.
- The plan now goes through the long-call client, and the call completed in 16 seconds.

**Backup coverage through MCP is now 12 of 12:**
- base backups, schema exports, schema and migration exports, configuration backups;
- encrypted off-host copies and encrypted off-host WAL;
- PostgreSQL WAL archiving, PITR;
- object-store snapshots, remote Git mirrors, the remote provider;
- secret-manager recovery.
