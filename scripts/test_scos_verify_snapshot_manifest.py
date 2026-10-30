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
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("scos_verify_snapshot_manifest.py")
SNAPSHOT = "11111111-1111-4111-8111-111111111111"
REPO = "22222222-2222-4222-8222-222222222222"


def git(repo, *args):
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    return subprocess.run(["git", *args], cwd=repo, check=True, env=env,
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

    def invoke(self, pages=None, env=None, extra_args=()):
        if pages is None:
            pages = [self.page(self.files)]
        path = self.repo / "pages.ndjson"
        path.write_text("\n".join(json.dumps(page) for page in pages) + "\n")
        return subprocess.run([sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
                               "--commit", self.commit, "--snapshot-id", SNAPSHOT,
                               "--pages-file", str(path), *extra_args], capture_output=True,
                              text=True, env=env)

    def page(self, files, **extra):
        return {"snapshot_id": SNAPSHOT, "repository_id": REPO, "branch": "main",
                "commit_id": self.commit, "parent_commit_id": "",
                "files": files, **extra}

    def test_unbound_manifest_rejects_nul_in_exported_branch(self):
        result = self.invoke([self.page(self.files, branch="main\x00forged")])
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("invalid branch", result.stderr)
        self.assertNotIn("Traceback", result.stderr)

    def test_independent_trusted_identity_accepts_matching_export(self):
        result = self.invoke(extra_args=("--repository-id", REPO, "--branch", "main"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIs(json.loads(result.stdout)["trusted_identity_checked"], True)

    def test_independent_trusted_identity_rejects_consistently_forged_export(self):
        forged = self.page(self.files, repository_id="33333333-3333-4333-8333-333333333333",
                           branch="attacker")
        result = self.invoke([forged],
                             extra_args=("--repository-id", REPO, "--branch", "main"))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("trusted repository_id", result.stderr)
        result = self.invoke([self.page(self.files, branch="attacker")],
                             extra_args=("--repository-id", REPO, "--branch", "main"))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("trusted branch", result.stderr)

    def test_independent_trusted_identity_requires_valid_pair(self):
        for flags in (("--repository-id", REPO), ("--branch", "main"),
                      ("--repository-id", "invalid", "--branch", "main"),
                      ("--repository-id", REPO, "--branch", "  ")):
            with self.subTest(flags=flags):
                result = self.invoke(extra_args=flags)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("trusted", result.stderr)

    def test_require_trusted_identity_fails_closed_without_inventory(self):
        missing = self.invoke(extra_args=("--require-trusted-identity",))
        self.assertNotEqual(missing.returncode, 0, missing.stdout)
        self.assertIn("trusted", missing.stderr)
        bound = self.invoke(extra_args=("--require-trusted-identity", "--repository-id", REPO,
                                        "--branch", "main"))
        self.assertEqual(bound.returncode, 0, bound.stderr)
        self.assertIs(json.loads(bound.stdout)["trusted_identity_checked"], True)

    def test_valid_complete_git_manifest(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output["file_count"], 2)
        self.assertEqual(output["status"], "stored_manifest_matches_git_bytes")
        self.assertIs(output["trusted_identity_checked"], False)

    def test_uppercase_sha256_hex_from_accepted_index_payload_is_supported(self):
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = files[0]["content_hash"].upper()
        result = self.invoke([self.page(files)])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_index_handler_accepted_surrounding_sha_whitespace(self):
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = "  \t" + files[0]["content_hash"].upper() + " \n"
        result = self.invoke([self.page(files)])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_explicit_trusted_parent_matches_recorded_snapshot_parent(self):
        parent = self.commit
        (self.repo / "a.py").write_text("a = 2\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "target")
        self.commit = git(self.repo, "rev-parse", "HEAD")
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = hashlib.sha256(b"a = 2\n").hexdigest()
        self.assertEqual(self.invoke([self.page(files, parent_commit_id=parent)],
                                     extra_args=("--parent", parent)).returncode, 0)

    def test_explicit_parent_rejects_forged_or_missing_recorded_binding(self):
        parent = self.commit
        (self.repo / "a.py").write_text("a = 2\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "target")
        self.commit = git(self.repo, "rev-parse", "HEAD")
        files = [dict(x) for x in self.files]
        files[0]["content_hash"] = hashlib.sha256(b"a = 2\n").hexdigest()
        for claimed in ("", "not-a-git-commit"):
            with self.subTest(claimed=claimed):
                result = self.invoke([self.page(files, parent_commit_id=claimed)],
                                     extra_args=("--parent", parent))
                self.assertNotEqual(result.returncode, 0, result.stdout)

    def test_explicit_parent_rejects_target_itself_and_empty_revision(self):
        same = self.invoke([self.page(self.files, parent_commit_id=self.commit)],
                           extra_args=("--parent", self.commit))
        self.assertNotEqual(same.returncode, 0, same.stdout)
        empty = self.invoke(extra_args=("--parent", ""))
        self.assertNotEqual(empty.returncode, 0, empty.stdout)

    def test_explicit_parent_must_be_target_ancestor(self):
        unrelated = self.commit
        git(self.repo, "checkout", "--orphan", "unrelated")
        git(self.repo, "rm", "-rq", "--cached", ".")
        (self.repo / "a.py").write_text("a = 1\n")
        (self.repo / "b.go").write_text("package b\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "independent history")
        self.commit = git(self.repo, "rev-parse", "HEAD")
        result = self.invoke([self.page(self.files, parent_commit_id=unrelated)],
                             extra_args=("--parent", unrelated))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("ancestor", result.stderr)

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

    def test_fixture_git_helper_ignores_inherited_foreign_git_directory(self):
        foreign = self.repo / "foreign-for-helper"
        foreign.mkdir()
        subprocess.run(["git", "init", "-q"], cwd=foreign, check=True,
                       env={k: v for k, v in os.environ.items() if not k.startswith("GIT_")})
        with patch.dict(os.environ, {"GIT_DIR": str(foreign / ".git"),
                                     "GIT_WORK_TREE": str(foreign)}):
            self.assertEqual(git(self.repo, "rev-parse", "--show-toplevel"),
                             str(self.repo))

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
