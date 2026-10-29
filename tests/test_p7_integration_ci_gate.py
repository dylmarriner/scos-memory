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

    def test_policy_only_changes_trigger_restricted_role_postgres_suite(self):
        workflow = (ROOT / '.github/workflows/p4-hybrid-retrieval.yml').read_text()
        # Both pull_request and push filters must include RLS policy changes.
        self.assertEqual(2, len(re.findall(
            r'(?m)^\s+- "database/policies/\*\*"\s*$', workflow
        )))


    def test_governance_rls_fixture_is_required_in_postgres_ci(self):
        workflow = (ROOT / '.github/workflows/p4-hybrid-retrieval.yml').read_text()
        job = workflow.split('  postgres-integration:', 1)[1].split('  embeddings-worker:', 1)[0]
        self.assertIn('Run governance RLS fixture against isolated TLS database', job)
        self.assertIn('SCOS_RLS_TEST_CREATE_DATABASE: "1"', job)
        self.assertIn('sslmode=require', job)
        self.assertIn("go test -tags=integration -run '^TestGovernanceRLSAndIsolation$'", job)
        self.assertEqual(2, len(re.findall(
            r'(?m)^\s+- "database/integration/\*\*"\s*$', workflow
        )))


if __name__ == '__main__':
    unittest.main()
