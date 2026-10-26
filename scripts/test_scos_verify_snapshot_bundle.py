#!/usr/bin/env python3
"""End-to-end real-Git tests for joint snapshot manifest/event export verification."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("scos_verify_snapshot_bundle.py")
SNAPSHOT = "11111111-1111-4111-8111-111111111111"
REPO = "22222222-2222-4222-8222-222222222222"
OTHER_REPO = "33333333-3333-4333-8333-333333333333"


def git(repo, *args):
    return subprocess.run(["git", *args], cwd=repo, check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.decode().strip()


class SnapshotBundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.name", "SCOS Fixture")
        git(self.repo, "config", "user.email", "fixture@example.invalid")
        (self.repo / "before.py").write_text("x = 1\n")
        (self.repo / "stable.go").write_text("package stable\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "parent")
        self.parent = git(self.repo, "rev-parse", "HEAD")
        (self.repo / "before.py").write_text("x = 2\n")
        (self.repo / "new.rs").write_text("fn new() {}\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "target")
        self.target = git(self.repo, "rev-parse", "HEAD")
        self.manifest = [
            {"path": "before.py", "language": "python",
             "content_hash": hashlib.sha256(b"x = 2\n").hexdigest()},
            {"path": "new.rs", "language": "rust",
             "content_hash": hashlib.sha256(b"fn new() {}\n").hexdigest()},
            {"path": "stable.go", "language": "go",
             "content_hash": hashlib.sha256(b"package stable\n").hexdigest()},
        ]
        self.events = [
            {"path": "before.py", "change_kind": "modify", "evidence_class": "exact_path"},
            {"path": "new.rs", "change_kind": "add", "evidence_class": "snapshot_manifest"},
        ]

    def page(self, key, items, **overrides):
        return {"snapshot_id": SNAPSHOT, "repository_id": REPO, "branch": "main",
                "commit_id": self.target, "parent_commit_id": self.parent,
                key: items, **overrides}

    def invoke(self, manifests=None, events=None, args=None):
        if manifests is None:
            manifests = [self.page("files", self.manifest)]
        if events is None:
            events = [self.page("events", self.events)]
        (self.repo / "manifest.ndjson").write_text(
            "".join(json.dumps(x) + "\n" for x in manifests))
        (self.repo / "events.ndjson").write_text(
            "".join(json.dumps(x) + "\n" for x in events))
        argv = [sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
                "--commit", self.target, "--parent", self.parent,
                "--snapshot-id", SNAPSHOT,
                "--manifest-pages-file", str(self.repo / "manifest.ndjson"),
                "--event-pages-file", str(self.repo / "events.ndjson")]
        if args:
            argv += args
        return subprocess.run(argv, capture_output=True, text=True)

    def test_valid_bundle_with_multiple_pages(self):
        m = [self.page("files", self.manifest[:2], next_after_path="new.rs"),
             self.page("files", self.manifest[2:])]
        e = [self.page("events", self.events[:1], next_after_path="before.py"),
             self.page("events", self.events[1:])]
        result = self.invoke(m, e)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["file_count"], 3)
        self.assertEqual(json.loads(result.stdout)["event_count"], 2)

    def test_individually_valid_exports_from_different_repositories_fail_joint_binding(self):
        events = [self.page("events", self.events, repository_id=OTHER_REPO)]
        result = self.invoke(events=events)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("repository", result.stderr)

    def test_individually_valid_exports_from_different_branches_fail_joint_binding(self):
        events = [self.page("events", self.events, branch="release")]
        result = self.invoke(events=events)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("branch", result.stderr)

    def test_event_and_manifest_have_same_snapshot_but_different_parent_claims(self):
        # Both claim the same target; a stale event export must not be accepted.
        events = [self.page("events", self.events, parent_commit_id="")]
        result = self.invoke(events=events)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("parent", result.stderr)

    def test_manifest_pages_with_repository_drift_fail(self):
        manifests = [
            self.page("files", self.manifest[:1], next_after_path="before.py"),
            self.page("files", self.manifest[1:], repository_id=OTHER_REPO),
        ]
        self.assertNotEqual(self.invoke(manifests=manifests).returncode, 0)

    def test_forged_event_evidence_fails(self):
        forged = [dict(x) for x in self.events]
        forged[0]["evidence_class"] = "snapshot_manifest"
        self.assertNotEqual(self.invoke(events=[self.page("events", forged)]).returncode, 0)

    def test_initial_snapshot_with_no_parent_is_accepted(self):
        initial = [self.page("files", self.manifest, parent_commit_id="")]
        additions = [{"path": f["path"], "change_kind": "add",
                      "evidence_class": "snapshot_manifest"} for f in self.manifest]
        events = [self.page("events", additions, parent_commit_id="")]
        # Override the default trusted-parent option in a dedicated subprocess.
        (self.repo / "manifest.ndjson").write_text(json.dumps(initial[0]) + "\n")
        (self.repo / "events.ndjson").write_text(json.dumps(events[0]) + "\n")
        result = subprocess.run([
            sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
            "--commit", self.target, "--initial", "--snapshot-id", SNAPSHOT,
            "--manifest-pages-file", str(self.repo / "manifest.ndjson"),
            "--event-pages-file", str(self.repo / "events.ndjson"),
        ], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def rerun_existing_exports(self, args=()):
        return subprocess.run([
            sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
            "--commit", self.target, "--parent", self.parent,
            "--snapshot-id", SNAPSHOT,
            "--manifest-pages-file", str(self.repo / "manifest.ndjson"),
            "--event-pages-file", str(self.repo / "events.ndjson"),
            *args,
        ], capture_output=True, text=True)

    def test_trusted_repository_and_branch_accept_matching_export(self):
        result = self.invoke(args=["--repository-id", REPO, "--branch", "main"])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_trusted_repository_rejects_consistently_forged_both_exports(self):
        result = self.invoke(
            manifests=[self.page("files", self.manifest, repository_id=OTHER_REPO)],
            events=[self.page("events", self.events, repository_id=OTHER_REPO)],
            args=["--repository-id", REPO, "--branch", "main"])
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("trusted repository", result.stderr)

    def test_trusted_branch_rejects_consistently_forged_both_exports(self):
        result = self.invoke(
            manifests=[self.page("files", self.manifest, branch="release")],
            events=[self.page("events", self.events, branch="release")],
            args=["--repository-id", REPO, "--branch", "main"])
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("trusted branch", result.stderr)

    def test_trusted_repository_id_must_be_canonical_uuid(self):
        result = self.invoke(args=["--repository-id", "not-a-uuid", "--branch", "main"])
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("canonical UUID", result.stderr)

    def test_manifest_export_rejects_duplicate_json_identity_keys(self):
        self.assertEqual(self.invoke().returncode, 0)
        path = self.repo / "manifest.ndjson"
        raw = path.read_text()
        path.write_text(raw.replace('"branch": "main"', '"branch": "release", "branch": "main"'))
        result = self.rerun_existing_exports()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("duplicate JSON key", result.stderr)

    def test_event_export_rejects_duplicate_json_identity_keys(self):
        self.assertEqual(self.invoke().returncode, 0)
        path = self.repo / "events.ndjson"
        raw = path.read_text()
        path.write_text(raw.replace('"repository_id": "' + REPO + '"',
                                    '"repository_id": "' + OTHER_REPO + '", "repository_id": "' + REPO + '"'))
        result = self.rerun_existing_exports()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("duplicate JSON key", result.stderr)

    def test_partial_trusted_identity_is_rejected(self):
        for args in (["--repository-id", REPO], ["--branch", "main"]):
            with self.subTest(args=args):
                result = self.invoke(args=args)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("supplied together", result.stderr)

    def test_nested_duplicate_event_key_is_rejected(self):
        self.assertEqual(self.invoke().returncode, 0)
        path = self.repo / "events.ndjson"
        raw = path.read_text()
        path.write_text(raw.replace('"change_kind": "modify"',
                                    '"change_kind": "delete", "change_kind": "modify"'))
        result = self.rerun_existing_exports()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("duplicate JSON key", result.stderr)

    def test_nonfinite_manifest_export_values_fail_closed(self):
        for constant in ("NaN", "Infinity", "-Infinity", "1e999", "-1e999"):
            with self.subTest(constant=constant):
                self.assertEqual(self.invoke().returncode, 0)
                path = self.repo / "manifest.ndjson"
                raw = path.read_text()
                path.write_text(raw.replace('"files": [', '"unconsumed": {"nested": ' + constant + '}, "files": ['))
                result = self.rerun_existing_exports()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("non-finite JSON", result.stderr)

    def test_nonfinite_event_export_values_fail_closed(self):
        for constant in ("NaN", "Infinity", "-Infinity", "1e999", "-1e999"):
            with self.subTest(constant=constant):
                self.assertEqual(self.invoke().returncode, 0)
                path = self.repo / "events.ndjson"
                raw = path.read_text()
                path.write_text(raw.replace('"change_kind": "modify"',
                                            '"unconsumed": [' + constant + '], "change_kind": "modify"'))
                result = self.rerun_existing_exports()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("non-finite JSON", result.stderr)

    def test_git_replace_cannot_forge_trusted_commit_tree(self):
        # A local Git replacement ref can make the original SHA appear to
        # contain different source bytes unless replacement lookup is disabled.
        git(self.repo, "checkout", "-q", self.parent)
        (self.repo / "before.py").write_text("x = 999\n")
        (self.repo / "new.rs").write_text("fn new() {}\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "forged alternate tree")
        forged_commit = git(self.repo, "rev-parse", "HEAD")
        git(self.repo, "checkout", "-q", self.target)
        git(self.repo, "replace", self.target, forged_commit)
        forged = [dict(item) for item in self.manifest]
        forged[0]["content_hash"] = hashlib.sha256(b"x = 999\n").hexdigest()
        result = self.invoke(manifests=[self.page("files", forged)])
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("stored content hash mismatch", result.stderr)

    def test_grafted_unrelated_parent_cannot_pass_trusted_bundle(self):
        # Manufacture a distinct root with identical source to the real
        # parent. Git's deprecated grafts file can forge ancestry without
        # changing either tree, even with GIT_NO_REPLACE_OBJECTS enabled.
        git(self.repo, "checkout", "--orphan", "unrelated")
        git(self.repo, "rm", "-rf", "-q", ".")
        (self.repo / "before.py").write_text("x = 1\n")
        (self.repo / "stable.go").write_text("package stable\n")
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", "unrelated root")
        unrelated = git(self.repo, "rev-parse", "HEAD")
        git(self.repo, "checkout", "-q", self.target)
        (self.repo / ".git" / "info" / "grafts").write_text(
            f"{self.target} {unrelated}\n")
        result = self.invoke(
            manifests=[self.page("files", self.manifest, parent_commit_id=unrelated)],
            events=[self.page("events", self.events, parent_commit_id=unrelated)],
            args=["--parent", unrelated],
        )
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("graft", result.stderr.lower())

    def test_forged_stored_hash_fails(self):
        forged = [dict(x) for x in self.manifest]
        forged[0]["content_hash"] = hashlib.sha256(b"forged").hexdigest()
        self.assertNotEqual(self.invoke(manifests=[self.page("files", forged)]).returncode, 0)

    def test_missing_event_fails(self):
        self.assertNotEqual(
            self.invoke(events=[self.page("events", self.events[:1])]).returncode, 0)

    def test_parent_equal_to_target_fails(self):
        result = self.invoke(args=["--parent", self.target])
        self.assertNotEqual(result.returncode, 0, result.stdout)

    def test_incomplete_manifest_page_fails(self):
        result = self.invoke(manifests=[
            self.page("files", self.manifest[:1], next_after_path="before.py")])
        self.assertNotEqual(result.returncode, 0, result.stdout)


if __name__ == "__main__":
    unittest.main()
