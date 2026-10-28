"""P7 restricted-role HTTP acceptance must run in the PostgreSQL CI job."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]


class P7IntegrationCIGateTests(unittest.TestCase):
    def test_postgres_job_runs_integration_tagged_retrieval_suite(self):
        workflow = (ROOT / '.github/workflows/p4-hybrid-retrieval.yml').read_text()
        job = workflow.split('  postgres-integration:', 1)[1].split('  embeddings-worker:', 1)[0]
        self.assertRegex(job, r'(?m)^\s+run: go test -tags=integration -count=1 ./services/retrieval-orchestrator/cmd/server\s*$')
        self.assertIn('SCOS_APP_TEST_DATABASE_URL:', job)
        self.assertIn('SCOS_REQUIRE_APP_RLS_TESTS:', job)


if __name__ == '__main__':
    unittest.main()
