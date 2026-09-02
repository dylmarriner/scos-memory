BEGIN;

-- Short-lived, single-use tokens embedded in the personalized "Connect this
-- machine" install one-liner (dashboard Connect tab -> cloud-control-plane
-- POST /api/cli/provisioning-token). Deliberately not the long-lived
-- cloud_api_keys bearer token itself -- this is what's safe to show/copy in
-- a one-liner, since POST /api/cli/exchange consumes it once to mint a real
-- key server-side rather than handing the key out directly.
CREATE TABLE IF NOT EXISTS cloud_provisioning_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES cloud_users(id) ON DELETE CASCADE,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at timestamptz
);
CREATE INDEX IF NOT EXISTS cloud_provisioning_tokens_active_idx
    ON cloud_provisioning_tokens(token_hash, expires_at) WHERE used_at IS NULL;

COMMIT;
