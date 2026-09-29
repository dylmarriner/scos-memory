package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// P0-0 (make RLS actually enforce): Postgres exempts BYPASSRLS roles and
// superusers from every row-security
// policy regardless of FORCE ROW LEVEL SECURITY. database/policies/ enables
// and forces RLS on every tenant-scoped table; that guarantee only holds if
// every service actually connects as a NOSUPERUSER NOBYPASSRLS role
// (scos_app, provisioned by database/policies/000011_app_role.sql). This is
// the regression guard for that: without it, someone could quietly repoint
// a service's DATABASE_URL back at the migration-owning role and nothing
// would fail until an isolation incident -- this makes that fail loud at
// startup instead.
//
// rolsuper is checked explicitly: a role created SUPERUSER without BYPASSRLS
// has rolbypassrls = false in pg_roles yet still skips every policy.
const bypassRLSQuery = `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`

// ErrConnectingRoleBypassesRLS is returned when the connected role is a
// superuser or has BYPASSRLS -- every
// row-security policy in database/policies/ is silently inert for this
// connection regardless of FORCE ROW LEVEL SECURITY.
var ErrConnectingRoleBypassesRLS = fmt.Errorf("connecting database role is a superuser or has BYPASSRLS -- row-level security policies are not enforced for this connection; services must connect as the restricted scos_app role, not the migration-owning role")

// requireNoBypassRLS is called automatically by NewPool, once per pool, right
// after it opens -- not opt-in, unlike RequireTLS, because NewPool is never
// used by the migration runner (scripts/scos-migrate is a plain shell
// script, not Go code, and never touches this package), so there is no
// legitimate caller of NewPool that needs BYPASSRLS to succeed. Any Go
// process that opens a pool through this package is, by construction, a
// request-serving service that must be subject to RLS.
func requireNoBypassRLS(ctx context.Context, pool *pgxpool.Pool) error {
	var bypass bool
	if err := pool.QueryRow(ctx, bypassRLSQuery).Scan(&bypass); err != nil {
		return fmt.Errorf("checking connecting role's SUPERUSER/BYPASSRLS attributes: %w", err)
	}
	if bypass {
		return ErrConnectingRoleBypassesRLS
	}
	return nil
}
