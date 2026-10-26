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

    def invoke(self, *args, env=None):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--git-dir", str(self.repo),
             "--repository-id", REPO_ID, "--branch", "main", *args],
            capture_output=True, text=True, env=env,
        )

    def test_foreign_git_dir_environment_does_not_redirect_worklist(self):
        (self.repo / "source.py").write_text("value = 1\n")
        parent = self.commit("parent")
        (self.repo / "source.py").write_text("value = 2\n")
        head = self.commit("head")
        foreign = self.repo / "foreign"
        foreign.mkdir()
        git(foreign, "init", "-q")
        git(foreign, "config", "user.name", "Foreign")
        git(foreign, "config", "user.email", "foreign@example.invalid")
        (foreign / "other.py").write_text("different = True\n")
        git(foreign, "add", "-A")
        git(foreign, "commit", "-qm", "foreign")
        env = os.environ.copy()
        env.update({"GIT_DIR": str(foreign / ".git"),
                    "GIT_WORK_TREE": str(foreign)})
        result = self.invoke("--commit", head, "--parent", parent, env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["files"][0]["content"], "value = 2\n")
        events = self.invoke("--commit", head, "--parent", parent, "--events-only", env=env)
        self.assertEqual(events.returncode, 0, events.stderr)
        self.assertEqual(json.loads(events.stdout)["events"],
                         [{"type": "modify", "path": "source.py"}])

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
        self.assertEqual(data["parent_commit_id"], parent)
        self.assertNotIn("events", data)

    def test_supported_source_renamed_to_unsupported_is_deletion(self):
        (self.repo / "a.py").write_text("print(1)\n")
        parent = self.commit("parent")
        (self.repo / "a.py").rename(self.repo / "a.txt")
        head = self.commit("head")
        result = self.invoke("--commit", head, "--parent", parent, "--events-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["events"],
                         [{"type": "delete", "path": "a.py"}])

    def test_unsupported_type_change_does_not_block_supported_addition(self):
        (self.repo / "README.md").write_text("readme")
        parent = self.commit("parent")
        (self.repo / "README.md").unlink()
        (self.repo / "README.md").symlink_to("missing")
        (self.repo / "new.py").write_text("print(1)\n")
        head = self.commit("head")
        result = self.invoke("--commit", head, "--parent", parent, "--events-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["events"],
                         [{"type": "add", "path": "new.py"}])

    def test_git_event_sidecar_reports_exact_paths_and_changes(self):
        (self.repo / "before.py").write_text("value = 1\n")
        (self.repo / "removed.py").write_text("old = True\n")
        parent = self.commit("parent")
        (self.repo / "before.py").rename(self.repo / "after.py")
        (self.repo / "removed.py").unlink()
        (self.repo / "added.py").write_text("new = True\n")
        head = self.commit("target")
        result = self.invoke("--commit", head, "--parent", parent, "--events-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        events = json.loads(result.stdout)["events"]
        self.assertEqual({(e["type"], e["path"]) for e in events},
                         {("rename", "after.py"), ("delete", "removed.py"),
                          ("add", "added.py")})
        self.assertEqual(next(e for e in events if e["type"] == "rename")["old_path"], "before.py")

    def test_unchanged_source_omitted_with_explicit_reuse_and_full_source_fallback(self):
        (self.repo / "stable.py").write_text("def stable():\n    pass\n")
        parent = self.commit("first")
        (self.repo / "other.go").write_text("package other\n")
        head = self.commit("second")
        result = self.invoke("--commit", head, "--parent", parent)
        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        self.assertEqual(payload["parent_commit_id"], parent)
        by_path = {x["path"]: x for x in payload["files"]}
        self.assertEqual(by_path["stable.py"]["content"], "")
        self.assertTrue(by_path["stable.py"]["reuse_parent"])
        self.assertEqual(len(by_path["stable.py"]["content_hash"]), 64)
        self.assertTrue(by_path["other.go"]["content"])
        self.assertFalse(by_path["other.go"]["reuse_parent"])
        full = self.invoke("--commit", head, "--parent", parent, "--full-source")
        self.assertEqual(full.returncode, 0, full.stderr)
        full_by_path = {x["path"]: x for x in json.loads(full.stdout)["files"]}
        self.assertEqual(full_by_path["stable.py"]["content"], "def stable():\n    pass\n")
        self.assertFalse(full_by_path["stable.py"]["reuse_parent"])

    def test_events_rename_crosses_supported_boundary(self):
        (self.repo / "outside.py").write_text("print('original')\n")
        (self.repo / "inside.txt").write_text("print('incoming')\n")
        parent = self.commit("before extension boundary")
        (self.repo / "outside.py").rename(self.repo / "outside.txt")
        (self.repo / "inside.txt").rename(self.repo / "inside.py")
        head = self.commit("rename across indexed types")
        result = self.invoke("--commit", head, "--parent", parent, "--events-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["events"], [
            {"type": "add", "path": "inside.py"},
            {"type": "delete", "path": "outside.py"},
        ])

    def test_events_file_type_changes_and_irrelevant_types(self):
        (self.repo / "old.py").write_text("print(1)\n")
        (self.repo / "README.md").write_text("not indexed")
        (self.repo / "destination.py").write_text("print(2)\n")
        parent = self.commit("normal")
        (self.repo / "old.py").unlink()
        os.symlink("destination.py", self.repo / "old.py")
        (self.repo / "README.md").unlink()
        os.symlink("destination.py", self.repo / "README.md")
        (self.repo / "new.py").write_text("print(3)\n")
        head = self.commit("symlinks and new")
        result = self.invoke("--commit", head, "--parent", parent, "--events-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["events"], [
            {"type": "add", "path": "new.py"},
            {"type": "delete", "path": "old.py"},
        ])

    def test_events_symlink_becomes_regular_source(self):
        (self.repo / "target.py").write_text("print(9)\n")
        os.symlink("target.py", self.repo / "new.py")
        parent = self.commit("symlink")
        (self.repo / "new.py").unlink()
        (self.repo / "new.py").write_text("print(7)\n")
        head = self.commit("materialize")
        result = self.invoke("--commit", head, "--parent", parent, "--events-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn({"type": "add", "path": "new.py"},
                      json.loads(result.stdout)["events"])

    def test_unchanged_whitespace_source_is_sent_without_parent_reuse(self):
        (self.repo / "blank.py").write_text("  \n\t")
        (self.repo / "empty.py").write_text("")
        (self.repo / "unicode.py").write_text("\u00a0\u2003\n", encoding="utf-8")
        parent = self.commit("blank parent")
        (self.repo / "new.py").write_text("answer = 42\n")
        head = self.commit("new file")
        result = self.invoke("--commit", head, "--parent", parent)
        self.assertEqual(result.returncode, 0, result.stderr)
        files = {f["path"]: f for f in json.loads(result.stdout)["files"]}
        self.assertFalse(files["blank.py"]["reuse_parent"])
        self.assertEqual(files["blank.py"]["content"], "  \n\t")
        self.assertFalse(files["unicode.py"]["reuse_parent"])
        self.assertEqual(files["unicode.py"]["content"], "\u00a0\u2003\n")

    def test_large_unchanged_source_fits_guarded_delta_but_not_full_source(self):
        (self.repo / "stable.py").write_text("data = '" + "a" * 20000 + "'\n")
        parent = self.commit("big parent")
        (self.repo / "new.py").write_text("added = True\n")
        head = self.commit("small delta")
        incremental = self.invoke("--commit", head, "--parent", parent, "--max-bytes", "3000")
        self.assertEqual(incremental.returncode, 0, incremental.stderr)
        self.assertTrue(next(x for x in json.loads(incremental.stdout)["files"] if x["path"] == "stable.py")["reuse_parent"])
        fallback = self.invoke("--commit", head, "--parent", parent, "--full-source", "--max-bytes", "3000")
        self.assertNotEqual(fallback.returncode, 0)
        self.assertEqual(fallback.stdout, "")

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
