#!/usr/bin/env python3
"""End-to-end fixture tests for the P3 Git-to-SCOS indexing payload."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("scos_git_worklist.py")
REPO_ID = "11111111-1111-1111-1111-111111111111"


def git(directory, *args, input_data=None):
    return subprocess.run(
        ["git", *args], cwd=directory, input=input_data,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True,
    ).stdout.decode("utf-8").strip()


class GitWorklistTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.name", "SCOS Tester")
        git(self.repo, "config", "user.email", "scos@example.invalid")

    def commit(self, message):
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-qm", message)
        return git(self.repo, "rev-parse", "HEAD")

    def invoke(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
             "--repository-id", REPO_ID, "--branch", "main", *args],
            capture_output=True, text=True,
        )

    def test_initial_then_incremental_including_renames_and_deletions(self):
        (self.repo / "main.go").write_text("package demo\nfunc main(){}\n")
        (self.repo / "old.py").write_text("def old():\n    return 5\n")
        (self.repo / "remove.rs").write_text("fn obsolete() {}\n")
        (self.repo / "ignored.md").write_text("Not a source file")
        parent = self.commit("parent")
        initial = self.invoke("--commit", parent, "--initial")
        self.assertEqual(initial.returncode, 0, initial.stderr)
        baseline = json.loads(initial.stdout)
        self.assertEqual(baseline["commit_id"], parent)
        self.assertEqual({x["path"] for x in baseline["files"]},
                         {"main.go", "old.py", "remove.rs"})
        self.assertTrue(all(x["content"] for x in baseline["files"]))
        (self.repo / "old.py").rename(self.repo / "renamed.py")
        (self.repo / "remove.rs").unlink()
        (self.repo / "file with spaces ü.go").write_text("package demo\nfunc added(){}\n")
        (self.repo / "main.go").write_text("package demo\nfunc main(){println(2)}\n")
        head = self.commit("head")
        result = self.invoke("--commit", head, "--parent", parent)
        self.assertEqual(result.returncode, 0, result.stderr)
        data = json.loads(result.stdout)
        files = {x["path"]: x for x in data["files"]}
        self.assertEqual(set(files), {"main.go", "renamed.py", "file with spaces ü.go"})
        self.assertEqual(files["main.go"]["content"], "package demo\nfunc main(){println(2)}\n")
        self.assertEqual(files["renamed.py"]["content"], "def old():\n    return 5\n")
        self.assertNotIn("remove.rs", files)
        self.assertEqual(len(files), len(set(files)))
        self.assertEqual(data["repository_id"], REPO_ID)
        self.assertNotIn("parent", data)
        self.assertNotIn("events", data)

    def test_unchanged_content_is_omitted_but_hash_still_present(self):
        (self.repo / "stable.py").write_text("def stable():\n    pass\n")
        parent = self.commit("first")
        (self.repo / "other.go").write_text("package other\n")
        head = self.commit("second")
        result = self.invoke("--commit", head, "--parent", parent)
        self.assertEqual(result.returncode, 0, result.stderr)
        by_path = {x["path"]: x for x in json.loads(result.stdout)["files"]}
        self.assertEqual(by_path["stable.py"]["content"], "")
        self.assertEqual(len(by_path["stable.py"]["content_hash"]), 64)
        self.assertTrue(by_path["other.go"]["content"])

    def test_commit_snapshot_not_working_tree_and_safe_file_names(self):
        filename = "-weird\nname.py"
        (self.repo / filename).write_text("def original():\n    pass\n")
        base = self.commit("first")
        (self.repo / filename).write_text("def changed():\n    pass\n")
        result = self.invoke("--commit", base, "--initial")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["files"][0]["path"], filename)
        self.assertIn("original", json.loads(result.stdout)["files"][0]["content"])

    def test_rejects_wrong_parent_and_bounded_payload(self):
        (self.repo / "big.py").write_text("x = '" + "a" * 2800 + "'\n")
        base = self.commit("first")
        (self.repo / "new.py").write_text("new = True\n")
        newer = self.commit("second")
        wrong = self.invoke("--commit", base, "--parent", newer)
        self.assertNotEqual(wrong.returncode, 0)
        self.assertEqual(wrong.stdout, "")
        too_big = self.invoke("--commit", newer, "--initial", "--max-bytes", "500")
        self.assertNotEqual(too_big.returncode, 0)
        self.assertEqual(too_big.stdout, "")

    def test_large_source_rejected_before_emitting_partial_json(self):
        (self.repo / "huge.py").write_text("data = '" + "a" * (5 << 20) + "'\\n")
        target = self.commit("large committed source")
        response = self.invoke("--commit", target, "--initial")
        self.assertNotEqual(response.returncode, 0)
        self.assertEqual(response.stdout, "")
        self.assertIn("index request limit", response.stderr)

    def test_unsupported_languages_and_symlinks_are_not_indexed(self):
        (self.repo / "known.go").write_text("package known\n")
        (self.repo / "unsupported.tsx").write_text("const JSX = () => <div />;\n")
        os.symlink("known.go", self.repo / "alias.go")
        head = self.commit("initial")
        r = self.invoke("--commit", head, "--initial")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual([x["path"] for x in json.loads(r.stdout)["files"]], ["known.go"])


if __name__ == "__main__":
    unittest.main()
