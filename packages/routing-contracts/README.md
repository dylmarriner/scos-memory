# Routing contracts

This package owns the model-neutral contracts used by the Autonomous Cognitive Control Plane.

Core contracts:

- canonical ingress event
- capability registration
- multi-label intent classification
- route decision
- execution plan
- stage checkpoint
- route outcome
- replay request

Agents consume a prepared execution envelope. They do not need to discover or call memory, code intelligence or the execution proxy themselves.
