# Critical/High candidate cross-review

| Candidate | Primary | Independent reviewer | Result |
|---|---|---|---|
| AUDIT-001 execution proxy shell sink and spoofable identity | data/control flow | authorization + configuration | Reachability from POST routes confirmed; no verifier found. Local-only binding reduces external reach but does not authenticate callers. |
| AUDIT-002 security authority arbitrary issuance | authentication | authorization + configuration | `/v1/auth/issue` accepts principal/tenant/expiry from JSON and has no issuer check; downstream deployment use remains unverified. |
| AUDIT-003 control-plane mutators without ownership/auth | authorization | workflow + error | Tool/extension handlers mutate state without principal/tenant checks; valid transitions still require only state, not actor. |
| AUDIT-004 execution-proxy disclosure and operation-ID overwrite | data flow | authorization + business logic | Result/history/raw restore routes have no identity binding; caller-supplied IDs overwrite map entries. |
| AUDIT-005 non-default database credential | configuration | dependency/deployment | Confirmed default `change-me`; port binding fix verified but credential remains weak. |

The security-gate mutex race suspicion was falsified: reviewed token and audit-log
map accesses use `s.mu`; it is not reported. No live reproduction was run;
severity is capped where deployment assumptions remain unverified.
