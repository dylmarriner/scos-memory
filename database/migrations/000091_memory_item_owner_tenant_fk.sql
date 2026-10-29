-- P7 source provenance: ownership must not cross tenant boundaries even when
-- a privileged import bypasses application RLS. The legacy owner UUID FK
-- only proves the principal exists, not that it belongs to the memory tenant.
BEGIN;
SET LOCAL lock_timeout = '5s';

-- PostgreSQL needs a unique composite referenced key; principal IDs are
-- already unique globally, so this introduces no additional data ambiguity.
DO $guard$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'principals'::regclass
          AND conname = 'principals_tenant_id_id_unique'
    ) THEN
        ALTER TABLE principals
            ADD CONSTRAINT principals_tenant_id_id_unique
            UNIQUE (tenant_id, id);
    END IF;
END
$guard$;

-- NOT VALID checks all new INSERTs/UPDATEs without rewriting or accepting
-- historical invalid owner links. Existing malformed rows remain hidden by
-- the compose endpoint's same-tenant JOIN. Inventory and remediate those
-- rows before a separately approved production VALIDATE CONSTRAINT.
DO $guard$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'memory_items'::regclass
          AND conname = 'memory_items_owner_tenant_fk'
    ) THEN
        ALTER TABLE memory_items
            ADD CONSTRAINT memory_items_owner_tenant_fk
            FOREIGN KEY (tenant_id, owner_principal_id)
            REFERENCES principals (tenant_id, id)
            NOT VALID;
    END IF;
END
$guard$;
COMMIT;
