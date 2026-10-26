#!/usr/bin/env python3
"""Offline consistency verification: exported SCOS snapshot events vs committed Git trees.

This checks complete exported, authenticated event pages against a trusted local
Git parent->target transition. It never contacts SCOS or modifies Git. It does
NOT prove that the server snapshot actually indexes these Git commits: the
event API currently exposes no attested commit/hash manifest.
"""
from __future__ import annotations

import argparse
from collections import Counter
import json
from pathlib import Path
import subprocess
import sys
import uuid

from scos_git_worklist import LANGUAGE_SUFFIXES, WorklistError, commit_oid, git, tree_blobs


def fingerprint_manifest(repo: Path, commit: str | None) -> dict[str, tuple[str, str]]:
    if commit is None:
        return {}
    return {
        path: (oid, LANGUAGE_SUFFIXES[Path(path).suffix.lower()])
        for path, oid in tree_blobs(repo, commit).items()
    }


def read_complete_pages(path: Path, snapshot_id: str) -> list[dict]:
    pages = []
    with path.open(encoding="utf-8") as stream:
        for line in stream:
            if line.strip():
                pages.append(json.loads(line))
    if not pages:
        raise WorklistError("no exported event pages supplied")
    events = []
    last_path = None
    for number, page in enumerate(pages):
        if not isinstance(page, dict) or page.get("snapshot_id") != snapshot_id:
            raise WorklistError(f"page {number+1} has a wrong snapshot_id")
        items = page.get("events")
        if not isinstance(items, list):
            raise WorklistError(f"page {number+1} is missing an events array")
        for item in items:
            if not isinstance(item, dict) or not isinstance(item.get("path"), str):
                raise WorklistError("event must have a string path")
            name = item["path"]
            if not name or "\x00" in name or (last_path is not None and
                    name.encode("utf-8") <= last_path.encode("utf-8")):
                raise WorklistError(f"duplicate or out-of-order event path: {name!r}")
            last_path = name
            events.append(item)
        cursor = page.get("next_after_path", "")
        if number < len(pages)-1:
            if not items or cursor != last_path:
                raise WorklistError(f"page {number+1} has invalid pagination cursor")
        elif cursor:
            raise WorklistError("missing final page: export has an outstanding cursor")
    return events


def verify_events(
    old: dict[str, tuple[str, str]],
    new: dict[str, tuple[str, str]],
    events: list[dict],
) -> None:
    old_count = Counter(old.values())
    new_count = Counter(new.values())
    expected_add = set(new) - set(old)
    expected_modify = {p for p in old.keys() & new.keys() if old[p] != new[p]}
    expected_delete = set(old) - set(new)
    seen_add, seen_modify, seen_delete = set(), set(), set()
    for event in events:
        path = event["path"]
        kind = event.get("change_kind")
        previous = event.get("previous_path") or ""
        evidence = event.get("evidence_class")
        if kind == "modify":
            if path not in expected_modify or previous or evidence != "exact_path":
                raise WorklistError(f"invalid modify evidence for {path!r}")
            seen_modify.add(path)
        elif kind == "delete":
            if path not in expected_delete or previous or evidence != "snapshot_manifest":
                raise WorklistError(f"invalid delete evidence for {path!r}")
            if path in seen_delete:
                raise WorklistError(f"duplicate deletion coverage for {path!r}")
            seen_delete.add(path)
        elif kind == "add":
            if path not in expected_add or previous or evidence != "snapshot_manifest":
                raise WorklistError(f"invalid add evidence for {path!r}")
            seen_add.add(path)
        elif kind in ("rename", "copy"):
            if path not in expected_add or previous not in old or evidence != "unambiguous_content":
                raise WorklistError(f"invalid {kind} evidence for {path!r}")
            fp = new[path]
            if old[previous] != fp or old_count[fp] != 1:
                raise WorklistError(f"{kind} origin is not uniquely supported by Git: {path!r}")
            if kind == "rename":
                if previous not in expected_delete or new_count[fp] != 1:
                    raise WorklistError(f"rename origin/target not uniquely supported: {path!r}")
                if previous in seen_delete:
                    raise WorklistError(f"duplicate deletion coverage for {previous!r}")
                seen_delete.add(previous)
            elif new.get(previous) != fp:
                raise WorklistError(f"copy origin is not unchanged in target: {path!r}")
            seen_add.add(path)
        else:
            raise WorklistError(f"unsupported event kind {kind!r} for {path!r}")
    if (seen_add != expected_add or seen_modify != expected_modify or
            seen_delete != expected_delete):
        missing = (expected_add - seen_add) | (expected_modify - seen_modify) | (expected_delete - seen_delete)
        raise WorklistError(f"missing events for Git transition paths: {sorted(missing)!r}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--git-dir", type=Path, required=True)
    parser.add_argument("--commit", required=True, help="Immutable target commit or ref")
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--parent", help="Indexed parent Git commit")
    source.add_argument("--initial", action="store_true", help="No parent snapshot")
    parser.add_argument("--snapshot-id", required=True)
    parser.add_argument("--pages-file", type=Path, required=True,
                        help="NDJSON export of every authenticated /v2/code/snapshots/events response page")
    args = parser.parse_args()
    try:
        if str(uuid.UUID(args.snapshot_id)) != args.snapshot_id:
            raise WorklistError("snapshot_id must be a canonical UUID")
        repo = args.git_dir.resolve()
        target = commit_oid(repo, args.commit)
        parent = commit_oid(repo, args.parent) if args.parent else None
        if parent and subprocess.run(
            ["git", "merge-base", "--is-ancestor", parent, target],
            cwd=repo, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
            check=False,
        ).returncode != 0:
            raise WorklistError("parent must be an ancestor of target")
        old = fingerprint_manifest(repo, parent)
        new = fingerprint_manifest(repo, target)
        events = read_complete_pages(args.pages_file, args.snapshot_id)
        verify_events(old, new, events)
    except (WorklistError, ValueError, OSError, json.JSONDecodeError, UnicodeError) as exc:
        print(f"scos_verify_snapshot_events: {exc}", file=sys.stderr)
        return 1
    print(json.dumps({
        "status": "offline_consistency_verified",
        "snapshot_id": args.snapshot_id,
        "parent_commit_id": parent,
        "commit_id": target,
        "event_count": len(events),
        "limitation": "does_not_attest_server_snapshot_commit_or_source_hashes",
    }))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
