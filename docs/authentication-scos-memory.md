# Service authentication

All non-health HTTP routes require an HMAC-signed bearer token. The token is
issued by `services/security-gate` only when the request supplies the configured
`X-SCOS-Issuer-Secret`; the issuer secret is never accepted from request JSON.

Required deployment secrets:

- `SCOS_AUTH_ISSUER_SECRET`: long random secret shared by the service boundary.
- `SCOS_EXTENSION_SIGNING_SECRET`: long random secret used to verify extension
  manifests.
- `POSTGRES_PASSWORD`: non-placeholder database password.

Create an identity token through `POST /v1/auth/issue` with both
`X-SCOS-Issuer-Secret` and the service-authenticated deployment boundary. Send
the returned token as `Authorization: Bearer <token>`. Tenant and principal
headers are derived from the verified token by middleware; caller-supplied
headers and body identity fields cannot override them.

Administrative tokens must carry explicit scopes such as `tools:admin`,
`operations:execute`, `operations:complete`, `operations:reconcile`, or
`extensions:install`. Scope-less authenticated identities cannot mutate those
control-plane surfaces.

Tokens expire after one hour, include a random identifier, and use HMAC-SHA256.
This is a shared-secret service boundary, not a replacement for an external
identity provider or key rotation system; rotate the secret through deployment
orchestration and invalidate old tokens when required.
