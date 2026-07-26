# Autonomous routing recovery runbook

## Trigger conditions

Use this procedure when an orchestration run is interrupted, a route remains in an executing state, capability health changes during a turn, or the control plane restarts unexpectedly.

## Recovery procedure

1. Load the latest committed orchestration run and turn checkpoint.
2. Read events after the checkpoint sequence.
3. Identify completed, pending and unknown operations.
4. Reconcile every unknown side effect before retry.
5. Refresh the capability registry health generation.
6. Re-run deterministic permission, sensitivity and confirmation checks.
7. Rebuild the route using the original policy version when still available.
8. Compare the rebuilt route with the recorded decision.
9. Resume from the first incomplete idempotent stage.
10. Create an orchestration recovery event and attach the prior decision ID.

## Prohibited recovery behaviour

- Do not create a fresh route that silently repeats a non-idempotent operation.
- Do not downgrade confirmation requirements because the original agent is unavailable.
- Do not discard the committed user message, raw tool output or partial response chunks.
- Do not mark the turn complete until reply, checkpoint and consolidation outbox entries commit.

## Verification

The recovered run is valid only when its decision chain is replayable, its operations reconcile, its checkpoint advances and no duplicate external side effect occurred.
