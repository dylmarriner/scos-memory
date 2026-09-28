# SCOS / ARIS Memory Documentation

This directory contains the architecture, governance, deployment and security documentation for the durable ARIS Memory Fabric.

## Start here

- [ARIS Memory Fabric Technical Blueprint and Phased Roadmap](ARIS_MEMORY_TECHNICAL_BLUEPRINT_AND_ROADMAP.md)
- [ARIS system map: what goes where](ARIS_SYSTEM_MAP.md) (mirror of the canonical copy in `dylmarriner/ARIS`)
- [Architecture](architecture.md)
- [Security configuration](security-configuration.md)
- [Scaling](scaling.md)
- [Current governance audit](governance-audit-current.md)
- [Repository overview](../README.md)

## Canonical boundary

SCOS Memory is the governed durable-memory authority for ARIS: journal, episodic and semantic memory, provenance, retrieval, promotion, consolidation, OKF and memory recovery. It is not the global ARIS Executive or general task/tool orchestration layer, and it does not own prompt construction: it returns ranked evidence and Harness builds model context. See the [system map](ARIS_SYSTEM_MAP.md) for where every ARIS component belongs.

The cross-repository architecture is canonical in `dylmarriner/ARIS/docs/ARIS_MASTER_TECHNICAL_BLUEPRINT.md`. When older wording describes this repository as a broad cognitive control plane, interpret that control plane as memory-specific governance rather than global ARIS cognition.
