.PHONY: fmt test integration-test rls-integration-test failure-injection validate audit audit-runtime code-index deployed-benchmark compose-up compose-down db-shell

code-index:
	python3 scripts/generate_code_index.py

deployed-benchmark:
	@test -n "$${SCOS_BENCHMARK_ENDPOINT}" || (printf '%s\n' 'SCOS_BENCHMARK_ENDPOINT must be set' >&2; exit 1)
	@test -n "$${SCOS_BENCHMARK_TOKEN}" || (printf '%s\n' 'SCOS_BENCHMARK_TOKEN must be set through the secret mechanism' >&2; exit 1)
	@test -n "$${SCOS_BENCHMARK_TENANT_ID}" || (printf '%s\n' 'SCOS_BENCHMARK_TENANT_ID must be set' >&2; exit 1)
	@test -n "$${SCOS_BENCHMARK_PRINCIPAL_ID}" || (printf '%s\n' 'SCOS_BENCHMARK_PRINCIPAL_ID must be set' >&2; exit 1)
	python3 benchmarks/run_deployed.py --output "$${SCOS_BENCHMARK_OUTPUT:-benchmarks/deployed-results.json}"

fmt:
	gofmt -w $$(find services -name '*.go' -type f)
	python3 -m compileall -q workers/consolidation workers/index-maintenance workers/policy-evaluation integrations/hermes/plugins/scos

test:
	go test ./...
	python3 -m unittest discover -s workers/consolidation/tests -v
	python3 -m unittest discover -s workers/embeddings/tests -v
	python3 -m unittest discover -s workers/index-maintenance -p "test_*.py" -v
	python3 -m unittest scripts/test_scos_git_worklist.py -v
	python3 -m unittest discover -s tests -p test_definition_of_done_evidence.py -v
	python3 -m unittest discover -s workers/policy-evaluation -p "test_*.py" -v
	python3 -m unittest discover -s integrations/hermes/plugins/scos -p "test_*.py" -v
	python3 tests/failure-injection/test_runner.py -v
	python3 -m unittest discover -s tests -p 'test_compose_security.py' -v
	python3 -m unittest discover -s tests -p 'test_benchmark_publish.py' -v

integration-test:
	COMPOSE_PROJECT_NAME=scos-memory-integration docker compose -f deploy/compose/docker-compose.yml up -d postgres
	@trap 'COMPOSE_PROJECT_NAME=scos-memory-integration docker compose -f deploy/compose/docker-compose.yml stop postgres >/dev/null' EXIT; \
	  until COMPOSE_PROJECT_NAME=scos-memory-integration docker compose -f deploy/compose/docker-compose.yml exec -T postgres pg_isready -U "$${POSTGRES_USER:-scos}" -d "$${POSTGRES_DB:-scos_memory}" >/dev/null 2>&1; do sleep 1; done; \
	  container_id="$$(COMPOSE_PROJECT_NAME=scos-memory-integration docker compose -f deploy/compose/docker-compose.yml ps -q postgres)"; \
	  db_host="$$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$$container_id")"; \
	  test -n "$$db_host" || (printf '%s\n' 'could not determine PostgreSQL container IP' >&2; exit 1); \
	  until bash -c ":</dev/tcp/$$db_host/5432" >/dev/null 2>&1; do sleep 1; done; \
	  test -n "$${DATABASE_URL}" || (printf '%s\n' 'DATABASE_URL must be set with sslmode=require, verify-ca, or verify-full for integration tests' >&2; exit 1); DATABASE_URL="$${DATABASE_URL}" go test -tags=integration ./database/integration -count=1 -v

# Runtime-role suite against an already-migrated database: DATABASE_URL is
# the migration-owning role (seeding only), SCOS_APP_TEST_DATABASE_URL the
# restricted scos_app role every service and worker runs as.
rls-integration-test:
	@test -n "$${DATABASE_URL}" || (printf '%s\n' 'DATABASE_URL (migration owner) must be set' >&2; exit 1)
	@test -n "$${SCOS_APP_TEST_DATABASE_URL}" || (printf '%s\n' 'SCOS_APP_TEST_DATABASE_URL (scos_app runtime role) must be set' >&2; exit 1)
	@# -p 1: the packages share one database, and the backup drills dump all of it while other packages create and alter their temporary schemas.
	go test -tags=integration -count=1 -p 1 ./database/integration ./services/security-gate/cmd/server ./services/tool-governance/cmd/server ./services/recovery-orchestrator/cmd/server ./services/backup-manager/cmd/server ./services/cloud-control-plane/cmd/server ./services/memory-intelligence/cmd/server ./services/credential-vault/cmd/server ./services/okf-synchroniser/cmd/server ./services/autonomous-control-plane/cmd/server ./services/hermes-provider/cmd/server
	python3 -m unittest discover -s tests/integration -p 'test_*.py' -v

