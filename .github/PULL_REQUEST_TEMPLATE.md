## Summary

<!-- What does this change do, and why? -->

## Related

<!-- Link the relevant ADR, GOVERNANCE.md section, or issue, if any. -->

## Changes

<!-- Bullet the notable changes. -->

-

## Testing

<!-- How was this verified? Paste relevant command output if useful. -->

- [ ] `make fmt`
- [ ] `make test`
- [ ] `make validate`
- [ ] `make audit`
- [ ] `make failure-injection` (if this touches crash recovery, idempotency, or restarts)

## Checklist

- [ ] This change is scoped to one logical concern.
- [ ] New/changed services expose `/health/live` and `/health/ready`.
- [ ] Database access changes respect existing RLS policies.
- [ ] No secrets were added to `.env.example`, Compose files, fixtures, or logs.
- [ ] `indexes/CODE_INDEX.md` regenerated (`make code-index`) if services/packages changed.
