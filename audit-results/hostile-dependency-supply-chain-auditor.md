# Specialist audit artifact

```text
Skill: hostile-dependency-supply-chain-auditor
Agent: hostile-dependency-supply-chain-auditor-workflow-agent

Scope:
  Dependency provenance and build-time code execution across the whole
  repository, with focused attention on:
    - services/extension-manager extension install signature verification
      path vs. the packages/extension-contracts VerifySignature helper.
    - go.mod / go.sum and package.json version currency and lockfile
      integrity.
    - Makefile, scripts/, and every Dockerfile for build-time execution of
      untrusted or unpinned code (install hooks, floating tags, network
      fetches).

Files/components inspected:
  - go.mod, go.sum (module graph, checksums, toolchain pin)
  - package.json (no package-lock.json diff needed; dependency unused)
  - Makefile (all targets: fmt, test, integration-test, failure-injection,
    validate, audit, audit-runtime, compose-up/down, db-shell)
  - scripts/install_agent_plugins.py, scripts/generate_code_index.py,
    scripts/audit_governance.py, scripts/validate_json.py
  - deploy/Dockerfile (single shared image for all 19 Go services +
    graphify-provider's Python/pipx layer)
  - workers/embeddings/Dockerfile, workers/consolidation/Dockerfile,
    workers/index-maintenance/Dockerfile, workers/policy-evaluation/Dockerfile
  - packages/extension-contracts/extensions.go,
    packages/extension-contracts/extensions_test.go
  - services/extension-manager/cmd/server/main.go (manifest validation,
    install, dry-run, signature verification, isolation/version
    compatibility)
  - services/extension-manager/cmd/server/main_test.go,
    services/extension-manager/cmd/server/canary_test.go (signature test
    fixtures only, read for corroboration)
  - services/graphify-provider/cmd/server/main.go,
    services/graphify-provider/cmd/server/graphify.go (subprocess exec of
    the `graphify` CLI installed via pipx in the shared Dockerfile)
  - services/mcp-gateway/cmd/server/main.go (routing/exposure of
    scos_extension_install and scos_extension_install_dryrun to external
    MCP clients)
  - docs/adr/0004-extension-signing-and-isolation.md,
    docs/adr/0011-graphify-knowledge-graph-ingestion.md
  - deploy/compose/docker-compose.yml (env wiring for extension-manager,
    absence of SCOS_EXTENSION_SIGNING_PUBLIC_KEY)

Entry points inspected:
  - POST /v1/extensions/install and /v1/install (services/extension-manager,
    main.go:230-231, handleInstall at main.go:779) — reachable externally
    via mcp-gateway's scos_extension_install tool (services/mcp-gateway/
    cmd/server/main.go:1453, forwarded at main.go:462).
  - POST /v1/install/dryrun (handleInstallDryRun, main.go:878) — reachable
    via scos_extension_install_dryrun (mcp-gateway main.go:1474, forwarded
    at main.go:464). Dry-run performs no signature check (only
    validateManifest, main.go:892), by design.
  - `make` targets as CI/local build entry points.
  - `docker build` / `docker compose --build` via deploy/Dockerfile.
  - `python3 scripts/install_agent_plugins.py` (local developer/user
    machine install script — writes MCP config files and shells out to
    `gemini` and `codex` CLIs).

Important callers/callees:
  - extension-manager handleInstall -> validateInstallManifest (main.go:1001)
    -> verifySignature (main.go:1025, real Ed25519 check) -> determineIsolation
    -> compatibleVersion -> registerExtensionLocked -> persist.
  - packages/extension-contracts.Manifest.VerifySignature (extensions.go:57,
    sha256-based) is called only from its own test
    (extensions_test.go:42) — grep across the whole repo confirms zero
    production callers.
  - deploy/Dockerfile build stage 2 -> `pipx install graphifyy` (unpinned)
    -> binary landed on PATH as `graphify` -> exec'd by
    graphify-provider's runGraphify (graphify.go:19,
    exec.CommandContext(ctx, "graphify", args...), argv-only, no shell).

Workflows traced:
  1. Extension install via mcp-gateway -> extension-manager
     (POST /v1/extensions/install): manifest JSON in, `verifySignature`
     gates on Ed25519 signature over the JSON-marshaled manifest with
     Signature field cleared, keyed by SCOS_EXTENSION_SIGNING_PUBLIC_KEY.
     Confirmed this is the only enforced signature check on the install
     path; the sha256 VerifySignature in extension-contracts is not part
     of this workflow at all.
  2. Docker image build (deploy/Dockerfile): `go mod download` (checksum
     verified against go.sum/GOSUMDB, no GONOSUMCHECK/GOSUMDB=off found
     anywhere in the repo) -> `go build` all 19 services -> second stage
     `apk add` (pinned by `alpine:3.21` base tag) -> `pipx install
     graphifyy` (NOT pinned to a version, NOT hash-locked) -> chown/adduser
     -> USER scos drops privilege only after the pipx install has already
     run as root.
  3. graphify-provider runtime: exec's whatever `graphify` binary pipx
     installed at image-build time, against `s.repoPath` (server-
     configured via env, not attacker-controlled per request) — so the
     supply-chain exposure is at build time (image contents), not at
     request time.
  4. Local developer install (scripts/install_agent_plugins.py): copies
     repo-tracked files into the invoking user's home directory config
     paths and shells out to `gemini extensions link ...` and `codex
     plugin add scos-memory-fabric@scos-fabric-tools`. Both external
     commands are fixed argv lists (no string interpolation of untrusted
     input), and all file writes are backed up before being overwritten.

Trust boundaries:
  - PyPI package registry (graphifyy) trusted at Docker build time with no
    pin — a compromised/republished package version is fetched and its
    setup/entry-point code executed as root during every image rebuild.
  - Go module proxy/sum database (go.sum present, no override found) —
    integrity enforced by default toolchain behavior.
  - npm registry for @genkit-ai/mcp — dependency declared but no source
    reference found; a lockfile (package-lock.json) exists so resolution
    is at least pinned, even though the dependency itself is unused.
  - Extension manifest submitted over the MCP gateway from an external
    Claude Code/Desktop client is attacker-influenced input to
    extension-manager; gated by Ed25519 verification requiring possession
    of the private key matching SCOS_EXTENSION_SIGNING_PUBLIC_KEY.
  - Developer's home directory / local machine (install_agent_plugins.py)
    — trusted operator-run script, not attacker-reachable over the
    network in this repo's model.

Attacker-controlled inputs:
  - Extension manifest JSON body to /v1/extensions/install and
    /v1/install/dryrun (fields: PackageID, Version, Publisher, Signature,
    HookEndpoints, Dependencies, etc.) — gated by verifySignature for the
    non-dry-run path.
  - PyPI package content for `graphifyy` is not attacker-controlled today,
    but the absence of a version pin means a *future* malicious publish
    (typosquat takeover, maintainer account compromise, or a legitimate
    but backdoored release) becomes attacker-controlled build input with
    no version gate to catch it.

Invariants tested:
  - "Only the real (Ed25519) signature check gates extension install" —
    verified true; the sha256 VerifySignature helper in
    extension-contracts is unreachable from any HTTP handler or workflow.
  - "All third-party code executed at build time is pinned to an exact,
    reviewable version" — TRUE for Go (go.sum + toolchain pin + pinned
    golang:1.26.6-alpine and alpine:3.21 base images) and TRUE for the
    Python workers (version-range pins in pip install lines, though not
    hash-locked) — FALSE for `pipx install graphifyy` in deploy/Dockerfile,
    which has no version constraint at all.
  - "Extension install fails closed without a configured signing key" —
    verified true (verifySignature returns false when
    SCOS_EXTENSION_SIGNING_PUBLIC_KEY is unset or malformed, main.go:1026-
    1029); confirmed deploy/compose/docker-compose.yml does not set this
    variable for extension-manager, so installs fail closed by default in
    the shipped compose file.

Findings:
  1. [Medium] Unpinned PyPI dependency executed as root during Docker
     image build — deploy/Dockerfile:65 (`pipx install graphifyy`, no
     version/hash pin), inside the second build stage which still runs as
     root (USER scos is set later, at line 99). Every image rebuild
     silently pulls whatever the latest `graphifyy` release is on PyPI at
     build time, with no version pin and no hash pin, even though the
     codebase's own comments (graphify.go:43-46, docs/adr/0011:109-110,
     graphify_test.go:6) record that the wire format and behavior were
     verified specifically against graphifyy 0.9.48. A compromised
     maintainer account, a typosquat/dependency-confusion publish under
     a similar name, or simply an incompatible upstream release would be
     picked up automatically and its code would run with root privileges
     during the build, and its resulting CLI binary is later exec'd by
     graphify-provider at runtime (services/graphify-provider/cmd/server/
     graphify.go:19) against tenant-derived repository content. This is a
     genuine build-time supply-chain gap distinguishable from the go.mod/
     go.sum dependencies, which are all exact-pinned and checksum-verified.

  2. [Low / Hardening] Dead, non-cryptographic "signature" helper left in
     a shared contracts package — packages/extension-contracts/
     extensions.go:56-65. `Manifest.VerifySignature` computes
     `sha256(package_id:version:publisher:publicKey)` and compares it to
     `m.Signature` — this is not a real digital signature (no private key
     is ever involved; anyone who can read the manifest fields and knows
     the "publicKey" string, which is not actually secret since it is
     meant to be published as a public key, can compute a valid
     "signature" themselves). It is confirmed unreachable from any
     production code path (only extensions_test.go calls it); the actual
     install-time gate in services/extension-manager/cmd/server/
     main.go:1025 is a correct, real Ed25519 verification unrelated to
     this helper. Risk is latent: the misleading name and location
     (a shared contracts package other services could plausibly import)
     make it easy for a future change to wire this helper into a real
     enforcement path by mistake, silently downgrading extension install
     security from asymmetric-signature-verified to
     "guessable-hash-of-public-metadata"-verified. Recommend deleting it
     or renaming/documenting it clearly as non-cryptographic/test-only,
     and citing the real Ed25519 check as the canonical implementation.

  3. [Info / Hygiene] Unused npm dependency — package.json:3 declares
     `@genkit-ai/mcp` (^1.40.1) but no source file in the repository
     (searched *.ts/*.js outside node_modules) references it. A
     package-lock.json exists so the resolved version is at least pinned,
     but the dependency (and its transitive tree) is unnecessary attack
     surface with no offsetting benefit. Low severity since nothing
     appears to `require`/`import` it, but worth removing or explaining
     why it is kept.

  4. [Info / Hardening] Python worker images pin dependencies only by
     version range, not by hash lock — workers/embeddings/Dockerfile:11-14
     and workers/consolidation/Dockerfile:11 use `pip install
     "package>=X,<Y"` with no `--require-hashes` / pip-compile-generated
     lockfile. This is far less severe than finding 1 (ranges still bound
     what can be pulled, and `--no-cache-dir` avoids stale cache
     resurrection) but is a reproducibility/integrity gap versus the Go
     side of the repo, which is fully checksum-locked via go.sum.

Suspicions requiring validation (outside this lane, flagged for
cross-reference):
  - services/mcp-gateway/cmd/server/main.go:462-464 forwards
    scos_extension_install / scos_extension_install_dryrun directly to
    extension-manager with no visible call to scos_permission_check /
    scos_permission_classify beforehand in the switch arms inspected.
    Whether *any* authenticated MCP caller (vs. an admin-scoped one) can
    invoke a real extension install (subject only to possessing a
    correctly-signed manifest) is an authorization-boundary question,
    not a supply-chain one — recommend hostile-authorization-auditor
    confirm whether tool-level scoping exists elsewhere in the gateway or
    in tool-governance that was not visible from this file alone.

Tests/commands executed:
  - grep -rn "VerifySignature" --include="*.go" . — confirmed the sha256
    helper's only caller is its own unit test.
  - grep -n "Ed25519|ed25519|Signature|VerifySignature|PublicKey"
    services/extension-manager/cmd/server/main.go — confirmed the real
    Ed25519 gate and its exact line numbers.
  - grep -n "SCOS_EXTENSION_SIGNING_PUBLIC_KEY" -r . — confirmed the env
    var is referenced only in extension-manager source and its tests, and
    is absent from deploy/compose/docker-compose.yml (fail-closed by
    default).
  - grep -n "replace|exclude" go.mod; find . -iname vendor -maxdepth 2;
    find . -iname "*.patch" — confirmed no replace directives, no
    vendored tree, no patch files.
  - grep -rn "GONOSUMCHECK|GOFLAGS|GOPROXY|GONOSUMDB|GOSUMDB|GOPRIVATE" —
    confirmed no checksum-verification bypass anywhere in the repo.
  - grep -rln "curl \|wget \|pip install\|npm install\|subprocess\|
    os.system\|eval(\|exec(" scripts/ — reviewed the two matching scripts
    (install_agent_plugins.py, generate_code_index.py); no unsafe
    interpolation or network fetch of remote code found.
  - Read every Dockerfile in the repo (deploy/Dockerfile, and one per
    Python worker) in full.
  - grep -rn "graphifyy|graphify" across services/graphify-provider,
    deploy/Dockerfile, and docs/adr/0011 to confirm the version-pin
    mismatch (code assumes 0.9.48; Dockerfile installs unpinned).

Coverage gaps:
  - Did not build the Docker image or run `pipx install graphifyy` to
    observe which actual version resolves today, or whether it still
    matches the {nodes, links} wire format the code assumes — this audit
    is static/evidence-based only, not a runtime reproduction.
  - Did not inspect CI workflow files (e.g. GitHub Actions YAML) if any
    exist outside the paths grepped here; none were found under a
    standard `.github/workflows` search during this pass, but a dedicated
    CI-configuration sweep was not exhaustively performed.
  - Did not fully audit workers/index-maintenance/Dockerfile and
    workers/policy-evaluation/Dockerfile line-by-line (both share the
    same pip-install-with-range pattern seen in embeddings/consolidation
    per a filename-level check, but were not individually transcribed
    into this artifact).
  - Did not verify whether `codex plugin add
    scos-memory-fabric@scos-fabric-tools` (invoked by
    install_agent_plugins.py) resolves the plugin from a registry that
    could itself be a dependency-confusion vector — this is a local
    developer-machine action outside the primary attacker-reachable
    surface and was deprioritized accordingly.
  - Did not independently verify the mcp-gateway authorization gap noted
    under Suspicions; left for hostile-authorization-auditor.

Areas explicitly not reviewed:
  - Runtime behavior of the graphify CLI itself (its own dependency tree,
    since it is a third-party PyPI package, not vendored source in this
    repo).
  - Non-Go, non-Python integration bundles under integrations/ (Cline,
    Cursor, Windsurf, Antigravity, etc.) beyond confirming
    install_agent_plugins.py copies them without executing arbitrary
    content from them at copy time.

Recommended follow-up:
  1. Pin `graphifyy` to an exact version in deploy/Dockerfile (e.g.
     `pipx install graphifyy==0.9.48`, matching the version the ingestion
     code was actually verified against) and add a documented process for
     bumping it deliberately, ideally with a hash constraint if pipx/pip
     supports it in this install path.
  2. Delete (or clearly relabel as test-fixture-only, non-cryptographic)
     packages/extension-contracts/extensions.go's `VerifySignature`
     method to remove the risk of it being mistaken for, or accidentally
     wired into, a real enforcement path.
  3. Remove the unused `@genkit-ai/mcp` dependency from package.json, or
     document why it is intentionally retained.
  4. Consider hash-locking the Python worker dependencies (pip-tools /
     `--require-hashes`) to match the integrity guarantee already present
     on the Go side via go.sum.
```
