#!/usr/bin/env python3
"""Real-Git fixtures for offline snapshot-event ledger consistency checks."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("scos_verify_snapshot_events.py")
SNAPSHOT = "11111111-1111-4111-8111-111111111111"


def git(repo, *args):
    return subprocess.run(["git", *args], cwd=repo, check=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.decode().strip()


class SnapshotEventVerificationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.name", "SCOS Fixture")
        git(self.repo, "config", "user.email", "fixture@example.invalid")

    def commit(self, name):
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", name)
        return git(self.repo, "rev-parse", "HEAD")

    def invoke(self, target, events, parent=None, snapshot=SNAPSHOT, pages=None):
        page_data = pages if pages is not None else [
            {"snapshot_id": snapshot, "events": events}]
        path = self.repo / "exported-pages.ndjson"
        path.write_text("\n".join(json.dumps(page) for page in page_data) + "\n")
        args = [sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
                "--commit", target, "--snapshot-id", snapshot,
                "--pages-file", str(path)]
        args += ["--parent", parent] if parent else ["--initial"]
        return subprocess.run(args, text=True, capture_output=True)

    def fixture(self):
        (self.repo / "before.py").write_text("value = 1\n")
        (self.repo / "changed.go").write_text("package main\nfunc old(){}\n")
        (self.repo / "remove.rs").write_text("fn gone() {}\n")
        (self.repo / "stable.py").write_text("stable = 42\n")
        parent = self.commit("parent")
        (self.repo / "before.py").rename(self.repo / "after.py")
        (self.repo / "changed.go").write_text("package main\nfunc newer(){}\n")
        (self.repo / "remove.rs").unlink()
        (self.repo / "added.py").write_text("added = True\n")
        (self.repo / "copy.py").write_text("stable = 42\n")
        target = self.commit("target")
        events = [
            {"change_kind": "add", "path": "added.py", "evidence_class": "snapshot_manifest"},
            {"change_kind": "rename", "path": "after.py", "previous_path": "before.py",
             "evidence_class": "unambiguous_content"},
            {"change_kind": "modify", "path": "changed.go", "evidence_class": "exact_path"},
            {"change_kind": "copy", "path": "copy.py", "previous_path": "stable.py",
             "evidence_class": "unambiguous_content"},
            {"change_kind": "delete", "path": "remove.rs", "evidence_class": "snapshot_manifest"},
        ]
        return parent, target, events

    def test_complete_multi_page_ledger_matches_committed_git_transition(self):
        parent, target, events = self.fixture()
        pages = [
            {"snapshot_id": SNAPSHOT, "events": events[:2], "next_after_path": "after.py"},
            {"snapshot_id": SNAPSHOT, "events": events[2:]},
        ]
        # Endpoint orders by path; test must use the same ordering.
        events.sort(key=lambda e: e["path"].encode())
        pages[0]["events"] = events[:2]
        pages[0]["next_after_path"] = events[1]["path"]
        pages[1]["events"] = events[2:]
        result = self.invoke(target, events, parent, pages=pages)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["event_count"], 5)

    def test_missing_deletion_is_rejected(self):
        parent, target, events = self.fixture()
        events = [e for e in events if e["change_kind"] != "delete"]
        result = self.invoke(target, events, parent)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing", result.stderr.lower())

    def test_forged_rename_source_is_rejected(self):
        parent, target, events = self.fixture()
        for event in events:
            if event["change_kind"] == "rename":
                event["previous_path"] = "stable.py"
        result = self.invoke(target, events, parent)
        self.assertNotEqual(result.returncode, 0)

    def test_duplicate_event_path_is_rejected(self):
        parent, target, events = self.fixture()
        result = self.invoke(target, events + [events[0]], parent)
        self.assertNotEqual(result.returncode, 0)

    def test_incomplete_pagination_is_rejected(self):
        parent, target, events = self.fixture()
        events.sort(key=lambda e: e["path"].encode())
        pages = [{"snapshot_id": SNAPSHOT, "events": events[:1],
                  "next_after_path": events[0]["path"]}]
        result = self.invoke(target, events, parent, pages=pages)
        self.assertNotEqual(result.returncode, 0)

    def test_wrong_snapshot_id_is_rejected(self):
        parent, target, events = self.fixture()
        pages = [{"snapshot_id": "22222222-2222-4222-8222-222222222222", "events": events}]
        result = self.invoke(target, events, parent, pages=pages)
        self.assertNotEqual(result.returncode, 0)

    def test_ambiguous_duplicate_content_cannot_be_called_rename(self):
        (self.repo / "one.py").write_text("same = 1\n")
        (self.repo / "two.py").write_text("same = 1\n")
        parent = self.commit("parent")
        (self.repo / "one.py").rename(self.repo / "three.py")
        target = self.commit("target")
        events = [{"change_kind": "rename", "path": "three.py",
                   "previous_path": "one.py", "evidence_class": "unambiguous_content"}]
        result = self.invoke(target, events, parent)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("uniquely", result.stderr)

    def test_forged_copy_from_modified_source_is_rejected(self):
        (self.repo / "source.py").write_text("old = 1\n")
        parent = self.commit("parent")
        (self.repo / "source.py").write_text("new = 2\n")
        (self.repo / "copy.py").write_text("old = 1\n")
        target = self.commit("target")
        events = [
            {"change_kind": "copy", "path": "copy.py", "previous_path": "source.py",
             "evidence_class": "unambiguous_content"},
            {"change_kind": "modify", "path": "source.py", "evidence_class": "exact_path"},
        ]
        result = self.invoke(target, events, parent)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unchanged", result.stderr)

    def test_unchanged_path_must_not_have_modify_event(self):
        (self.repo / "stable.py").write_text("same = 1\n")
        parent = self.commit("parent")
        (self.repo / "README.md").write_text("not indexed")
        target = self.commit("target")
        result = self.invoke(target, [
            {"change_kind": "modify", "path": "stable.py", "evidence_class": "exact_path"},
        ], parent)
        self.assertNotEqual(result.returncode, 0)

    def test_initial_commit_additions_are_verified(self):
        (self.repo / "ü space.py").write_text("x = 1\n")
        target = self.commit("initial")
        result = self.invoke(target, [
            {"change_kind": "add", "path": "ü space.py",
             "evidence_class": "snapshot_manifest"}])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_misleading_evidence_class_is_rejected(self):
        parent, target, events = self.fixture()
        events[0]["evidence_class"] = "unambiguous_content"
        result = self.invoke(target, events, parent)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