failure-injection:
	@test -n "$${SCOS_INTERNAL_SERVICE_TOKEN}" || (printf '%s\n' 'SCOS_INTERNAL_SERVICE_TOKEN must be set; source .env first' >&2; exit 1)
	@test -n "$${SCOS_CONTROL_PLANE_TOKEN}" || (printf '%s\n' 'SCOS_CONTROL_PLANE_TOKEN must be set; source .env first' >&2; exit 1)
	@test -n "$${SCOS_SECURITY_ISSUER_SECRET}" || (printf '%s\n' 'SCOS_SECURITY_ISSUER_SECRET must be set; source .env first' >&2; exit 1)
	env SCOS_INTERNAL_SERVICE_TOKEN="$${SCOS_INTERNAL_SERVICE_TOKEN}" SCOS_CONTROL_PLANE_TOKEN="$${SCOS_CONTROL_PLANE_TOKEN}" SCOS_SECURITY_ISSUER_SECRET="$${SCOS_SECURITY_ISSUER_SECRET}" python3 tests/failure-injection/runner.py

validate:
	python3 scripts/validate_json.py
	@test -n "$${SCOS_SECURITY_ISSUER_SECRET}" || (printf '%s\n' 'SCOS_SECURITY_ISSUER_SECRET must be set for compose validation' >&2; exit 1)
	@test -n "$${SCOS_INTERNAL_SERVICE_TOKEN}" || (printf '%s\n' 'SCOS_INTERNAL_SERVICE_TOKEN must be set for compose validation' >&2; exit 1)
	@test -n "$${SCOS_CONTROL_PLANE_TOKEN}" || (printf '%s\n' 'SCOS_CONTROL_PLANE_TOKEN must be set for compose validation' >&2; exit 1)
	@test -n "$${SCOS_DATABASE_URL}" || (printf '%s\n' 'SCOS_DATABASE_URL must be set for compose validation' >&2; exit 1)
	@test -n "$${SCOS_AGENT_ENDPOINT}" || test -n "$${DEEPSEEK_API_KEY}" || (printf '%s\n' 'SCOS_AGENT_ENDPOINT or DEEPSEEK_API_KEY must be set for compose validation' >&2; exit 1)
	@test -n "$${POSTGRES_PASSWORD}" || (printf '%s\n' 'POSTGRES_PASSWORD must be set for compose validation' >&2; exit 1)
	@test -n "$${SCOS_PUBLIC_GATEWAY_URL}" || (printf '%s\n' 'SCOS_PUBLIC_GATEWAY_URL must be set for compose validation' >&2; exit 1)
	@test -n "$${SCOS_CONTROL_PLANE_TOKEN}" || (printf '%s\n' 'SCOS_CONTROL_PLANE_TOKEN must be set for compose validation' >&2; exit 1)
	docker compose -f deploy/compose/docker-compose.yml config >/dev/null
	python3 scripts/audit_governance.py

audit:
	python3 scripts/audit_governance.py

audit-runtime:
	python3 scripts/audit_governance_runtime.py

compose-up:
	docker compose --env-file .env -f deploy/compose/docker-compose.yml up --build

compose-down:
	docker compose --env-file .env -f deploy/compose/docker-compose.yml down

db-shell:
	docker compose --env-file .env -f deploy/compose/docker-compose.yml exec postgres psql -U "$${POSTGRES_USER:-scos}" -d "$${POSTGRES_DB:-scos_memory}"

doctor:
	python3 scripts/agent_integration_doctor.py

doctor-json:
	python3 scripts/agent_integration_doctor.py --json

install-agents:
	python3 scripts/install_agent_plugins.py
