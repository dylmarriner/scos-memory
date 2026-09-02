# Invariant register — 2026-09-02

| Invariant | Current enforcement | Result |
|---|---|---|
| active snapshot is complete | non-empty client file list only | violated by design |
| symbol_count equals indexed symbols | `len(files)` assigned to both counts | violated |
| conceptual file identity persists | new `code_files` row each snapshot | violated |
| current graph matches source | fingerprinted cache key | improved; lock/atomic write unproven |
| tenant comes from verified identity | claims path; header fallback | partial |
| RLS cannot be bypassed by runtime role | policies only; no FORCE/app role migration | unproven |
| derived edges retain evidence | Graphify relation collapse | partial |
