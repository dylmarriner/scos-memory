# Invariant register

| Invariant | Where enforced | Alternate paths | Concurrency risk | Failure risk | Valid-input abuse tested | Result |
|---|---|---|---|---|---|---|
| identity cannot be self-selected | gateway assumption | every internal HTTP port | high | high | yes | contradicted for exposed services |
| shell execution requires authenticated authorization | request validation only | CLI/proxy aliases | high | medium | yes | not strongly enforced |
| tenant/principal bind durable access | SQL/RLS context | workers/admin services | medium | high | partial | partially enforced |
| operation completion is owned by initiator | none visible | complete/reconcile | high | medium | yes | unenforced |
| manifest signature proves content/issuer | non-empty fields | dry-run/install | medium | high | yes | unenforced cryptographically |

