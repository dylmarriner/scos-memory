# Completeness passes — 2026-09-09

1. **Inputs to sinks:** source paths are represented by the data-flow and control-flow artifacts; current live reachability is unverified.
2. **State and business logic:** lifecycle, recovery, promotion, and idempotency are represented by the workflow/business-logic artifacts; replay under a live stack remains a required check.
3. **Dark corners:** scripts, deployment, integrations, generated artifacts, and legacy paths are represented by the dead-corner/configuration artifacts; generated `_to_delete/` content remains unrelated dirty work and was preserved.

New in this pass: documentation drift and missing current-turn specialist skill
files were recorded explicitly instead of silently treating old artifacts as a
fresh complete audit.
