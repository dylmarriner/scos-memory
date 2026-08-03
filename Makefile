.PHONY: fmt test validate compose-up compose-down db-shell

fmt:
	gofmt -w $$(find services -name '*.go' -type f)
	python3 -m compileall -q workers/consolidation workers/index-maintenance workers/policy-evaluation

test:
	go test ./...
	python3 -m unittest discover -s workers/consolidation/tests -v
	python3 -m unittest discover -s workers/index-maintenance -p "test_*.py" -v
	python3 -m unittest discover -s workers/policy-evaluation -p "test_*.py" -v

validate:
	python3 scripts/validate_json.py
	docker compose -f deploy/compose/docker-compose.yml config >/dev/null
	python3 scripts/audit_governance.py

audit:
	python3 scripts/audit_governance.py

compose-up:
	docker compose -f deploy/compose/docker-compose.yml up --build

compose-down:
	docker compose -f deploy/compose/docker-compose.yml down

db-shell:
	docker compose -f deploy/compose/docker-compose.yml exec postgres psql -U "$${POSTGRES_USER:-scos}" -d "$${POSTGRES_DB:-scos_memory}"
