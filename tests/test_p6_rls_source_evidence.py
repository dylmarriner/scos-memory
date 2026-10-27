"""Guard the merged P6 RLS source evidence against roadmap status drift."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]
TABLES = {
    '000079_code_provenance_force_rls.sql': (
        'code_repositories', 'code_repository_snapshots', 'code_symbols',
        'code_symbol_versions',
    ),
    '000080_derived_code_force_rls.sql': (
        'code_files', 'code_file_versions', 'code_chunks', 'code_edges',
        'code_impact_reconciliations',
    ),
}
DOCS = ('docs/SCOS_P0_P8_ROADMAP.md', 'docs/P6_MEMORY_CODE_READBACK.md')


class P6RlsSourceEvidenceTests(unittest.TestCase):
    def test_migrations_enable_and_force_all_nine_tables(self):
        for filename, tables in TABLES.items():
            sql = (ROOT / 'database/migrations' / filename).read_text()
            for table in tables:
                with self.subTest(filename=filename, table=table):
                    for action in ('ENABLE', 'FORCE'):
                        self.assertRegex(sql, rf'ALTER\s+TABLE\s+{table}\s+{action}\s+ROW\s+LEVEL\s+SECURITY\s*;')

    @staticmethod
    def _postgres_qualifiers_enabled(workflow):
        job = re.search(
            r'(?ms)^  postgres-integration:\n(.*?)(?=^  [a-zA-Z][\w-]*:\n|\Z)',
            workflow,
        )
        if not job:
            return False
        env = re.search(
            r'(?ms)^    env:\n(.*?)(?=^    [a-zA-Z][\w-]*:\n|\Z)',
            job.group(1),
        )
        if not env:
            return False
        for name in ('SCOS_EXPECT_CODE_PROVENANCE_RLS', 'SCOS_EXPECT_DERIVED_CODE_RLS'):
            if not re.search(
                rf'(?m)^      {name}:\s*[\"\']?1[\"\']?\s*$',
                env.group(1),
            ):
                return False
        return True

    def test_required_postgresql_ci_qualifiers_are_enabled_not_just_named(self):
        workflow = (ROOT / '.github/workflows/p4-hybrid-retrieval.yml').read_text()
        self.assertTrue(self._postgres_qualifiers_enabled(workflow))
        for name in ('SCOS_EXPECT_CODE_PROVENANCE_RLS', 'SCOS_EXPECT_DERIVED_CODE_RLS'):
            with self.subTest(disabled=name):
                disabled = re.sub(
                    rf'(?m)^      {name}:.*$', f'      {name}: "0"', workflow,
                )
                self.assertNotEqual(disabled, workflow)
                self.assertFalse(self._postgres_qualifiers_enabled(disabled))

    def test_roadmaps_distinguish_merged_source_from_live_deployment(self):
        stale = (
            'No four-table provenance RLS migration was added or deployed.',
            'No production code-provenance RLS migration, merge or rollout.',
            'four legacy production provenance tables **still lack',
            'four provenance tables remain unprotected by production RLS',
            'The four provenance tables remain unprotected by production RLS',
            'PR #92 development, not merged',
            'PR #92 development',
        )
        for path in DOCS:
            with self.subTest(path=path):
                content = (ROOT / path).read_text()
                self.assertIn('Source merged (PR #92 and PR #93)', content)
                self.assertIn('Live production deployment is not established', content)
                for obsolete in stale:
                    self.assertNotIn(obsolete, content)


if __name__ == '__main__':
    unittest.main()
