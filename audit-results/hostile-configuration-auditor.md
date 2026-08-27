# Specialist audit artifact

```text
Skill: hostile-configuration-auditor
Agent: hostile-configuration-auditor-workflow-agent
Scope: deploy/compose/docker-compose.yml, deploy/compose/docker-compose.override.dev.yml,
  deploy/native/env/*, deploy/native/install.sh, deploy/native/README.md, deploy/Dockerfile,
  every services/*/cmd/server/main.go (19 Go services), packages/auth/middleware.go,
  packages/tlsserve/serve.go, packages/database/{pool.go,tls.go}, packages/service-state/state.go,
  services/backup-manager (production-backup gating), services/mcp-gateway (identity resolution),
  docs/agent-connection-guide.md (for how mcp-gateway's env/credential story is presented to
  external operators).

Files/components inspected:
  - deploy/compose/docker-compose.yml (all 19 governed services + postgres + migrations +
    5 Python/embeddings workers + tls-certs generator, full env matrix, anchors
    *service-auth-environment, *service-tls-environment, *service-runtime-environment,
    *deepseek-environment)
  - deploy/compose/docker-compose.override.dev.yml
  - deploy/native/env/*.env (19 files) + scos-fabric.env.example
  - deploy/native/install.sh, deploy/native/README.md
  - deploy/Dockerfile
  - services/agent-registry/cmd/server/main.go
  - services/autonomous-control-plane/cmd/server/main.go
  - services/backup-manager/cmd/server/main.go (full read, incl. productionConfigError,
    requireTLSDatabaseURL, handleBackupStart, handleProductionBackupStart)
  - services/code-intelligence/cmd/server/main.go
  - services/compatibility-gateway/cmd/server/main.go
  - services/context-optimisation/cmd/server/main.go
  - services/execution-proxy/cmd/server/main.go (configuredGovernance, rejectingGovernance,
    toolGovernanceClient, addr default)
  - services/extension-manager/cmd/server/main.go
  - services/graphify-provider/cmd/server/main.go (namespace/tenant/principal/backend gating)
  - services/hermes-provider/cmd/server/main.go
  - services/mcp-gateway/cmd/server/main.go (full read: resolveUpstream, main, resolveIdentity,
    dispatchTool, header forwarding at ~line 781-806)
  - services/memory-gateway/cmd/server/main.go
  - services/memory-intelligence/cmd/server/main.go
  - services/observability/cmd/server/main.go
  - services/okf-synchroniser/cmd/server/main.go
  - services/recovery-orchestrator/cmd/server/main.go
  - services/retrieval-orchestrator/cmd/server/main.go
  - services/security-gate/cmd/server/main.go
  - services/tool-governance/cmd/server/main.go
  - services/transparency-projection/cmd/server/main.go (editPassphrase gating)
  - packages/auth/middleware.go (FromEnvironment, AuthenticatedHandler, ControlPlaneHandler,
    validateClaims)
  - packages/tlsserve/serve.go (ListenAndServe, Serve, TrustedClient)
  - packages/database/pool.go, packages/database/tls.go (RequireTLS)
  - packages/service-state/state.go (confirms RequireTLS is invoked internally by New())
  - docs/agent-connection-guide.md (bearer token guidance for external MCP clients)

Entry points inspected:
  - Every service's HTTP listener bind address (SCOS_*_HTTP_ADDR defaults) in main()
  - mcp-gateway /mcp (externally reachable per docs/agent-connection-guide.md), /health/live,
    /health/ready
  - backup-manager /v1/backups/start -> handleBackupStart -> handleProductionBackupStart
  - execution-proxy configuredGovernance() (tool-governance dependency wiring)
  - transparency-projection /v1/dashboard/login and edit-gated handlers
  - graphify-provider handleExtract (namespace/tenant/principal preconditions)

Important callers/callees:
  - tlsserve.ListenAndServe/Serve -> every service's main() -> auth.AuthenticatedHandler /
    auth.ControlPlaneHandler
  - auth.FromEnvironment -> auth.MustFromEnvironment (panics on missing
    SCOS_INTERNAL_SERVICE_TOKEN/SCOS_CONTROL_PLANE_TOKEN) -> called by every service's main()
  - execution-proxy.configuredGovernance -> tool-governance HTTP client or rejectingGovernance{}
    (fail closed when SCOS_TOOL_GOVERNANCE_URL unset)
  - servicestate.New -> database.RequireTLS (used by execution-proxy, observability, and others
    via packages/service-state, independently of packages/database.NewPool)
  - backup-manager.handleProductionBackupStart -> productionConfigError() (includes
    requireTLSDatabaseURL, SCOS_BACKUP_PROVIDER_VERIFIED, SCOS_BACKUP_OFF_HOST_VERIFIED,
    SCOS_SECRET_MANAGER/SCOS_BACKUP_ENCRYPTION_KEY_REF, PITR/WAL, git-mirror checks) before
    exec.CommandContext("pg_dump", ...)
  - mcp-gateway.dispatchTool -> resolveIdentity(r) -> falls back to raw
    X-SCOS-Tenant-ID/X-SCOS-Principal-ID request headers whenever the caller's Authorization
    bearer does not carry validated claims -> forwarded to every downstream governed service
    (memory-gateway, retrieval-orchestrator, execution-proxy, tool-governance, etc., all of
    which trust these headers for tenant scoping, per grep across 14 services)

Workflows traced:
  - Cold start with no env configured (compose): every `:?...must be set` var fails `docker
    compose up` outright (fail closed) — SCOS_INTERNAL_SERVICE_TOKEN, SCOS_CONTROL_PLANE_TOKEN,
    SCOS_SECURITY_ISSUER_SECRET, SCOS_DATABASE_URL, POSTGRES_PASSWORD, SCOS_AGENT_ENDPOINT.
  - Native systemd cold start: scos-fabric.env.example ships all four required secrets and
    SCOS_AGENT_ENDPOINT as empty values; docs/security-configuration.md is cited as the
    enforcement point but the actual enforcement is auth.MustFromEnvironment's panic at process
    start, observed in code, not a systemd-level check.
  - execution-proxy without SCOS_TOOL_GOVERNANCE_URL set: AuthorizeAndPrepare/Reconcile both
    return "tool governance dependency is not configured" — every execution request is rejected,
    confirmed fail-closed by reading rejectingGovernance{}.
  - backup-manager with default env (SCOS_BACKUP_PROVIDER unset -> "disabled"): handleBackupStart
    routes non-"local" providers to unavailable(); default is safe.
  - backup-manager production path: productionConfigError() gates handleProductionBackupStart
    before any pg_dump invocation, including its own requireTLSDatabaseURL check (duplicated
    logic vs packages/database.RequireTLS, but has the same fail-closed effect and is exercised
    on the only call site that shells out to pg_dump/pg_restore).
  - mcp-gateway external MCP call using the internal service token (not a JWT): resolveIdentity
    reads X-SCOS-Tenant-ID / X-SCOS-Principal-ID directly off the incoming request and forwards
    them downstream as the authenticated tenant/principal for every governed tool call.
  - TLS: SCOS_TLS_REQUIRED defaults to "true" in both compose (`${SCOS_TLS_REQUIRED:-true}`) and
    the native env template (`SCOS_TLS_REQUIRED=true`), so tlsserve.ListenAndServe/Serve fail
    closed rather than silently downgrading to plaintext for both shipped deployment paths.
    tlsserve.go's own default (no env at all) is plaintext-permitting — that behavior is reachable
    only by running a binary directly outside both shipped deployment paths.

Trust boundaries:
  - Public internet / Tailscale network -> mcp-gateway (only externally reachable service,
    per docs/agent-connection-guide.md and known context)
  - mcp-gateway -> every other governed service, using SCOS_CONTROL_PLANE_TOKEN and the caller's
    forwarded Authorization header plus X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers
  - Compose network host-port publication: `${SCOS_TAILSCALE_IP:-100.93.75.87}:<port>:<port>` —
    Docker's docker-proxy binds only that specific host IP, so the container network boundary is
    enforced at the Docker port-publish layer, not by the in-process listen address.
  - Native/systemd deployment: no Docker port-publish layer exists; the process's own listen
    address is the only boundary between "reachable from this host's loopback" and "reachable
    from every network interface the host has."
  - Postgres RLS scoped by tenant_id via set_config('scos.tenant_id', ...), fed by the
    tenant_id resolved from auth claims or (fallback) X-SCOS-Tenant-ID header.

Attacker-controlled inputs:
  - Environment variables an operator sets (or fails to set) for each deployment path.
  - For mcp-gateway specifically: the Authorization header and X-SCOS-Tenant-ID /
    X-SCOS-Principal-ID headers on an inbound external MCP request, when the caller holds only
    the plain SCOS_INTERNAL_SERVICE_TOKEN rather than a claims-bearing JWT signed with
    SCOS_SECURITY_ISSUER_SECRET.

Invariants tested:
  - "A service must not start without its required secrets" — held for
    SCOS_INTERNAL_SERVICE_TOKEN / SCOS_CONTROL_PLANE_TOKEN (auth.MustFromEnvironment panics).
  - "DATABASE_URL must enforce TLS with sslmode=require or stronger" — held for every service
    that touches Postgres, either via packages/database.RequireTLS (12 services call it directly)
    or via packages/service-state.New (execution-proxy, observability) or via backup-manager's
    own requireTLSDatabaseURL (used on its one pg_dump/pg_restore call site).
  - "Production backups require verified, off-host, encrypted, TLS storage" — held
    (productionConfigError enforces all of SCOS_BACKUP_PROVIDER_VERIFIED,
    SCOS_BACKUP_OFF_HOST_VERIFIED, secret manager/key ref, PITR/WAL, git-mirror).
  - "execution-proxy must not execute commands without tool-governance authorization" — held
    (rejectingGovernance{} is the fail-closed default).
  - "TLS must not silently downgrade to plaintext in a governed deployment" — held for both
    shipped deployment paths (SCOS_TLS_REQUIRED defaults to true in both env templates).
  - "Every governed service's tenant scoping must derive from an authenticated identity, not an
    attacker-suppliable header" — VIOLATED at the one externally-reachable boundary
    (mcp-gateway) whenever the caller is provisioned with the shared internal token instead of a
    claims JWT — see Findings.
  - "Internal-only services must not be reachable from outside the deployment network by
    default" — VIOLATED for the native/systemd deployment path — see Findings.

Findings:
  1. [High, Confirmed] mcp-gateway trusts caller-supplied X-SCOS-Tenant-ID/X-SCOS-Principal-ID
     headers as authenticated tenant identity whenever the Authorization bearer is the plain
     shared SCOS_INTERNAL_SERVICE_TOKEN rather than a claims JWT — and the project's own
     external-client onboarding docs hand that plain shared token to external MCP clients as a
     supported configuration.
     - services/mcp-gateway/cmd/server/main.go:797-806 (resolveIdentity): when
       auth.CanonicalClaims(ctx) has no claims, resolveIdentity reads
       r.Header.Get("X-SCOS-Tenant-ID") / X-SCOS-Principal-ID directly from the inbound request
       and treats them as the caller's identity, with no further validation.
     - packages/auth/middleware.go:149-158 (AuthenticatedHandler): claims are only attached to
       the request context when the bearer token contains two "." separators AND
       config.IssuerSecret is non-empty AND validateClaims succeeds; a bearer equal to the plain
       SCOS_INTERNAL_SERVICE_TOKEN authenticates successfully via the
       `subtle.ConstantTimeCompare` branch but carries no claims at all.
     - docs/agent-connection-guide.md:52 documents
       `SCOS_HERMES_BEARER_TOKEN=<internal service token or claims JWT>` as the value external
       Claude Code/Desktop clients configure — i.e. the operator-facing instructions explicitly
       offer the plain shared token (not a per-tenant JWT) as a valid credential for the only
       externally-reachable service in the fleet.
     - Impact: any external MCP client configured per the documented instructions with the
       plain internal service token (rather than a claims JWT signed with
       SCOS_SECURITY_ISSUER_SECRET) can set X-SCOS-Tenant-ID / X-SCOS-Principal-ID to any value
       and have every downstream governed service (memory-gateway, retrieval-orchestrator,
       execution-proxy, tool-governance, agent-registry, hermes-provider, code-intelligence,
       compatibility-gateway, extension-manager, autonomous-control-plane, transparency-projection,
       security-gate, graphify-provider, memory-intelligence — all grep-confirmed readers of
       X-SCOS-Tenant-ID) treat it as authenticated identity, which ultimately becomes the
       Postgres RLS tenant scope (set_config('scos.tenant_id', ...) per the audit's stated
       background). This crosses the tenant-isolation boundary using only the single credential
       every internal service already shares, not a tenant-scoped secret.
     - This is a configuration/deployment-guidance issue, not a pure code bug: the identity
       fallback exists because internal service-to-service calls legitimately need to forward a
       trusted header when the caller is another internal service acting on a known tenant's
       behalf; the gap opens only when that same fallback is reachable from outside the trust
       boundary because the docs recommend the wrong credential type for that boundary.
     - Likely overlaps with hostile-authorization-auditor's territory (identity/claims
       resolution) — flagged here specifically because it stems from a documented deployment/
       credential-configuration choice, which is this auditor's mandate.
     - Mitigation direction: either (a) require a claims-bearing JWT (never the plain shared
       token) for every mcp-gateway caller, i.e. treat "claims absent" as unauthenticated for
       this one externally-reachable service specifically, or (b) stop offering the plain
       SCOS_INTERNAL_SERVICE_TOKEN as a valid external-client credential in
       docs/agent-connection-guide.md and enforce that at the code level.

  2. [Medium, High-confidence] Every one of the 19 Go services binds its HTTP listener to all
     network interfaces by default (`SCOS_*_HTTP_ADDR` defaults to `:<port>`, i.e.
     `0.0.0.0:<port>` — confirmed identically in all 19 services/*/cmd/server/main.go, e.g.
     services/execution-proxy/cmd/server/main.go:517-520,
     services/tool-governance/cmd/server/main.go:182-185,
     services/security-gate/cmd/server/main.go:228-230). In the Compose deployment this is safe
     in practice because docker-compose.yml only publishes host ports bound to one specific host
     IP (`${SCOS_TAILSCALE_IP:-100.93.75.87}:<port>:<port>`) and governance/internal services
     (memory-gateway, execution-proxy, tool-governance, security-gate, etc.) publish no host port
     at all in the production compose file — the Docker port-publish layer is the actual network
     boundary, not the process's own bind address. The native/systemd deployment path
     (deploy/native/) has no equivalent layer: deploy/native/env/*.env ships the same `:<port>`
     addresses (e.g. execution-proxy.env: SCOS_EXECUTION_PROXY_HTTP_ADDR=:8085,
     security-gate.env: SCOS_SECURITY_GATE_HTTP_ADDR=:8091), and neither deploy/native/install.sh
     nor deploy/native/README.md configures or even mentions a host firewall
     (`grep -n "firewall\|ufw\|iptables"` over both returns nothing). The README states "Upstream
     service URLs point at 127.0.0.1:<port>" (implying loopback-only reachability) but that
     statement describes only the outbound client URLs each service is configured to call, not
     what address each service's own listener binds — nothing enforces that assumption.
     - Impact: on a native/systemd install, execution-proxy (executes shell commands on behalf
       of agents), tool-governance, security-gate, memory-gateway, and every other
       "internal-only" service are reachable from any network the host is attached to (LAN, a
       misconfigured cloud NIC, a VPN peer, etc.) unless the operator manually adds firewall
       rules that the shipped tooling never prompts for. The bearer-token/TLS requirements
       (Finding 1's caveats aside) still gate access, so this is a network-exposure widening
       rather than a bypass of authentication by itself, but it removes the second layer of
       defense-in-depth the Compose path relies on and silently contradicts the "every governed
       service is reachable only inside the compose network" invariant documented in
       docker-compose.override.dev.yml:68-70 for the one deployment path where the analogous
       comment does not appear.
     - This matches the skill's own learned heuristic almost exactly: "Inspect container port
       publication, bind addresses, network aliases, and ingress topology before accepting any
       internal-only assumption. A private process is not private when deployment configuration
       publishes it" — here inverted: a process is not private when the deployment configuration
       *fails to restrict* it, and the doc's own phrasing invites that assumption.
     - Mitigation direction: default `SCOS_*_HTTP_ADDR` to `127.0.0.1:<port>` in
       deploy/native/env/*.env for every service that has no legitimate reason to accept
       non-loopback connections (all but the systemd host's own mcp-gateway instance, if one is
       ever run natively), or add an explicit firewall step to deploy/native/install.sh /
       README.md.

Suspicions requiring validation:
  - Whether any deployment actually runs mcp-gateway on the native/systemd path (Finding 2) —
    the native README and env files include mcp-gateway.env with the same `:8098` pattern, so
    the same exposure applies to the one service that is *supposed* to be externally reachable,
    which is lower severity for that specific service but still means the bind address is not
    doing the restricting there either — TLS + auth are the only controls, same as Compose.
  - Whether operators in practice are told anywhere *not* to hand out the plain
    SCOS_INTERNAL_SERVICE_TOKEN to external clients (Finding 1) — docs/agent-connection-guide.md
    was the only operator-facing doc read in this pass; a fuller sweep of docs/ (e.g.
    docs/security-configuration.md, GOVERNANCE.md §24.5 referenced in mcp-gateway's own comment)
    might contain an explicit warning against it that would downgrade this finding's severity.
    Not confirmed either way in this pass.
  - The exact production/staging boundary for SCOS_TAILSCALE_IP's hardcoded literal default
    (100.93.75.87) in docker-compose.yml — a specific IP address checked into version control as
    the fallback for every port-publish line. Fails closed (Docker refuses to bind if that IP
    isn't present on the host) rather than falling open, so this was not raised as a finding, but
    it is an unusual pattern worth a operator-facing comment confirming intent if not already
    present elsewhere in the repo.

Tests/commands executed:
  - find/grep inventory of deploy/compose, deploy/native/env, services/*/cmd/server/main.go
  - grep -rn "SCOS_INTERNAL_SERVICE_TOKEN|SCOS_TLS_REQUIRED|SCOS_SECURITY_ISSUER_SECRET|
    SCOS_CONTROL_PLANE_TOKEN" across packages/ (non-test) to find every enforcement point
  - grep -rln "database.RequireTLS|RequireTLS(" across services/ to build the TLS-enforcement
    coverage matrix, then manually traced the 3 apparent gaps (backup-manager, execution-proxy,
    observability) to their actual DB-access code paths and closed 2 of them
    (packages/service-state.New calls RequireTLS internally)
  - grep -n "HTTP_ADDR" across every main.go to enumerate every listen-address default
  - grep -n "firewall|ufw|iptables|0.0.0.0|127.0.0.1|bind" over deploy/native/README.md and
    install.sh (returned no firewall automation)
  - grep -rln "X-SCOS-Tenant-ID" across services/ to confirm how widely the header-fallback
    identity is trusted downstream
  - Read deploy/Dockerfile in full to confirm non-root USER and pinned base images (no finding —
    both correctly configured)
  - No live docker compose / systemd runs were performed (static analysis only)

Coverage gaps:
  - Did not read every line of every one of the 19 main.go files — targeted grep + read of the
    security-relevant sections (listener setup, auth wiring, TLS/DB config, and any file flagged
    by a prior grep hit). Business-logic-only code paths inside each service were not fully read.
  - Did not read GOVERNANCE.md §24.5 or docs/security-configuration.md, both referenced by
    in-repo comments as authoritative — a fuller read might change confidence on Finding 1's
    "documented as valid" framing (Suspicions, above).
  - Did not inspect the Python workers (consolidation-worker, embeddings-worker,
    index-maintenance-worker, policy-evaluation-worker) beyond their compose env blocks — no
    main.go equivalent existed to read per the assigned scope, and their env vars in
    docker-compose.yml were reviewed but the Python source itself was not.
  - Did not inspect database/policies/*.sql (RLS definitions) directly to confirm exactly how
    scos.tenant_id is consumed relative to the X-SCOS-Tenant-ID header chain described in
    Finding 1 — that trace stopped at "known context" plus the Go-side header propagation; the
    SQL-side consumption is assumed per the task's stated background, not independently verified
    in this pass.
  - Did not attempt to actually start the stack (compose or native) to observe real bind
    behavior (e.g. `ss -tlnp` after a native install) — all binding conclusions are from static
    reading of net.Listen/http.ListenAndServe argument construction, which is standard Go
    semantics for an address with no host part (":8085" binds all interfaces) but was not
    empirically re-verified here.
  - Did not cross-check every one of the 14 services reading X-SCOS-Tenant-ID to confirm each
    one treats it identically (i.e. that none of them additionally re-validate it against a
    signed claim before using it) — the grep confirms the header is read in each file, not the
    full trust logic in each.

Areas explicitly not reviewed:
  - Application-level business logic inside each service (memory ingestion, retrieval ranking,
    consolidation/synthesis heuristics, code-intelligence indexing) — out of this auditor's
    config-focused scope.
  - CI/CD pipeline configuration (no CI YAML was located under the assigned scope hint; not
    searched for in this pass).
  - Frontend/dashboard static asset (dashboard/index.html) contents beyond the
    SCOS_DASHBOARD_EDIT_PASSPHRASE gating already covered in transparency-projection's main.go.
  - database/migrations/*.sql schema correctness (persistence-auditor territory).

Recommended follow-up:
  - hostile-authorization-auditor should independently verify Finding 1 (mcp-gateway identity
    fallback) end-to-end against the actual RLS policies in database/policies/, since this
    auditor stopped at the Go-side header propagation per the config-auditor's scope boundary.
  - hostile-authorization-auditor or hostile-exploit-chain-auditor should assess whether Finding
    1 is independently exploitable today (i.e. whether the plain SCOS_INTERNAL_SERVICE_TOKEN is
    ever actually distributed to an external/lower-trust party in a real deployment, versus only
    ever used service-to-service) — this determines whether it escalates from a
    configuration-hygiene gap to an actively exploitable cross-tenant read/write path.
  - Whoever owns deploy/native/ should decide whether Finding 2's fix (default `127.0.0.1` binds,
    or an added firewall step) is appropriate, given the stated design goal of the native path
    (a single trusted host) — if the native deployment target is genuinely always a
    single-tenant, network-isolated Pi with no other listeners, the risk may be accepted
    deliberately; that intent was not stated anywhere in deploy/native/README.md and should be
    made explicit either way.
```
