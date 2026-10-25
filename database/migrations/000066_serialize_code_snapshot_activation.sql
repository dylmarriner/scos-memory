BEGIN;

-- P1: serialize activation across *different* generations of the same
-- repository. Locking only the target snapshot does not prevent two
-- concurrent transactions from each marking a distinct generation active:
-- both can pass the "supersede current active" UPDATE before either commits.
-- Lock the shared repository row before examining active generations.
-- This also serializes the repository-wide active_snapshot_id pointer across
-- branches while leaving branch-local active snapshots independent.
CREATE OR REPLACE FUNCTION activate_code_snapshot(p_snapshot_id uuid)
RETURNS void AS $$
DECLARE
    v_repository_id uuid;
    v_branch text;
BEGIN
    -- The repository ID is immutable. Read it without locking the snapshot
    -- so every activation path can lock repository -> snapshot consistently.
    SELECT repository_id INTO v_repository_id
      FROM code_repository_snapshots
     WHERE id = p_snapshot_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'snapshot % does not exist', p_snapshot_id;
    END IF;

    -- Under READ COMMITTED the next UPDATE observes the previous
    -- activator's committed active generation after waiting on this lock.
    PERFORM 1 FROM code_repositories
     WHERE id = v_repository_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'repository % does not exist', v_repository_id;
    END IF;

    SELECT branch INTO v_branch
      FROM code_repository_snapshots
     WHERE id = p_snapshot_id AND repository_id = v_repository_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'snapshot % moved or vanished', p_snapshot_id;
    END IF;

    UPDATE code_repository_snapshots
       SET status = 'superseded'
     WHERE repository_id = v_repository_id
       AND branch = v_branch
       AND status = 'active'
       AND id != p_snapshot_id;

    UPDATE code_repository_snapshots
       SET status = 'active', activated_at = now()
     WHERE id = p_snapshot_id;

    UPDATE code_repositories
       SET active_snapshot_id = p_snapshot_id, updated_at = now()
     WHERE id = v_repository_id;
END;
$$ LANGUAGE plpgsql;

COMMIT;
