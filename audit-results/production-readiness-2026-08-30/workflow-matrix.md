# Workflow matrix

| Workflow | Main risk | Result |
|---|---|---|
| Cloud signup/login/session | brute force, CSRF, session lifecycle | auth exists; CSRF/rate-limit gap |
| API key to MCP | replay, scopes, forwarding | signed claims; runtime/rate-limit gap |
| Creator admin | role mutation and audit durability | server role check; best-effort audit |
| Team invite/accept | cap race and replay | lock fix applied; concurrent DB test needed |
| High-impact execution | classifier-derived approval and shell | inherited high-risk gap |
| Memory/RLS | cross-tenant persistence | live verification blocker |
| Deployment/startup | wildcard exposure and health | PR-001/002 fixed locally; render test pending |
