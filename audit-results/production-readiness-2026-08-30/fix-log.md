# Fix log

- PR-001: added `SCOS_CLOUD_BIND_ADDRESS` with loopback default in Compose and `.env.example`.
- PR-002: changed cloud readiness probe from 8099 to the actual 8079 listener.
- PR-004: locked the team row before capacity checks in invite and accept flows; invitation row is locked during acceptance.
- Remaining fixes are intentionally not guessed: CSRF, RLS role design, independent approval, durable state, and idempotency need product/runtime decisions and integration coverage.
