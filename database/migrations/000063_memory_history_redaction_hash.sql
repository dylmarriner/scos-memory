BEGIN;

-- A redaction rewrites a version's content, so it must also rewrite
-- content_hash to match: otherwise the column the guard of 000062 left free
-- could be set to anything, and a hash that no longer describes its content
-- would pass integrity checks for text that is gone. content_hash is the hex
-- SHA-256 of the UTF-8 content, as memory-intelligence's sha256Hex writes it.
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
        AND NEW.content_hash = encode(sha256(convert_to(NEW.content, 'UTF8')), 'hex')
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

COMMIT;
