"""Fail-closed PostgreSQL HBA deploy preflight tests (no live Docker)."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CHECK = ROOT / "deploy/scripts/verify_postgres_hba_access.sh"
DEPLOY = ROOT / "deploy/scripts/auto-deploy.sh"


class HbaDeployPreflightTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.config = self.directory / "pg_hba.conf"
        self.config.write_text("hostssl all all all scram-sha-256\n")
        fake = self.directory / "docker"
        fake.write_text(
            "#!/bin/sh\n"
            'printf "%s\\n" "$*" >> "$DOCKER_CALLS"\n'
            'if [ "$MOCK_FAIL" = "image" ] && [ "$1" = "image" ]; then exit 1; fi\n'
            'if [ "$MOCK_FAIL" = "run" ] && [ "$1" = "run" ]; then exit 1; fi\n'
            "exit 0\n"
        )
        fake.chmod(0o755)
        self.calls = self.directory / "calls"

    def check(self, mode="pass", config=None):
        env = os.environ.copy()
        env.update({
            "PATH": str(self.directory) + os.pathsep + env["PATH"],
            "DOCKER_CALLS": str(self.calls),
            "MOCK_FAIL": mode,
            "SCOS_POSTGRES_IMAGE": "pgvector/pgvector:pg17",
        })
        return subprocess.run(
            ["bash", str(CHECK), str(config or self.config)],
            capture_output=True, text=True, timeout=12, env=env,
        )

    def test_read_check_uses_isolated_nonprivileged_disposable_container(self):
        result = self.check()
        self.assertEqual(0, result.returncode, result.stderr)
        calls = self.calls.read_text().splitlines()
        self.assertEqual(2, len(calls))
        self.assertIn("image inspect pgvector/pgvector:pg17", calls[0])
        for required in ("run --rm", "--pull=never", "--network none", "--read-only",
                         "--user 999:999", "--entrypoint /bin/sh",
                         "readonly", "test -r /etc/scos/postgres/pg_hba.conf"):
            self.assertIn(required, calls[1])
        self.assertNotIn("--privileged", calls[1])

    def test_permission_failure_blocks_preflight_without_leaking_file_contents(self):
        result = self.check(mode="run")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("pg_hba.conf", result.stderr)
        self.assertNotIn("scram-sha-256", result.stderr)

    def test_missing_image_blocks_run(self):
        result = self.check(mode="image")
        self.assertNotEqual(0, result.returncode)
        self.assertEqual(1, len(self.calls.read_text().splitlines()))

    def test_missing_file_blocks_before_docker(self):
        result = self.check(config=self.directory / "missing.conf")
        self.assertNotEqual(0, result.returncode)
        self.assertFalse(self.calls.exists())

    def test_auto_deploy_checks_before_early_return_and_mutations(self):
        script = DEPLOY.read_text()
        guard = script.index("verify_postgres_hba_access.sh")
        self.assertLess(guard, script.index('if [ "$BEFORE" = "$AFTER" ]'))
        self.assertLess(guard, script.index("git merge --ff-only"))
        self.assertLess(guard, script.index("run --rm migrations"))
        # New checkout may replace the HBA inode and drop its former ACL.
        # Verify again before build/migrations after the fast-forward.
        self.assertEqual(2, script.count("verify_postgres_hba_access.sh"))
        following = script.index("verify_postgres_hba_access.sh", guard + 1)
        self.assertGreater(following, script.index("git merge --ff-only"))
        self.assertLess(following, script.index('"${COMPOSE[@]}" build'))



if __name__ == "__main__":
    unittest.main()
