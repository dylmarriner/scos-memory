# P5 contradiction evidence: endpoint visibility and temporal scope

Status: **source-level security hardening under review**, not production qualified.

## Finding

The `memory_relations_item_access` policy in `database/policies/000003_data_plane_rls.sql` originally checked only the source memory. A scoped runtime principal could therefore read a relation from an authorized source to an inaccessible target, exposing the target UUID. The retrieval orchestrator also aggregated conflict IDs from all linked rows without independently checking the opposite endpoint's namespace, tenant, sensitivity, status, or validity. This remained visible in queries using a database owner role even when RLS would otherwise filter rows.

The regression `TestP5ConflictEvidenceRequiresReadableBothEndpoints` reproduced both defects on disposable PostgreSQL 17/pgvector before implementation: exact and full-text retrieval each returned six conflict entries instead of the one eligible peer, and the restricted application role returned five relation rows instead of three authorized pairs. These are local test fixtures, not evidence of production exploitation.

## Source changes

- Migration `000081_memory_relation_endpoint_rls.sql` transactionally replaces the old policy **if it already exists**, so installed databases can upgrade without needing to reapply all policy files. On a fresh install, policy file `000003` creates the new two-endpoint rule after migrations. Both source and target must be readable through `memory_items` RLS and have matching tenant IDs. The same rule applies to writes.
- All seven retrieval projections (exact, full-text, vector, recent, task state, session state, relation expansion) now check the opposite endpoint's tenant, selected namespace, requested sensitivity ceiling **and namespace ceiling**, current status, valid-time interval, and current expiry. Relation valid-time filtering remains in place. Retrieval with `as_of_valid_time` still represents **current system belief effective at that valid time**, not a historical belief reconstruction.
- The retrieval regression covers same-tenant unreadable namespaces, a readable but unselected namespace, cross-tenant links, reverse-direction relations, above-namespace-ceiling targets, future-valid and revoked targets, seven retrieval lanes, the public HTTP response, runtime-role direct reads and writes, and connection-context cleanup.
- The memory-intelligence `loadRelations` GET and `loadActiveContradictions` reflection paths now independently require same-tenant, readable source and target namespaces and respect both namespace sensitivity ceilings even under a database-owner connection. The tagged PostgreSQL regression `TestP5RelationReadbackRequiresBothEndpoints` reproduced and corrected both owner-role leaks; restricted-role behavior remains covered.

## Verification and limitations

On AMD through OpenCommander, a disposable PostgreSQL 17/pgvector instance applied the prior 79 migration scripts and 21 policies; the upgrade migration then applied successfully. A second fresh disposable database applied all 80 migration scripts and 21 policies, and its catalog confirmed the endpoint-aware policy. The initial RED reproduction and subsequent focused GREEN run are recorded in the engineering execution evidence. Further RED/GREEN regressions demonstrated that the semantic retrieval lane, elevated requested sensitivity, HTTP namespace selection, memory-intelligence GET, and reflection needed separate qualification.

After the complete change set, both focused PostgreSQL regressions passed five consecutive runs each, the full uncached Go suite passed across 51 tested packages, the tagged memory-intelligence integration suite passed, `go vet ./...` passed and `git diff --check` passed. The §41 definition-of-done checker still reports **FAIL** because governance audit §41 remains PARTIAL. PR review and merged-main CI are separate pending gates; they must not be inferred from local testing. No production migration, live tenant data change, deployment or recovery drill has been performed. P5 system-time reconstruction, temporal contradiction acceptance, and the broader P0–P8 gates remain open.
