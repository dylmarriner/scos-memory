#!/usr/bin/env python3
"""Offline compare authenticated SCOS snapshot file hashes with actual Git bytes.

Input is a complete NDJSON export of /v2/code/snapshots/manifest pages.
The Git repository and export are explicit local inputs. No SCOS access,
network calls, Git writes, or production changes are performed.

This proves the *exported stored hashes* match Git bytes, not that the
server actually parsed those bytes, or that the export is a signed receipt.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import uuid

from scos_git_worklist import LANGUAGE_SUFFIXES, WorklistError, commit_oid, git_env, tree_blobs


def read_pages(path: Path, snapshot_id: str, commit_id: str) -> dict[str, tuple[str, str]]:
    files: dict[str, tuple[str, str]] = {}
    last_path: str | None = None
    binding: tuple[str, str, str] | None = None
    count = 0
    with path.open(encoding="utf-8") as stream:
        for number, line in enumerate(stream, 1):
            if not line.strip():
                continue
            count += 1
            page = json.loads(line)
            if not isinstance(page, dict) or page.get("snapshot_id") != snapshot_id:
                raise WorklistError(f"page {number} has incorrect snapshot_id")
            if page.get("commit_id") != commit_id:
                raise WorklistError(f"page {number} has wrong or missing Git commit binding")
            repo, branch, parent = (page.get("repository_id"),
                                    page.get("branch"), page.get("parent_commit_id"))
            try:
                if not isinstance(repo, str) or str(uuid.UUID(repo)) != repo:
                    raise ValueError("invalid repository UUID")
            except (ValueError, AttributeError):
                raise WorklistError(f"page {number} has invalid repository_id")
            if not isinstance(branch, str) or not branch.strip() or not isinstance(parent, str):
                raise WorklistError(f"page {number} has invalid branch or parent_commit_id")
            identity = (repo, branch, parent)
            if binding is not None and identity != binding:
                raise WorklistError(f"page {number} changed repository, branch or parent binding")
            binding = identity
            items = page.get("files")
            if not isinstance(items, list):
                raise WorklistError(f"page {number} missing files array")
            for item in items:
                if not isinstance(item, dict):
                    raise WorklistError("manifest file must be an object")
                name, language, digest = (item.get("path"), item.get("language"),
                                          item.get("content_hash"))
                if not isinstance(name, str) or not name or "\x00" in name:
                    raise WorklistError("invalid manifest path")
                if last_path is not None and name.encode() <= last_path.encode():
                    raise WorklistError(f"duplicate or unordered manifest path: {name!r}")
                if not isinstance(language, str) or not isinstance(digest, str):
                    raise WorklistError(f"invalid SHA-256 or language for {name!r}")
                normalized = digest.strip().lower()
                if not re.fullmatch(r"[0-9a-f]{64}", normalized):
                    raise WorklistError(f"invalid SHA-256 for {name!r}")
                files[name] = (language, normalized)
                last_path = name
            cursor = page.get("next_after_path", "")
            if not isinstance(cursor, str):
                raise WorklistError("invalid cursor")
            # A nonempty cursor must be the final path of this page. A final
            # page with a cursor is never a complete export.
            if cursor and (not items or cursor != last_path):
                raise WorklistError(f"page {number} has incorrect cursor")
            if cursor:
                pending = True
            else:
                pending = False
                # No pages are permitted after a terminal page.
                if any(tail.strip() for tail in stream):
                    raise WorklistError("extra pages after terminal cursor")
                break
    if not count:
        raise WorklistError("empty manifest export")
    if pending:
        raise WorklistError("incomplete manifest export: outstanding cursor")
    return files


def hash_git_blobs(repo: Path, blobs: dict[str, str]) -> dict[str, str]:
    """Stream Git blobs through one cat-file process, bounded in memory.

    Batch protocol is binary and length-prefixed, so embedded newlines and
    unusual UTF-8 filenames cannot confuse content boundaries.
    """
    try:
        process = subprocess.Popen(
            ["git", "cat-file", "--batch"], cwd=repo, env=git_env(),
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        )
    except OSError as exc:
        raise WorklistError(f"unable to execute git cat-file: {exc}") from exc
    digests: dict[str, str] = {}
    try:
        assert process.stdin is not None and process.stdout is not None
        for path, oid in sorted(blobs.items(), key=lambda item: item[0].encode()):
            process.stdin.write((oid + "\n").encode("ascii"))
            process.stdin.flush()
            header = process.stdout.readline().strip().split(b" ")
            if len(header) != 3 or header[0] != oid.encode("ascii") or header[1] != b"blob":
                raise WorklistError(f"invalid Git blob response for {path!r}")
            try:
                remaining = int(header[2])
            except ValueError as exc:
                raise WorklistError(f"invalid Git blob size for {path!r}") from exc
            if remaining < 0:
                raise WorklistError(f"invalid negative Git blob size for {path!r}")
            digest = hashlib.sha256()
            while remaining:
                data = process.stdout.read(min(remaining, 64 * 1024))
                if not data:
                    raise WorklistError(f"truncated Git blob for {path!r}")
                digest.update(data)
                remaining -= len(data)
            if process.stdout.read(1) != b"\n":
                raise WorklistError(f"invalid Git blob delimiter for {path!r}")
            digests[path] = digest.hexdigest()
        process.stdin.close()
        if process.wait() != 0:
            raise WorklistError("git cat-file failed")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
    return digests


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--git-dir", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--snapshot-id", required=True)
    parser.add_argument("--pages-file", type=Path, required=True)
    args = parser.parse_args()
    try:
        if str(uuid.UUID(args.snapshot_id)) != args.snapshot_id:
            raise WorklistError("snapshot_id must be a canonical UUID")
        repo = args.git_dir.resolve()
        commit = commit_oid(repo, args.commit)
        manifest = read_pages(args.pages_file, args.snapshot_id, commit)
        blobs = tree_blobs(repo, commit)
        if manifest.keys() != blobs.keys():
            missing = sorted(blobs.keys() - manifest.keys())
            extra = sorted(manifest.keys() - blobs.keys())
            raise WorklistError(f"manifest path mismatch: missing={missing[:10]!r}, extra={extra[:10]!r}")
        for path, (language, _) in manifest.items():
            if language != LANGUAGE_SUFFIXES[Path(path).suffix.lower()]:
                raise WorklistError(f"manifest language mismatch for {path!r}")
        actual = hash_git_blobs(repo, blobs)
        for path, (_, stored_hash) in manifest.items():
            if actual[path] != stored_hash:
                raise WorklistError(f"stored content hash mismatch against Git blob for {path!r}")
    except (WorklistError, ValueError, OSError, UnicodeError, json.JSONDecodeError) as exc:
        print(f"scos_verify_snapshot_manifest: {exc}", file=sys.stderr)
        return 1
    print(json.dumps({
        "status": "stored_manifest_matches_git_bytes",
        "snapshot_id": args.snapshot_id,
        "commit_id": commit,
        "file_count": len(manifest),
        "limitation": "authenticated_export_not_signed_or_server_git_tree_attested",
    }))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
