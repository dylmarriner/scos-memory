# Contributing

Thanks for considering a contribution to SCOS Memory Fabric. This project is
governed by a normative specification, not just conventions — read this file
before your first PR, it's short.

## Prerequisites

- Go 1.26+
- Python 3.11+
- Docker and Docker Compose
- `openssl` (for generating local secrets)

## Setup

```bash
git clone https://github.com/dylmarriner/scos-memory.git
cd scos-memory
cp .env.example .env
${EDITOR:-vi} .env   # set the tokens described in .env.example
set -a; . ./.env; set +a
docker compose -f deploy/compose/docker-compose.yml up --build
```

See the [Quick start](README.md#quick-start) in the README for the full sequence,
and [`docs/security-configuration.md`](docs/security-configuration.md) for what
each required variable does.

## Before you change architecture

Read [`GOVERNANCE.md`](GOVERNANCE.md). It is the authoritative specification —
services are audited against it, not against this file. If a change touches
persistence boundaries, service responsibilities, or interfaces, check whether
it needs a new [ADR](docs/adr/) before writing code.

## Development workflow

```bash
make fmt        # gofmt + Python compileall
make test       # unit tests: Go, Python workers, failure-injection unit runner
make validate   # JSON Schema + Compose config validation (requires .env sourced)
make audit      # governance compliance gate (must pass before calling a change done)
```

Run `make failure-injection` against a live Compose stack for changes that touch
crash recovery, idempotency, or service restart behavior — it's an integration
probe against real HTTP services and PostgreSQL, not a mock.

Regenerate the code index after adding or removing services/packages:

```bash
make code-index
```

## Coding standards

- Go: `gofmt`-clean, table-driven tests alongside the code they cover.
- Python: standard `unittest`, discoverable under each package's `tests/`.
- New services follow the existing pattern: `/health/live` and `/health/ready`
  endpoints, RLS-scoped database access, and an entry in
  [`indexes/CODE_INDEX.md`](indexes/CODE_INDEX.md) after `make code-index`.
- No secrets in `.env.example`, Compose files, fixtures, or logs — see
  [`SECURITY.md`](SECURITY.md).

## Commits and PRs

- Keep commits scoped and descriptive; explain *why*, not just *what*.
- Fill out the PR template — tests and `make audit` status are not optional
  checkboxes, they're what gets a PR reviewed quickly.
- Link the relevant ADR or `GOVERNANCE.md` section for anything touching
  architecture, persistence, or security boundaries.
- One logical change per PR. Large refactors are easier to review split by
  service or layer.

## Where to ask for help

Open a [discussion or issue](https://github.com/dylmarriner/scos-memory/issues)
if you're unsure whether a change fits the governance model before investing
time in it — that's cheaper than a large PR that needs architectural rework.
