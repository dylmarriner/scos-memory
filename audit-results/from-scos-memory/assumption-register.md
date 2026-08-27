# Assumption register

| Assumption | Classification | Evidence/next action |
|---|---|---|
| internal HTTP callers are trusted | Contradicted | compose publishes ports; centralize auth or restrict exposure |
| identity headers were authenticated upstream | Convention only | no verifier in several services; require signed identity |
| operation IDs are unique and caller-bound | Unenforced | caller chooses IDs and maps key only by ID |
| database RLS context is set on every transaction | Partially enforced | inspect all DB paths/workers |
| non-empty signature/hash means valid manifest | Contradicted | no cryptographic verification in install handler |
| tests cover security boundaries | Unenforced | add endpoint negative tests |

