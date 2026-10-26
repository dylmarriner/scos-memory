#!/usr/bin/env python3
"""Verify two complete authenticated SCOS exports describe one trusted Git snapshot.

Combines Git-byte manifest readback and parent->target event-ledger checks.
Both exports must have exactly the same snapshot, repository, branch and Git
commit identities. Offline integrity check, NOT a server-signed attestation.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import subprocess
import sys
import uuid

from scos_git_worklist import LANGUAGE_SUFFIXES, WorklistError, commit_oid, git_env, tree_blobs
from scos_verify_snapshot_events import fingerprint_manifest, read_complete_pages, verify_events
from scos_verify_snapshot_manifest import hash_git_blobs, read_pages


def verify_bundle(
    repo: Path, target: str, parent: str | None, snapshot_id: str,
    manifest_pages: Path, event_pages: Path,
) -> dict:
    if parent is not None:
        if parent == target:
            raise WorklistError("trusted parent must be a strict ancestor of target")
        ancestor = subprocess.run(
            ["git", "merge-base", "--is-ancestor", parent, target],
            cwd=repo, env=git_env(), stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, check=False,
        )
        if ancestor.returncode != 0:
            raise WorklistError("trusted parent must be a strict ancestor of target")

    files, manifest_identity = read_pages(
        manifest_pages, snapshot_id, target, parent or "", return_identity=True)
    events, event_identity = read_complete_pages(
        event_pages, snapshot_id, target, parent, return_identity=True)
    if manifest_identity != event_identity:
        differences = [
            name for name, a, b in zip(
                ("repository", "branch", "parent commit"), manifest_identity, event_identity
            ) if a != b
        ]
        raise WorklistError("manifest/event export identity mismatch: " + ", ".join(differences))

    blobs = tree_blobs(repo, target)
    if files.keys() != blobs.keys():
        missing = sorted(blobs.keys() - files.keys())
        extra = sorted(files.keys() - blobs.keys())
        raise WorklistError(f"manifest path mismatch: missing={missing[:10]!r}, extra={extra[:10]!r}")
    for path, (language, _) in files.items():
        if language != LANGUAGE_SUFFIXES[Path(path).suffix.lower()]:
            raise WorklistError(f"manifest language mismatch for {path!r}")
    actual = hash_git_blobs(repo, blobs)
    for path, (_, stored_hash) in files.items():
        if actual[path] != stored_hash:
            raise WorklistError(f"stored content hash mismatch against Git blob for {path!r}")

    verify_events(fingerprint_manifest(repo, parent),
                  fingerprint_manifest(repo, target), events)
    return {
        "status": "offline_snapshot_bundle_consistent",
        "snapshot_id": snapshot_id,
        "repository_id": manifest_identity[0],
        "branch": manifest_identity[1],
        "parent_commit_id": parent or "",
        "commit_id": target,
        "file_count": len(files),
        "event_count": len(events),
        "limitation": "server_commit_ids_and_exports_are_not_cryptographically_attested",
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--git-dir", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--parent", help="Trusted indexed parent Git revision")
    source.add_argument("--initial", action="store_true", help="Snapshot has no indexed parent")
    parser.add_argument("--snapshot-id", required=True)
    parser.add_argument("--manifest-pages-file", type=Path, required=True)
    parser.add_argument("--event-pages-file", type=Path, required=True)
    args = parser.parse_args()
    try:
        if str(uuid.UUID(args.snapshot_id)) != args.snapshot_id:
            raise WorklistError("snapshot_id must be a canonical UUID")
        if args.parent == "":
            raise WorklistError("trusted parent cannot be empty")
        repo = args.git_dir.resolve()
        target = commit_oid(repo, args.commit)
        parent = commit_oid(repo, args.parent) if args.parent is not None else None
        result = verify_bundle(
            repo, target, parent, args.snapshot_id,
            args.manifest_pages_file, args.event_pages_file)
    except (WorklistError, ValueError, OSError, UnicodeError, json.JSONDecodeError) as exc:
        print(f"scos_verify_snapshot_bundle: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
