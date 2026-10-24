BEGIN;

-- memory_versions is append-only (000007), so a secret that reached a
-- memory's history -- written before the scanner knew its shape -- stayed
-- there for good, readable through scos_fact_history. A version may now be
-- rewritten in exactly one way: spans of its text replaced by the scanner's
-- [REDACTED:kind] markers. Every other column stays as it was, and no text
-- can be added or changed, so history can lose a secret but cannot be
-- forged. The rewrite is announced per row through the transaction-local
-- setting scos.redacting_version.

-- scos_is_redaction_of reports whether redacted is original with zero or
-- more non-empty spans replaced by [REDACTED:kind] markers.
CREATE OR REPLACE FUNCTION scos_is_redaction_of(original text, redacted text)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
AS $$
DECLARE
    parts text[];
    n integer;
    pos integer;
    found integer;
BEGIN
    IF original IS NOT DISTINCT FROM redacted THEN
        RETURN true;
    END IF;
    IF original IS NULL OR redacted IS NULL THEN
        RETURN false;
    END IF;
    parts := regexp_split_to_array(redacted, '\[REDACTED:[a-z_]+\]');
    n := array_length(parts, 1);
    IF n < 2 OR left(original, length(parts[1])) <> parts[1] THEN
        RETURN false;
    END IF;
    -- pos is the 1-based index just after the text matched so far; each
    -- marker must stand for at least one character, so the next part starts
    -- strictly after it.
    pos := length(parts[1]) + 1;
    FOR i IN 2..n - 1 LOOP
        found := strpos(substr(original, pos + 1), parts[i]);
        IF found = 0 THEN
            RETURN false;
        END IF;
        pos := pos + found + length(parts[i]);
    END LOOP;
    RETURN length(original) - length(parts[n]) >= pos
        AND right(original, length(parts[n])) = parts[n];
END;
$$;

CREATE OR REPLACE FUNCTION guard_memory_version_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'UPDATE'
        AND current_setting('scos.redacting_version', true) = OLD.id::text
        AND to_jsonb(NEW) - 'content' - 'structured_content' - 'content_hash'
            = to_jsonb(OLD) - 'content' - 'structured_content' - 'content_hash'
        AND scos_is_redaction_of(OLD.content, NEW.content)
        AND scos_is_redaction_of(OLD.structured_content::text, NEW.structured_content::text)
    THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'authoritative history table % is append-only; DELETE is not permitted', TG_TABLE_NAME
            USING ERRCODE = '55006';
    END IF;
    RAISE EXCEPTION 'authoritative history table % is append-only; UPDATE is permitted only as a redaction of secret spans',
        TG_TABLE_NAME
        USING ERRCODE = '55006';
END;
$$;

DROP TRIGGER IF EXISTS memory_versions_append_only ON memory_versions;
CREATE TRIGGER memory_versions_append_only
    BEFORE UPDATE OR DELETE ON memory_versions
    FOR EACH ROW
    EXECUTE FUNCTION guard_memory_version_mutation();

COMMIT;
