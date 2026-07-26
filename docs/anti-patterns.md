# Governing prohibitions (§42 reference)

The following patterns are explicitly prohibited in the SCOS Memory Fabric.
This list serves as a quick-reference guide derived from §42 of GOVERNANCE.md.

## Data & persistence

- Writing every reply directly into semantic memory
- Allowing unsupported shared-memory mutation
- Embedding secrets in memory
- Making embeddings the only copy of data
- Deleting history during correction
- Signalling completion before commit
- Weakening durability settings for benchmarks

## Security & access

- Giving agents database-owner credentials
- Mixing private and shared scopes
- Treating vector similarity as authority
- Allowing retrieved documents to override policy
- Persisting hidden reasoning
- Feeding entire histories into every prompt

## Architecture & infrastructure

- Adding infrastructure because its diagram looks impressive
- Calling untested backups complete
- Requiring agents to remember to call memory for normal continuity
- Making core optimisation systems dependent on external hosted services
- Using arbitrary fixed-size chunks when semantic code boundaries exist
- Serving partially committed or cross-branch code indexes

## Autonomy & routing

- Allowing supported ingress to bypass the Autonomous Cognitive Control Plane
- Allowing a model to grant itself permissions or select prohibited tools
- Treating a single semantic classifier as sufficient authority for high-impact routing
- Running post-turn persistence only when requested by the agent
- Hiding autonomous route decisions or making them impossible to replay
- Silently performing confirmation-gated side effects under the label of autonomy

## Proxying & execution

- Rewriting unknown or destructive commands speculatively
- Allowing the adaptive proxy to bypass authority, confirmation, idempotency or audit
- Letting learned policy modify live executable rules without governed promotion

## Compression & optimisation

- Dropping governing instructions, exact values, evidence, errors or warnings during compression
- Creating irreversible summaries without restoration handles
- Optimising token count at the expense of task success or required detail
- Replacing evidence with agent summaries

## Extensions

- Installing extensions without manifest, permission review, compatibility check and rollback path
- Treating skills as unversioned prompt fragments
- Allowing hooks to run with undeclared authority or unlimited blocking time

## Compatibility

- Claiming compatibility with an agent that exposes no tested accessible boundary
- Requiring Hermes to call an explicit memory tool for ordinary recall or persistence

## Transparency

- Displaying fabricated dashboard metrics, graph nodes, live events or user knowledge
- Calling hidden chain-of-thought operational transparency
- Hiding which memories, plugins, skills, hooks, agents or tools influenced a response
- Preventing an authorised user from inspecting or correcting eligible user-model entries
- Conflating inferred user traits with explicit facts
- Promoting perspective-specific beliefs into global truth without evidence

## Evaluation

- Claiming superiority without reproducible comparative evidence
- Cherry-picked demonstrations counting as evidence
