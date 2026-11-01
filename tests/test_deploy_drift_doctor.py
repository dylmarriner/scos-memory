"""Read-only deployed source versus GitHub main release drift contract."""
import importlib.util
import json
import os
import subprocess
import tempfile
import io
from contextlib import redirect_stdout
from unittest.mock import patch
from pathlib import Path
from types import SimpleNamespace
import unittest

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "deploy_drift_doctor", ROOT / "deploy/scripts/deploy_drift_doctor.py"
)
doctor = importlib.util.module_from_spec(SPEC)
# Missing implementation is intentional for initial failing TDD run.
SPEC.loader.exec_module(doctor)

LIVE = "a" * 40
REMOTE = "b" * 40


class FakeGit:
    def __init__(self, repo, *, live=LIVE, remote=REMOTE, dirty=False,
                 remote_ok=True, malformed=False):
        self.repo = repo
        self.live = live
        self.remote = remote
        self.dirty = dirty
        self.remote_ok = remote_ok
        self.malformed = malformed
        self.commands = []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        assert command[:3] == ["git", "-C", str(self.repo)], command
        tail = command[3:]
        if tail == ["rev-parse", "--show-toplevel"]:
            return SimpleNamespace(returncode=0, stdout=str(self.repo) + "\n", stderr="")
        if tail == ["rev-parse", "HEAD"]:
            return SimpleNamespace(returncode=0, stdout=self.live + "\n", stderr="")
        if tail == ["status", "--porcelain", "--untracked-files=no"]:
            return SimpleNamespace(returncode=0, stdout=" M deploy/compose/config.yml\n" if self.dirty else "", stderr="")
        if tail == ["ls-remote", "--heads", "origin", "refs/heads/main"]:
            if not self.remote_ok:
                return SimpleNamespace(returncode=128, stdout="", stderr="token=SECRET_NO_LOG")
            sha = "not-a-sha" if self.malformed else self.remote
            return SimpleNamespace(returncode=0, stdout=f"{sha}\trefs/heads/main\n", stderr="")
        raise AssertionError(f"Unexpected or mutating git invocation: {command}")


class DeployDriftDoctorTests(unittest.TestCase):
    def setUp(self):
        self.repo = ROOT.resolve()

    def test_clean_matching_release_is_aligned(self):
        fake = FakeGit(self.repo, live=LIVE, remote=LIVE)
        result = doctor.audit(self.repo, runner=fake)
        self.assertEqual("aligned", result["status"])
        self.assertTrue(result["matches_target"])
        self.assertTrue(result["clean"])
        self.assertEqual(LIVE, result["release_sha"])
        self.assertEqual(LIVE, result["target_sha"])
        self.assertEqual("origin/main", result["source_ref"])
        self.assertEqual(0, doctor.exit_code(result))
        self.assertEqual(4, len(fake.commands))
        for command in fake.commands:
            self.assertFalse(any(arg in command for arg in ("fetch", "pull", "push",
                                                            "reset", "checkout", "switch",
                                                            "merge", "docker")))

    def test_old_deployed_release_is_detected_without_fetch(self):
        fake = FakeGit(self.repo)
        result = doctor.audit(self.repo, runner=fake)
        self.assertEqual("drifted", result["status"])
        self.assertEqual(LIVE, result["release_sha"])
        self.assertEqual(REMOTE, result["target_sha"])
        self.assertFalse(result["matches_target"])
        self.assertEqual(1, doctor.exit_code(result))

    def test_dirty_deployed_release_is_not_reported_aligned(self):
        fake = FakeGit(self.repo, live=LIVE, remote=LIVE, dirty=True)
        result = doctor.audit(self.repo, runner=fake)
        self.assertEqual("modified", result["status"])
        self.assertFalse(result["clean"])
        self.assertEqual(1, doctor.exit_code(result))

    def test_stale_and_dirty_release_produces_both_flags(self):
        result = doctor.audit(self.repo, runner=FakeGit(self.repo, dirty=True))
        self.assertEqual("drifted_modified", result["status"])
        self.assertFalse(result["matches_target"])
        self.assertFalse(result["clean"])

    def test_unavailable_remote_fails_closed_without_credentials(self):
        fake = FakeGit(self.repo, remote_ok=False)
        with self.assertRaises(doctor.DriftError) as error:
            doctor.audit(self.repo, runner=fake)
        self.assertNotIn("SECRET_NO_LOG", str(error.exception))

    def test_malformed_git_remote_reply_fails_closed(self):
        with self.assertRaises(doctor.DriftError):
            doctor.audit(self.repo, runner=FakeGit(self.repo, malformed=True))

    def test_cli_prints_machine_readable_json_and_exit_codes(self):
        import io
        from contextlib import redirect_stdout
        fake = FakeGit(self.repo)
        out = io.StringIO()
        with redirect_stdout(out):
            code = doctor.main(["--repo", str(self.repo)], runner=fake)
        self.assertEqual(1, code)
        self.assertEqual("drifted", json.loads(out.getvalue())["status"])

    def test_main_only_is_a_live_remote_query_not_stale_local_ref(self):
        fake = FakeGit(self.repo, live=LIVE, remote=LIVE)
        doctor.audit(self.repo, runner=fake)
        self.assertIn(
            ["git", "-C", str(self.repo), "ls-remote", "--heads", "origin", "refs/heads/main"],
            fake.commands,
        )


