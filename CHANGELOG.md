# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to adopt [Semantic Versioning](https://semver.org/)
once it cuts its first tagged release.

## [Unreleased]

### Added

- Native (Docker-free) deployment path: systemd units, native PostgreSQL +
  pgvector setup, and a build script covering the Go service fleet and the
  consolidation worker. See [`deploy/native/README.md`](deploy/native/README.md).
- Shared production-hardening packages: `packages/httpmid` (panic recovery,
  structured JSON request logging) and graceful shutdown in
  `packages/tlsserve`, rolled out to memory-gateway, security-gate, and
  autonomous-control-plane.
- CI: Go build/vet/test, Python compile/test/schema validation, and a
  supply-chain security job (govulncheck, pip-audit, gitleaks, Trivy
  container scan, CycloneDX SBOM).
- Community and governance files: `CONTRIBUTING.md`, `SECURITY.md`,
  `CODE_OF_CONDUCT.md`, issue forms, PR template, `CODEOWNERS`, Dependabot.
- `docs/security/threat-model.md`, `docs/security/incident-response.md`,
  and `docs/scaling.md` (honest gap analysis for horizontal scale-out).
- `LICENSE` (MIT).

### Changed

- Upgraded the Go toolchain from 1.22 (end-of-life) to 1.26.6, resolving the
  standard-library CVEs that could only be fixed by a major-version move.
- README and architecture documentation maintained as the primary project
  landing page, with technical claims separated from deployment-owned evidence.

### Fixed

- Two pre-existing false positives in `scripts/audit_governance.py`'s
  incomplete-work marker scan that caused `make audit` to fail on legitimate
  code (SQL bind-parameter comments using the word "placeholder").
- `gofmt` drift in several files, caught by extending CI's format check to
  `packages/` and `adapters/` in addition to `services/`.

<!--
When cutting the first tagged release, replace [Unreleased] above with
[0.1.0] - YYYY-MM-DD and add a new empty [Unreleased] section here.
-->
