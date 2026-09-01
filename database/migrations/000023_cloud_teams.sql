BEGIN;

CREATE TABLE IF NOT EXISTS cloud_teams (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL UNIQUE REFERENCES tenants(id) ON DELETE CASCADE,
    owner_user_id uuid NOT NULL REFERENCES cloud_users(id) ON DELETE CASCADE,
    name text NOT NULL,
    max_members integer NOT NULL DEFAULT 50 CHECK (max_members BETWEEN 1 AND 50),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cloud_team_members (
    team_id uuid NOT NULL REFERENCES cloud_teams(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES cloud_users(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    role text NOT NULL DEFAULT 'member' CHECK (role IN ('owner','admin','member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id),
    UNIQUE (team_id, principal_id)
);

CREATE TABLE IF NOT EXISTS cloud_team_invitations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id uuid NOT NULL REFERENCES cloud_teams(id) ON DELETE CASCADE,
    email text NOT NULL,
    role text NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member')),
    token_hash text NOT NULL UNIQUE,
    invited_by uuid NOT NULL REFERENCES cloud_users(id),
    expires_at timestamptz NOT NULL DEFAULT now() + interval '7 days',
    accepted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cloud_team_invitations_email_idx ON cloud_team_invitations(lower(email)) WHERE accepted_at IS NULL;

COMMIT;