class RealGitDriftRegressions(unittest.TestCase):
    """Exercise the real git executable in disposable repositories only."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.repo = self.base / "release"
        self.remote = self.base / "remote.git"
        subprocess.run(["git", "init", "--bare", "--initial-branch=main", str(self.remote)], check=True, capture_output=True)
        subprocess.run(["git", "init", "--initial-branch=main", str(self.repo)], check=True, capture_output=True)
        self.git("config", "user.name", "SCOS test")
        self.git("config", "user.email", "test@example.invalid")
        (self.repo / "tracked.txt").write_text("same content\n")
        self.git("add", "tracked.txt")
        self.git("commit", "-m", "test initial commit")
        self.git("remote", "add", "origin", str(self.remote))
        self.git("push", "origin", "main")

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.repo), *args], check=True, capture_output=True)

    def test_status_does_not_rewrite_deployed_index(self):
        index = self.repo / ".git" / "index"
        before = index.read_bytes()
        os.utime(self.repo / "tracked.txt", (946684800, 946684800))
        result = doctor.audit(self.repo)
        self.assertEqual("aligned", result["status"])
        self.assertEqual(before, index.read_bytes(), "read-only drift doctor modified the Git index")
        self.assertFalse((self.repo / ".git" / "index.lock").exists())

    def test_invalid_utf8_tracked_filename_fails_closed_with_json(self):
        self.git("config", "core.quotePath", "false")
        raw_name = os.fsencode(self.repo) + b"/invalid_\xff"
        fd = os.open(raw_name, os.O_CREAT | os.O_WRONLY, 0o600)
        os.write(fd, b"original")
        os.close(fd)
        self.git("add", "--all")
        self.git("commit", "-m", "tracked invalid UTF8 name")
        self.git("push", "origin", "main")
        fd = os.open(raw_name, os.O_WRONLY | os.O_TRUNC)
        os.write(fd, b"modified")
        os.close(fd)
        out = io.StringIO()
        with redirect_stdout(out):
            code = doctor.main(["--repo", str(self.repo)])
        self.assertEqual(2, code)
        report = json.loads(out.getvalue())
        self.assertEqual("unknown", report["status"])
        self.assertNotIn("invalid_", report.get("reason", ""))

    def test_symlink_loop_is_machine_readable_unknown(self):
        loop = self.base / "loop"
        loop.symlink_to(loop)
        out = io.StringIO()
        with redirect_stdout(out):
            code = doctor.main(["--repo", str(loop)])
        self.assertEqual(2, code)
        self.assertEqual("unknown", json.loads(out.getvalue())["status"])

    def test_path_resolution_runtime_error_fails_closed(self):
        original = Path.resolve
        def fail_loop(path, *args, **kwargs):
            if str(path).endswith("loop-probe"):
                raise RuntimeError("symbolic link loop")
            return original(path, *args, **kwargs)
        out = io.StringIO()
        with patch.object(Path, "resolve", fail_loop), redirect_stdout(out):
            code = doctor.main(["--repo", str(self.base / "loop-probe")])
        self.assertEqual(2, code)
        self.assertEqual("unknown", json.loads(out.getvalue())["status"])

    def test_directory_probe_oserror_fails_closed(self):
        original = Path.is_dir
        def fail_probe(path, *args, **kwargs):
            if str(path).endswith("inaccessible-probe"):
                raise OSError("untrusted path diagnostic")
            return original(path, *args, **kwargs)
        out = io.StringIO()
        with patch.object(Path, "is_dir", fail_probe), redirect_stdout(out):
            code = doctor.main(["--repo", str(self.base / "inaccessible-probe")])
        self.assertEqual(2, code)
        report = json.loads(out.getvalue())
        self.assertEqual("unknown", report["status"])
        self.assertNotIn("untrusted path diagnostic", out.getvalue())

    def test_unknown_result_maps_to_exit_two(self):
        self.assertEqual(2, doctor.exit_code({"status": "unknown"}))

    def test_runbook_identifies_script_checkout_separately_from_release(self):
        runbook = (ROOT / "docs/runbooks/release-drift.md").read_text()
        self.assertIn("cd /path/to/scos-memory-source", runbook)
        self.assertIn("--repo /run/media/kubuntux/backup-dev-ext4/scos/scos-memory-release", runbook)


if __name__ == "__main__":
    unittest.main()
