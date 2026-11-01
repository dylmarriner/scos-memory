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
            'if [ "$MOCK_FAIL" = "daemon" ] && [ "$1" = "run" ]; then exit 125; fi\n'
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



    def test_docker_runtime_failure_is_not_misdiagnosed_as_acl(self):
        result = self.check(mode="daemon")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("Docker probe failed", result.stderr)
        self.assertNotIn("cannot read pg_hba.conf", result.stderr)

    def test_pinned_probe_image_matches_compose_postgres_service(self):
        compose = (ROOT / "deploy/compose/docker-compose.yml").read_text()
        verifier = CHECK.read_text()
        self.assertIn("  postgres:\n    image: pgvector/pgvector:pg17", compose)
        self.assertIn('postgres_image="pgvector/pgvector:pg17"', verifier)

    def test_image_override_cannot_diverge_from_compose(self):
        env = os.environ.copy()
        env.update({"PATH": str(self.directory) + os.pathsep + env["PATH"],
                    "DOCKER_CALLS": str(self.calls), "MOCK_FAIL": "pass",
                    "SCOS_POSTGRES_IMAGE": "untrusted/postgres:latest"})
        result = subprocess.run(["bash", str(CHECK), str(self.config)],
                                capture_output=True, text=True, env=env, timeout=12)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("image", result.stderr.lower())
        self.assertFalse(self.calls.exists())

    def test_post_checkout_acl_failure_retries_same_commit_after_repair(self):
        """A failed post-FF preflight must not consume a revision forever."""
        source = self.directory / "source"
        remote = self.directory / "remote.git"
        checkout = self.directory / "checkout"
        source.mkdir()

        def git(*args, cwd=None):
            return subprocess.run(["git", *args], cwd=cwd or source,
                                  check=True, capture_output=True, text=True).stdout.strip()

        git("init", "-b", "main")
        git("config", "user.name", "SCOS Test")
        git("config", "user.email", "test@example.invalid")
        (source / "deploy/scripts").mkdir(parents=True)
        (source / "deploy/compose/postgres").mkdir(parents=True)
        (source / "deploy/scripts/auto-deploy.sh").write_bytes(DEPLOY.read_bytes())
        (source / "deploy/scripts/verify_postgres_hba_access.sh").write_bytes(CHECK.read_bytes())
        (source / "deploy/compose/postgres/pg_hba.conf").write_text("allow\n")
        (source / ".env").write_text("# disposable test\n")
        git("add", ".")
        git("commit", "-m", "initial")
        git("clone", "--bare", str(source), str(remote))
        git("clone", str(remote), str(checkout))
        git("remote", "add", "origin", str(remote))

        # The checkout initially passes the HBA preflight. The fetched revision
        # replaces the bind source, so only the post-checkout gate rejects it.
        (source / "deploy/compose/postgres/pg_hba.conf").write_text("deny\n")
        git("add", ".")
        git("commit", "-m", "replace HBA source")
        git("push", "origin", "main")
        new_head = git("rev-parse", "HEAD")

        fake = self.directory / "docker"
        fake.write_text("""#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_CALLS"
if [ "$1" = image ]; then exit 0; fi
if [ "$1" = run ]; then
  if grep -q deny "$HBA_SOURCE"; then exit 1; fi
  exit 0
fi
if [ "$1" = inspect ]; then echo sha256:fake; exit 0; fi
if [ "$1" = compose ]; then
  case " $* " in
    *' images -q '*) echo sha256:fake ;;
    *' ps -q '*) echo testcontainer ;;
  esac
  exit 0
fi
exit 1
""")
        fake.chmod(0o755)
        env = os.environ.copy()
        env.update({"PATH": str(self.directory) + os.pathsep + env["PATH"],
                    "DOCKER_CALLS": str(self.calls),
                    "HBA_SOURCE": str(checkout / "deploy/compose/postgres/pg_hba.conf"),
                    "SCOS_REPO_DIR": str(checkout), "SCOS_MODEL_RUNTIME_ENABLED": "false"})

        def deploy():
            return subprocess.run(["bash", str(checkout / "deploy/scripts/auto-deploy.sh")],
                                  env=env, capture_output=True, text=True, timeout=20)

        first = deploy()
        self.assertNotEqual(0, first.returncode, first.stdout)
        self.assertEqual(new_head, git("rev-parse", "HEAD", cwd=checkout))
        self.assertEqual(new_head, (checkout / ".git/scos-pending-deploy").read_text().strip())
        self.assertNotIn(" compose ", " " + self.calls.read_text())
        # Repair only the disposable source, not any live service.
        (checkout / "deploy/compose/postgres/pg_hba.conf").write_text("allow\n")
        self.calls.write_text("")
        second = deploy()
        self.assertEqual(0, second.returncode, second.stderr)
        self.assertIn("compose --env-file .env -f deploy/compose/docker-compose.yml build", self.calls.read_text())
        self.assertFalse((checkout / ".git/scos-pending-deploy").exists())
        self.calls.write_text("")
        third = deploy()
        self.assertEqual(0, third.returncode, third.stderr)
        self.assertNotIn(" compose ", " " + self.calls.read_text())


if __name__ == "__main__":
    unittest.main()
