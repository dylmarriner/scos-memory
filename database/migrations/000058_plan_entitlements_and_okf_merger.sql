BEGIN;

-- Cloud plan tiers carried their quotas only as marketing copy in
-- cloud_plan_definitions.perks ("10,000 MCP requests/month", "1 GB memory
-- storage", "Advanced retrieval and graphify", "extended retention") and
-- nothing enforced any of it. The enforced limits now live in columns the
-- cloud control plane reads on every hosted MCP request, and the pricing
-- page renders its quota and feature lines from these same columns, so what
-- a plan advertises is exactly what it enforces. NULL means unlimited.
--
--   monthly_mcp_requests  hosted /mcp requests per calendar month (UTC)
--   storage_bytes         memory content stored (current versions), checked
--                         before any tool call that adds memory content
--   features              gated capabilities; "graphify" admits the
--                         graphify tools and dashboard panels
--
-- perks keeps only the free-text lines that are not quotas or gated
-- features. "Advanced retrieval" and "extended retention" had no
-- implementation distinguishing any plan, so they are no longer advertised.
ALTER TABLE cloud_plan_definitions
    ADD COLUMN IF NOT EXISTS monthly_mcp_requests bigint CHECK (monthly_mcp_requests IS NULL OR monthly_mcp_requests >= 0),
    ADD COLUMN IF NOT EXISTS storage_bytes bigint CHECK (storage_bytes IS NULL OR storage_bytes >= 0),
    ADD COLUMN IF NOT EXISTS features jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(features) = 'array');

UPDATE cloud_plan_definitions SET monthly_mcp_requests = 10000, storage_bytes = 1000000000, features = '[]'::jsonb,
    perks = '["1 personal sandbox","Community support"]'::jsonb, updated_at = now() WHERE id = 'free';
UPDATE cloud_plan_definitions SET monthly_mcp_requests = 100000, storage_bytes = 10000000000, features = '[]'::jsonb,
    perks = '["1 personal sandbox","Email support"]'::jsonb, updated_at = now() WHERE id = 'basic';
UPDATE cloud_plan_definitions SET monthly_mcp_requests = 1000000, storage_bytes = 100000000000, features = '["graphify"]'::jsonb,
    perks = '["1 personal sandbox","Email support"]'::jsonb, updated_at = now() WHERE id = 'pro';
UPDATE cloud_plan_definitions SET monthly_mcp_requests = 5000000, storage_bytes = 1000000000000, features = '["graphify"]'::jsonb,
    perks = '["1 personal sandbox","Priority support"]'::jsonb, updated_at = now() WHERE id = 'expert';
-- Platform administration is granted by the creator platform role, not by
-- this plan, so the plan no longer claims it.
UPDATE cloud_plan_definitions SET monthly_mcp_requests = NULL, storage_bytes = NULL, features = '["graphify"]'::jsonb,
    perks = '["Assigned to platform creator accounts"]'::jsonb, updated_at = now() WHERE id = 'creator';

-- The OKF merge is its own privileged act: record who performed it and the
-- governance role they held, separately from the reviewer who approved.
ALTER TABLE okf_proposals
    ADD COLUMN IF NOT EXISTS merged_by text,
    ADD COLUMN IF NOT EXISTS merge_role text;

COMMIT;
