BEGIN;

-- execution-proxy used to run a destructive command whenever its caller sent
-- `confirmed: true`, and forwarded that flag to tool governance, which
-- recorded it on the operation it prepared. Any holder of the control-plane
-- token could therefore assert a confirmation that was never given.
--
-- The confirmation that counts is now the one tool governance recorded when
-- the autonomous control plane started the operation, which it does with
-- confirmed=true only after the requesting principal confirmed the decision
-- through /v1/control/confirm. execution-proxy runs a command under that
-- operation by naming it to /v1/operations/prepare, and governance answers
-- with the confirmation and risk on the operation's record. Two facts make
-- that hand-over safe, and are recorded here with the tool_executions
-- snapshot:
--
--   started_by           the principal that started or prepared the
--                        operation; only that principal's executor may
--                        run it.
--   executor_claimed_at  when an executor took the operation. An operation
--                        prepared by its executor is claimed when it is
--                        created; one the control plane started is claimed
--                        by the first prepare that names it, so a recorded
--                        confirmation authorises exactly one execution.
--
-- Rows written before this migration have no started_by, so no executor can
-- claim them: they keep the history they recorded and authorise nothing new.
ALTER TABLE tool_executions
    ADD COLUMN IF NOT EXISTS started_by text,
    ADD COLUMN IF NOT EXISTS executor_claimed_at timestamptz;

COMMIT;
