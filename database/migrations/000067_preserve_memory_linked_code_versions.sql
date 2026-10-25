-- P1: linked code evidence is immutable until the governing memory evidence
-- relation is explicitly removed through the authorised lifecycle.
-- The prior ON DELETE CASCADE silently destroyed memory_code_links when
-- a code symbol version (or its parent snapshot) was garbage-collected.
--
-- Keep the metadata lock short. Split validation into a separate transaction
-- so readers and writers are not held behind a long full-table scan.
BEGIN;
SET LOCAL lock_timeout='5s';
ALTER TABLE memory_code_links
    DROP CONSTRAINT memory_code_links_code_symbol_version_id_fkey,
    ADD CONSTRAINT memory_code_links_code_symbol_version_id_fkey
    FOREIGN KEY (code_symbol_version_id)
    REFERENCES code_symbol_versions(id)
    ON DELETE RESTRICT
    NOT VALID;
COMMIT;

BEGIN;
SET LOCAL lock_timeout='5s';
ALTER TABLE memory_code_links
    VALIDATE CONSTRAINT memory_code_links_code_symbol_version_id_fkey;
COMMIT;
