# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately using
[GitHub Security Advisories](https://github.com/dylmarriner/scos-memory/security/advisories/new)
rather than filing a public issue.

Include:

- A description of the vulnerability and its impact.
- Steps to reproduce, or a proof of concept.
- The affected service(s) or component(s), if known.

You should receive an initial response within a few days. Please allow time
for a fix before any public disclosure.

## Scope

In scope: the services under `services/`, `packages/`, `workers/`, `adapters/`,
the `deploy/` Compose stack, and the database migrations/RLS policies under
`database/`.

Out of scope: vulnerabilities that require an attacker to already possess a
valid `SCOS_INTERNAL_SERVICE_TOKEN` or `SCOS_CONTROL_PLANE_TOKEN` and are
solely about actions those tokens are documented to authorize.

## Supported configuration

This project has no secure-by-default secrets. Deployments **must** set their
own values for `SCOS_INTERNAL_SERVICE_TOKEN`, `SCOS_CONTROL_PLANE_TOKEN`,
`SCOS_SECURITY_ISSUER_SECRET`, and `POSTGRES_PASSWORD` before startup — the
base Compose file will not start without them. See
[`docs/security-configuration.md`](docs/security-configuration.md) for the
full configuration surface, including TLS mode requirements for
`SCOS_DATABASE_URL` and the row-level-security model enforced at the
database layer.

Running the stack with placeholder or example secrets, or with
`sslmode=disable`, is not a supported or secure configuration.
