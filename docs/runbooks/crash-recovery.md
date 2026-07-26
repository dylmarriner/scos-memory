# Crash Recovery Runbook

## Purpose

Restore SCOS Memory Fabric after a process crash, host reboot or power loss without duplicating side effects or losing committed turns.

## Recovery sequence

1. Confirm PostgreSQL is healthy and migrations match the expected schema version.
2. Confirm the object store and OKF repository are accessible.
3. Start the Memory Gateway.
4. Query turns in `running` or `interrupted` state.
5. Load each session's latest checkpoint.
6. Replay events after `last_event_sequence`.
7. Identify operations in `executing` or `unknown` state.
8. Reconcile externally side-effecting operations before retry.
9. Reset expired outbox leases.
10. Resume idempotent pending work.
11. Rebuild disposable caches and indexes only after authoritative services are stable.
12. Record a recovery audit entry.

## Prohibitions

- Do not delete interrupted turns to make dashboards look clean.
- Do not retry unknown non-idempotent operations without provider reconciliation.
- Do not mark readiness until mandatory dependency checks pass.
- Do not rebuild from embeddings when authoritative content exists.

## Verification

- Every accepted user message remains present.
- Every completed reply remains present.
- Interrupted chunks are visible and ordered.
- No operation has been applied twice.
- Outbox backlog declines normally.
- Retrieval can cite authoritative evidence.
