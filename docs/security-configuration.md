# Security Configuration

## Compose startup requirements

The base Compose file intentionally has no secret defaults. Before running
`docker compose`, provide these required variables in the deployment environment
or `.env` (never commit their values):

- `SCOS_INTERNAL_SERVICE_TOKEN`
- `SCOS_CONTROL_PLANE_TOKEN`
- `SCOS_SECURITY_ISSUER_SECRET`
- `POSTGRES_PASSWORD`
- `SCOS_DATABASE_URL`, using `sslmode=require`, `verify-ca`, or `verify-full`
- either `SCOS_AGENT_ENDPOINT`, or `DEEPSEEK_API_KEY` with
  `DEEPSEEK_MODEL=deepseek-v4-flash` for the built-in DeepSeek dispatcher

`POSTGRES_USER` and `POSTGRES_DB` default to `scos` and `scos_memory` for local
development. Keep the password in `POSTGRES_PASSWORD` synchronized with the
password in `SCOS_DATABASE_URL`.

The `security-gate` service requires `SCOS_SECURITY_ISSUER_SECRET` and
`SCOS_SECURITY_GATE_STORE_PATH` at startup. The store path must be in a writable
directory; it contains token metadata, audit records, and secret references
(never secret values).
Provide it through the deployment environment or a secret manager; do not commit
the value to `.env.example`, Compose files, logs, or source control.

Every HTTP service also requires the shared internal bearer credential
`SCOS_INTERNAL_SERVICE_TOKEN` at startup. Send it as
`Authorization: Bearer <value>` for service-to-service calls. Set
`SCOS_SERVICE_IDENTITY` to the stable service identity; it is attached by the
middleware and caller-supplied identity headers are not trusted.

Governed services additionally require `SCOS_CONTROL_PLANE_TOKEN` in the
`X-SCOS-Control-Plane-Token` header. Only the autonomous control plane sends
this header; direct calls to governed service routes are rejected even when a
shared bearer is supplied. Compose keeps those service ports internal and
publishes only the control-plane ingress.

The Compose deployment validates the issuer secret before creating the service.
The readiness endpoint verifies both configuration values and store
availability, without returning the secret or a derived value.

### Securestore secret-reference provider (optional)

`security-gate` supports a `securestore` provider for `SecretReference`
entries (§27: memory retains only a reference, never the value). A
`securestore` reference's `ref` field stores the credential's Securestore
`credentialId`. To enable resolution, set:

- `SCOS_SECURESTORE_URL` — base URL of the Securestore deployment, e.g.
  `https://securestore:8443`
- `SCOS_SECURESTORE_AGENT_API_KEY` — the `sss_...` agent API key sent as
  `Authorization: Bearer <key>` to Securestore
- `SCOS_SECURESTORE_WORKSPACE_ID` — optional; sent as
  `X-SecureStore-Workspace-Id` when set

These are optional. If `SCOS_SECURESTORE_URL` is unset, `POST
/v1/secrets/{id}/resolve` fails closed with `503 securestore provider not
configured` rather than silently no-oping. When configured, resolution calls
Securestore's `GET /v1/secrets/:credentialId` on every request — the value is
never cached or persisted — and fails closed (`502` if Securestore is
unreachable or returns a non-2xx response, `503` if Securestore reports
`approval_required`). Every successful resolution is audited with
`Action: restricted_retrieval`, matching the audit trail security-gate keeps
for all other restricted reads.

Resolution is authorised against the reference before Securestore is
contacted. Each reference stores a `scope` and a `data_class`:

- `scope` is a comma- or space-separated list of `tenant` (any authenticated
  principal of the owning tenant), `principal:<id>` and `agent:<id>`
  selectors, matched against the caller's signed claims (`principal_id`,
  `agent_id`). An empty scope makes the reference unresolvable; any other
  selector is rejected with `400` when the reference is written.
- `data_class` is one of `public`, `internal`, `confidential`, `restricted`,
  `secret` and defaults to `secret` (migration 000049 gives existing
  references `secret`). The caller's claims must carry a
  `data_class:<class>` scope at or above it; the `admin` scope clears every
  class.

A caller outside the scope, under-cleared, or without signed claims gets
`403` and the refusal is audited (`Result: denied`, with the reference's data
class and the caller's agent and trace id from its claims).

`POST /v1/audit/record` cannot assert audited facts: the entry is attributed
to the authenticated caller and stored as action `caller_annotation`,
decision `recorded`. The caller's `action`, `result`, `data_class`, `reason`,
`session_id` and `trace_id` are stored and returned only under
`caller_annotation`; a `tenant`, `principal` or `agent_id` in the body that
differs from the authenticated identity is refused with `403`.

### Credential vault key versioning

`credential-vault` seals every value with AES-256-GCM and records the
`key_version` it was sealed under. Its keyring is configured with:

- `SCOS_CREDENTIAL_VAULT_MASTER_KEY` — current key, base64 of 32 bytes
- `SCOS_CREDENTIAL_VAULT_KEY_VERSION` — version of the current key
  (default `1`, matching values stored before versioning existed)
- `SCOS_CREDENTIAL_VAULT_PREVIOUS_KEYS` — retired keys still needed for
  reading, as comma-separated `<version>:<base64 key>` entries

To rotate: move the current key into `SCOS_CREDENTIAL_VAULT_PREVIOUS_KEYS`,
set a new master key with a higher version, restart the service, then call
`POST /v1/admin/keys/rotate` once per tenant with a claims token carrying the
`credentials:admin` (or `admin`) scope. Every value is opened with the key for
its recorded version and re-sealed under the current one in a single
transaction; each re-seal is audited as `credential_key_rotated`. If any
stored version has no configured key, nothing is re-sealed and the call
returns `409` naming the missing version. `GET /v1/admin/keys` reports the
per-version counts; remove a previous key only after every tenant reports
`pending_rotation: 0`.

Generate a high-entropy value with a local secret generator, for example:

```sh
openssl rand -base64 32
```

PostgreSQL connection URLs must use `sslmode=require`, `verify-ca`, or
`verify-full`. Compose does not publish database or service ports by default;
put an authenticated TLS reverse proxy in front of an explicitly exposed
service when host access is required. Passwords and backup keys must be provided
by the deployment secret manager. The base Compose file does not pass a raw
`SCOS_BACKUP_ENCRYPTION_KEY`; configure `SCOS_SECRET_MANAGER` and
`SCOS_BACKUP_ENCRYPTION_KEY_REF` so the backup manager can obtain the key at
runtime.

For certificate verification, mount the deployment CA bundle read-only into
each database client container and reference it in `SCOS_DATABASE_URL`, for
example:

```text
postgresql://scos@postgres:5432/scos_memory?sslmode=verify-full&sslrootcert=/run/secrets/postgres-ca.crt
```

The `/run/secrets/postgres-ca.crt` file must be supplied by the orchestrator
(Docker/Kubernetes secret or an equivalent secret-manager volume); do not put
certificate material in this repository. For mutual TLS, mount the client
certificate and key as separate read-only secrets and add
`sslcert=/run/secrets/postgres-client.crt&sslkey=/run/secrets/postgres-client.key`
to the URL. The PostgreSQL container itself must receive its server
certificate, key, and CA through secret-backed mounts and be configured with
`ssl=on`; keep the key mode restricted to the database process. A CA mount is
not a substitute for `sslmode=verify-ca` or `verify-full`.
