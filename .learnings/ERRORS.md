## [ERR-20260902-001] unittest-module-discovery
Summary: Targeted unittest invocation could not import the new test as `tests.test_deployed_benchmark`.
Error: `ModuleNotFoundError: No module named 'tests.test_deployed_benchmark'`; the repository's tests directory is not a Python package.
Suggested Fix: Run the test by file path or with unittest discovery, matching the repository's existing test commands.

## [ERR-20260902-002] compose-security-regression
Summary: Full `make test` failed in the pre-existing Compose security test while validating current deployment configuration.
Error: `test_no_plaintext_host_ports_or_password_fallbacks` expected a `${SCOS_TAILSCALE_IP|CLOUD_BIND_ADDRESS}` binding but found the literal `scos.172.236.155.15.nip.io:172.236.155.15`.
Suggested Fix: Replace the literal host binding with an environment-variable-based bind address, preserving the secure default and update the test if the deployment contract intentionally changed.

## [ERR-20260902-003] governance-marker-scan
Summary: Governance audit gate failed on four tunnel-client files containing the scanner's incomplete-marker terms.
Error: `GOVERNANCE AUDIT: FAIL`; reported `integrations/tunnel-client/cmd/client/run_command_test.go`, `pkg/oauth/hostbundle_test.go`, `pkg/oauth/auth_meta.go`, and `pkg/localproxy/localproxy_test.go`.
Suggested Fix: Inspect the scanner matches and either remove false-positive marker text or update the audit scanner's exclusion/word-boundary logic without weakening genuine production incomplete-state detection.

## [ERR-20260902-004] governance-runtime-placeholder
Summary: Runtime governance audit found an existing tunnel-client handler returning HTTP 501/unavailable.
Error: `GOVERNANCE RUNTIME AUDIT: FAIL`; reported `integrations/tunnel-client/pkg/localproxy/localproxy.go:1164`.
Suggested Fix: Implement or explicitly gate the unsupported runtime path, then add a regression test; do not claim the repository audit gate passes until resolved.

## [ERR-20260902-005] verification-workdir-typo
Summary: A combined final verification command did not run because the supplied working directory path had incorrect capitalization.
Error: `FileSystem.access` failed for `/media/kubuntux/DEVELOPMENT1/scOS-memory-fabric`.
Suggested Fix: Re-run from `/media/kubuntux/DEVELOPMENT1/scos-memory-fabric`.

## [ERR-20260903-001] compose-validation-missing-secrets
Summary: Compose configuration validation could not run without deployment secrets.
Error: `docker compose -f deploy/compose/docker-compose.yml config --quiet` failed because required `SCOS_APP_DATABASE_URL`, service tokens, issuer secret, database password, and related variables were unset.
Suggested Fix: Run validation in the deployment environment after injecting secrets through the documented secret mechanism; never add insecure defaults to make local validation pass.

## [ERR-20260903-002] governance-audit-marker-scan
Summary: The repository governance audit remains blocked by its incomplete-marker scan in existing tunnel-client files.
Error: `make audit` reported `integrations/tunnel-client/cmd/client/run_command_test.go`, `pkg/oauth/hostbundle_test.go`, `pkg/oauth/auth_meta.go`, and `pkg/localproxy/localproxy_test.go`.
Suggested Fix: Resolve the existing false-positive marker matches or update the scanner's matching logic without weakening genuine production completeness checks.

## [ERR-20260903-003] tunnel-client-test-timeout
Summary: The nested tunnel-client test suite did not complete within the 120-second verification window.
Error: `go test ./...` in `integrations/tunnel-client` produced no output and was terminated by the command timeout.
Suggested Fix: Run focused packages first, then investigate the specific long-running or blocked package with per-test timeouts before claiming the nested suite passes.
