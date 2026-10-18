import importlib.util
import json
import os
import pathlib
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("import_agent_memories", ROOT / "scripts/import_agent_memories.py")
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


class UnitsTests(unittest.TestCase):
    def test_bullets_paragraphs_and_noise(self):
        md = """---
name: x
---
# Deploys
- runs from kubuntux backup drive
- Deploys never touch the DEVELOPMENT drive.

A paragraph that
spans two lines.

```
code is not memory
```
| table | row |
§
- [Index link](other.md)
[Dispatchline typography](rule_type.md) — Type uses the brand fonts.
@/home/kubuntux/.codex/RTK.md
<!-- skill-router:managed:start -->
**Status:** the deploy is **green**.
"""
        got = mod.units(md)
        self.assertEqual(got, [
            "Deploys: runs from kubuntux backup drive",
            "Deploys never touch the DEVELOPMENT drive.",
            "A paragraph that spans two lines.",
            "Status: the deploy is green.",
        ])

    def test_long_unit_is_split_at_sentences(self):
        text = " ".join(f"Sentence number {i} says something useful." for i in range(60))
        pieces = mod.units(text)
        self.assertGreater(len(pieces), 1)
        self.assertTrue(all(len(p) <= mod.MAX_UNIT for p in pieces))
        self.assertEqual(" ".join(pieces), text)


class ScopeTests(unittest.TestCase):
    def test_project_scope(self):
        self.assertEqual(mod.project_scope("-media-kubuntux-DEVELOPMENT1-Dispatchline"), "Dispatchline")
        self.assertEqual(mod.project_scope("-run-media-kubuntux-DEVELOPMENT-Maer-ken-island"), "Maer-ken-island")
        self.assertEqual(mod.project_scope("-run-media-kubuntux-backup-dev-ext4-RuView-work"), "RuView-work")
        self.assertEqual(mod.project_scope("-home-kubuntux"), "kubuntux")


class CollectTests(unittest.TestCase):
    def test_collect_reads_known_stores_with_origin_and_dedups(self):
        with tempfile.TemporaryDirectory() as d:
            home = pathlib.Path(d)
            mem = home / ".claude/projects/-media-kubuntux-DEVELOPMENT1-Mealify/memory"
            mem.mkdir(parents=True)
            (mem / "MEMORY.md").write_text("- [Pref](pref.md) — terse\n")
            (mem / "pref.md").write_text("---\nname: pref\ndescription: The user wants terse answers.\ntype: feedback\n---\nKeep replies short.\n")
            (home / ".hermes").mkdir()
            (home / ".hermes/USER.md").write_text("The user lives in New Zealand.\n§\nKeep replies short.\n")
            daily = home / ".openclaw/workspace/memory"
            daily.mkdir(parents=True)
            (daily / "2026-09-03.md").write_text("- Set up the Pi 5 backup target.\n")
            (home / ".openclaw/workspace/SOUL.md").write_text("You are a persona.\n")
            items = list(mod.collect(home, "box"))
        by = {i["content"]: i for i in items}
        self.assertIn("The user wants terse answers.", by)
        pref = by["The user wants terse answers."]
        self.assertEqual(pref["network"], "procedure")
        self.assertEqual(pref["scope"], "Mealify")
        self.assertTrue(pref["origin"].startswith("claude-code@box:~/.claude/projects/"))
        self.assertEqual(by["The user lives in New Zealand."]["network"], "entity")
        self.assertEqual(by["Set up the Pi 5 backup target."]["observed_at"], "2026-09-03T00:00:00Z")
        self.assertEqual(by["Set up the Pi 5 backup target."]["network"], "experience")
        self.assertNotIn("You are a persona.", by)
        self.assertEqual(sum(1 for i in items if i["content"] == "Keep replies short."), 1)
        self.assertFalse(any("Index" in i["content"] or "Pref" in i["content"] for i in items))
        json.dumps(items)


if __name__ == "__main__":
    unittest.main()
