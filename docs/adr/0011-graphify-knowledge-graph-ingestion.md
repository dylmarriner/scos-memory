# ADR-0011: graphify is ingested as a fact/relation source, not a parallel memory store

- **Status:** Accepted
- **Date:** 2026-08-22
- **Decision owner:** Dylan Marriner

## Context

[graphify](https://github.com/Graphify-Labs/graphify) parses a codebase and
its docs into a local knowledge graph (`graph.json`), tagging every edge
`EXTRACTED` (explicit in source) or `INFERRED`/`AMBIGUOUS` (resolved by
graphify). We want its output recallable through the normal `scos_recall`
path, and its own `query`/`path`/`explain` traversal exposed as MCP tools,
without introducing a second, ungoverned graph store alongside
`memory_items`/`memory_relations` and without giving a new component a
backdoor into the database (§4 authority rules: postgres is the sole
authority; every write goes through a service's own HTTP API, never direct
SQL from another service).

## Decision

A new peer service, `services/graphify-provider`, owns the `graphify` CLI
subprocess and has no database access of its own:

- `POST /v1/graphify/extract` runs `graphify extract <path> --code-only`
  (local tree-sitter AST only, no API key, per graphify's headless/CI mode)
  and, for each graph node, calls memory-intelligence's existing
  `POST /v1/facts` with `network: "entity"` and `source: "explicit"` —
  graphify only tags *edges*, not nodes, with EXTRACTED/INFERRED/AMBIGUOUS
  confidence (a node either exists in the parsed source or isn't emitted),
  so every node maps to the existing `Fact.Source` value `explicit`. For
  each edge it calls the (now dual GET/POST) `POST /v1/relations` endpoint
  with `relation_type` `extracted_from` (edge confidence `EXTRACTED`) or
  `inferred_from` (`INFERRED`/`AMBIGUOUS`), targeting the existing
  `memory_relations` table.
- `POST /v1/graphify/query|path|explain` are thin subprocess wrappers
  around graphify's own read commands, for direct graph traversal that
  doesn't belong in the fact/relation model (e.g. "what connects X to Y").
- `mcp-gateway` gained three tools — `scos_graphify_query`,
  `scos_graphify_path`, `scos_graphify_explain` — forwarding to
  graphify-provider exactly like `scos_memory_graph` forwards to
  memory-intelligence today: no new authority, just a new upstream.

Everything graphify writes lands as ordinary `memory_items`/
`memory_relations` rows with normal RLS, normal namespace scoping, and
normal recall — a graphify-derived fact looks like any other fact except
for `structured_content.derivation` (`graphify:node:<id>`), which preserves
provenance without a schema change.

## Consequences

- No new authoritative data model: `memory_items`/`memory_relations`
  remain the single graph of record; graphify is one more contributor to
  it, on the same footing as any other fact source.
- The ingesting principal needs a `namespace_memberships` row with
  `can_write_candidates=true` for its configured `GRAPHIFY_NAMESPACE_ID` —
  the same `memory_items_candidate_write` RLS policy every other writer
  goes through (`database/policies/000001_rls.sql`). No RLS change was
  needed for the new `POST /v1/relations` writes either: the existing
  `memory_relations_item_access` policy (`database/policies/000003_data_plane_rls.sql`)
  is `FOR ALL`, gated only on the referenced `memory_items` row being
  visible under that row's own SELECT policy — which the ingesting
  principal already needs in order to have created the node in the first
  place.
- Repeated extraction runs re-POST every node; idempotency is handled by
  `handleFacts`' existing contradiction/merge-dedup logic (`findMergeCandidate`),
  not by new code in graphify-provider.

## Compatibility

Additive only. No existing `Fact`, `memory_items`, or `memory_relations`
field changed meaning; `relation_type` values `extracted_from`/
`inferred_from` are new strings in an already-unconstrained `text` column.

## Migration

None. No new tables, columns, or RLS policies were required — see
Consequences above for why the existing `memory_relations_item_access`
policy already covers the new write path.

## Rollback

Stop calling `POST /v1/graphify/extract` (or revoke the ingesting
principal's `can_write_candidates`) to halt new ingestion. Already-ingested
facts are never hard-deleted — they follow the normal soft-delete path
(`forgetFact`/`handleFactsForgetAll`), exactly like any other memory.
Decommissioning graphify-provider itself is deleting one stateless service;
it holds no data of its own.

## Index rebuild

Not applicable — graphify-provider produces facts through the existing
`POST /v1/facts` path, so any derived index (embeddings, full-text) that
already covers `memory_items` covers graphify-derived facts too, with no
separate rebuild step.

## Verification

`go test ./services/graphify-provider/... ./services/memory-intelligence/... ./services/mcp-gateway/...`.
End-to-end: run `graphify-provider`'s `POST /v1/graphify/extract` for a
test tenant/namespace and confirm `memory_items` rows appear with
`structured_content.derivation` starting `graphify:node:` and
`memory_relations` rows with `relation_type` `extracted_from`/
`inferred_from`; confirm `scos_recall` and `scos_memory_graph` surface
them; call `scos_graphify_query`/`path`/`explain` through `mcp-gateway`'s
`/mcp` endpoint and confirm forwarding.

The `graph.json` parser (`services/graphify-provider/cmd/server/graphify.go`)
was verified against a real `graphify extract --code-only` run (graphifyy
0.9.48): the document is networkx node-link JSON — `{nodes, links}`, not
`{nodes, edges}` — and confidence (`EXTRACTED`/`INFERRED`/`AMBIGUOUS`) is an
edge-only field; nodes carry `source_file`/`source_location` (a string like
`"L17"`) but no confidence of their own.
