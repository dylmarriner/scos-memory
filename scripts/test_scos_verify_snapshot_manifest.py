#!/usr/bin/env python3
"""Real-Git fixtures for independently verifying SCOS snapshot hash readback."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("scos_verify_snapshot_manifest.py")
SNAPSHOT = "11111111-1111-4111-8111-111111111111"
REPO = "22222222-2222-4222-8222-222222222222"


def git(repo, *args):
    return subprocess.run(["git", *args], cwd=repo, check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.decode().strip()


class SnapshotManifestVerificationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name)
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.name", "SCOS")
        git(self.repo, "config", "user.email", "fixture@example.invalid")
        (self.repo / "a.py").write_text("a = 1\n")
        (self.repo / "b.go").write_text("package b\n")
        (self.repo / "skip.md").write_text("ignored\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "fixture")
        self.commit = git(self.repo, "rev-parse", "HEAD")
        self.files = [
            {"path": "a.py", "language": "python", "content_hash": hashlib.sha256(b"a = 1\n").hexdigest()},
            {"path": "b.go", "language": "go", "content_hash": hashlib.sha256(b"package b\n").hexdigest()},
        ]

    def invoke(self, pages=None, env=None):
        if pages is None:
            pages = [self.page(self.files)]
        path = self.repo / "pages.ndjson"
        path.write_text("\n".join(json.dumps(page) for page in pages) + "\n")
        return subprocess.run([sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
                               "--commit", self.commit, "--snapshot-id", SNAPSHOT,
                               "--pages-file", str(path)], capture_output=True,
                              text=True, env=env)

    def page(self, files, **extra):
        return {"snapshot_id": SNAPSHOT, "repository_id": REPO, "branch": "main",
                "commit_id": self.commit, "parent_commit_id": "",
                "files": files, **extra}

    def test_valid_complete_git_manifest(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output["file_count"], 2)
        self.assertEqual(output["status"], "stored_manifest_matches_git_bytes")

    def test_uppercase_sha256_hex_from_accepted_index_payload_is_supported(self):
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = files[0]["content_hash"].upper()
        result = self.invoke([self.page(files)])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_forged_file_bytes_under_true_commit_are_rejected(self):
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = hashlib.sha256(b"forged\n").hexdigest()
        result = self.invoke([self.page(files)])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mismatch", result.stderr)

    def test_missing_and_extra_paths_are_rejected(self):
        self.assertNotEqual(self.invoke([self.page(self.files[:1])]).returncode, 0)
        extra = self.files + [{"path": "fake.py", "language": "python",
                               "content_hash": hashlib.sha256(b"fake").hexdigest()}]
        self.assertNotEqual(self.invoke([self.page(extra)]).returncode, 0)

    def test_incomplete_pagination_and_wrong_order_are_rejected(self):
        first = self.page(self.files[:1], next_after_path="a.py")
        self.assertNotEqual(self.invoke([first]).returncode, 0)
        self.assertNotEqual(self.invoke([self.page(self.files[::-1])]).returncode, 0)
        self.assertEqual(self.invoke([first, self.page(self.files[1:])]).returncode, 0)

    def test_commit_or_page_identity_drift_is_rejected(self):
        self.assertNotEqual(self.invoke([self.page(self.files, commit_id="bad")]).returncode, 0)
        first = self.page(self.files[:1], next_after_path="a.py")
        last = self.page(self.files[1:], repository_id="33333333-3333-4333-8333-333333333333")
        self.assertNotEqual(self.invoke([first, last]).returncode, 0)

    def test_scip_pseudo_hash_is_not_git_byte_proof(self):
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = "scip:fake"
        self.assertNotEqual(self.invoke([self.page(files)]).returncode, 0)

    def test_large_blob_and_unicode_filename_stream_correctly(self):
        raw = ("π = 42\n".encode("utf-8") * 15000)
        name = "src/éxample.py"
        (self.repo / "src").mkdir()
        (self.repo / name).write_bytes(raw)
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "large unicode")
        self.commit = git(self.repo, "rev-parse", "HEAD")
        files = self.files + [{"path": name, "language": "python",
                               "content_hash": hashlib.sha256(raw).hexdigest()}]
        result = self.invoke([self.page(sorted(files, key=lambda x: x["path"].encode()))])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_parent_binding_drift_and_duplicate_page_are_rejected(self):
        first = self.page(self.files[:1], next_after_path="a.py", parent_commit_id="parent-a")
        last = self.page(self.files[1:], parent_commit_id="parent-b")
        self.assertNotEqual(self.invoke([first, last]).returncode, 0)
        self.assertNotEqual(self.invoke([self.page(self.files), self.page(self.files)]).returncode, 0)

    def test_symlinks_and_unsupported_files_are_not_manifest_entries(self):
        (self.repo / "linked.py").symlink_to("a.py")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "symlink")
        self.commit = git(self.repo, "rev-parse", "HEAD")
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_ambient_git_overrides_cannot_redirect_attestation(self):
        foreign = self.repo / "foreign"
        foreign.mkdir()
        git(foreign, "init", "-q")
        env = os.environ.copy()
        env.update({"GIT_DIR": str(foreign / ".git"), "GIT_WORK_TREE": str(foreign)})
        result = self.invoke(env=env)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
