BEGIN;

CREATE TABLE IF NOT EXISTS cloud_plan_definitions (
    id text PRIMARY KEY,
    name text NOT NULL,
    price text NOT NULL,
    perks jsonb NOT NULL DEFAULT '[]'::jsonb,
    sort_order integer NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO cloud_plan_definitions (id, name, price, perks, sort_order) VALUES
    ('free', 'Free', '$0', '["1 personal sandbox","10,000 MCP requests/month","1 GB memory storage","Community support"]'::jsonb, 1),
    ('basic', 'Basic', '$9/month', '["1 personal sandbox","100,000 MCP requests/month","10 GB memory storage","Email support"]'::jsonb, 2),
    ('pro', 'Pro', '$29/month', '["1 personal sandbox","1,000,000 MCP requests/month","100 GB memory storage","Advanced retrieval and graphify"]'::jsonb, 3),
    ('expert', 'Expert', '$99/month', '["1 personal sandbox","5,000,000 MCP requests/month","1 TB memory storage","Priority support and extended retention"]'::jsonb, 4),
    ('creator', 'Creator', 'Platform role', '["All Expert capabilities","Platform administration","Customer and tenant support tools","Plan and entitlement management"]'::jsonb, 5)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS cloud_admin_audit_log (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id uuid NOT NULL REFERENCES cloud_users(id) ON DELETE CASCADE,
    action text NOT NULL,
    target text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cloud_admin_audit_log_created_idx ON cloud_admin_audit_log(created_at DESC);

COMMIT;
