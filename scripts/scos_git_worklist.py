#!/usr/bin/env python3
"""Build a bounded, deterministic SCOS /v2/code/index payload from Git objects.

The full target *manifest* is necessary because omitting a file means deletion
to the indexer. Unchanged same-path blob objects omit source content to let the
database reuse immutable declarations from its already-indexed parent snapshot.

This tool never reads uncommitted files, contacts services, or mutates Git.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
import uuid
from pathlib import Path

LANGUAGE_SUFFIXES = {
    ".go": "go",
    ".py": "python",
    ".js": "javascript",
    ".mjs": "javascript",
    ".cjs": "javascript",
    ".ts": "typescript",
    ".mts": "typescript",
    ".cts": "typescript",
    ".rs": "rust",
}
DEFAULT_LIMIT = 4 << 20


class WorklistError(Exception):
    pass


def git(repo: Path, *arguments: str) -> bytes:
    try:
        process = subprocess.run(
            ["git", *arguments], cwd=repo, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, check=False,
        )
    except OSError as exc:
        raise WorklistError(f"cannot execute git: {exc}") from exc
    if process.returncode != 0:
        description = process.stderr.decode("utf-8", errors="replace").strip()
        raise WorklistError(f"git {' '.join(arguments[:2])} failed: {description}")
    return process.stdout


def commit_oid(repo: Path, name: str) -> str:
    if not name or "\x00" in name:
        raise WorklistError("commit revision is invalid")
    raw = git(repo, "rev-parse", "--verify", "--end-of-options", name + "^{commit}")
    oid = raw.decode("ascii").strip()
    if not re.fullmatch(r"[a-f0-9]{40}|[a-f0-9]{64}", oid):
        raise WorklistError("Git revision did not resolve to a valid commit object")
    return oid


def tree_blobs(repo: Path, commit: str) -> dict[str, str]:
    """Return path -> git blob oid, rejecting paths JSON cannot round-trip."""
    result: dict[str, str] = {}
    for line in git(repo, "ls-tree", "-rz", "--full-tree", commit).split(b"\0"):
        if not line:
            continue
        try:
            fields, raw_path = line.split(b"\t", 1)
            mode, kind, oid = fields.split(b" ")
            path = raw_path.decode("utf-8", errors="strict")
            sha = oid.decode("ascii")
        except (ValueError, UnicodeError) as exc:
            raise WorklistError("non-UTF-8 or malformed Git tree entry") from exc
        # Skip submodules, symlinks, and tree objects. Do not follow links.
        if kind != b"blob" or mode not in (b"100644", b"100755"):
            continue
        if Path(path).suffix.lower() not in LANGUAGE_SUFFIXES:
            continue
        if path in result:
            raise WorklistError(f"duplicate Git tree file {path!r}")
        result[path] = sha
    return result



def read_blob(repo: Path, oid: str, include_source: bool, cap: int) -> tuple[str, bytes]:
    """Hash committed bytes without materializing unbounded Git blobs.

    Incremental unchanged files are hashed streaming-only. Changed files
    cannot exceed the API's entire request budget, so stop before reading
    more than cap bytes into memory.
    """
    try:
        process = subprocess.Popen(
            ["git", "cat-file", "blob", oid], cwd=repo,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
    except OSError as exc:
        raise WorklistError(f"unable to open Git blob: {exc}") from exc
    digest = hashlib.sha256()
    fragments: list[bytes] = []
    size = 0
    try:
        assert process.stdout is not None
        while True:
            chunk = process.stdout.read(64 * 1024)
            if not chunk:
                break
            size += len(chunk)
            if include_source and size > cap:
                raise WorklistError(
                    f"single changed Git blob exceeds {cap}-byte index request limit"
                )
            digest.update(chunk)
            if include_source:
                fragments.append(chunk)
        assert process.stderr is not None
        details = process.stderr.read().decode("utf-8", errors="replace")
        if process.wait() != 0:
            raise WorklistError(f"git cat-file failed: {details.strip()}")
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate()
    return digest.hexdigest(), b"".join(fragments) if include_source else b""

def build(
    repo: Path, repository_id: str, branch: str, target: str,
    parent: str | None, initial: bool, max_bytes: int,
) -> bytes:
    try:
        repository_id = str(uuid.UUID(repository_id))
    except (ValueError, AttributeError) as exc:
        raise WorklistError("repository-id must be an existing SCOS UUID") from exc
    if not branch.strip() or "\x00" in branch:
        raise WorklistError("branch must be nonempty")
    if max_bytes <= 0 or max_bytes > DEFAULT_LIMIT:
        raise WorklistError("max-bytes must be positive and no more than 4194304")
    if initial == (parent is not None):
        raise WorklistError("select exactly one of --initial and --parent")
    head_oid = commit_oid(repo, target)
    parent_oid = commit_oid(repo, parent) if parent else None
    if parent_oid:
        if parent_oid == head_oid:
            raise WorklistError("parent and target cannot be the same commit")
        process = subprocess.run(
            ["git", "merge-base", "--is-ancestor", parent_oid, head_oid],
            cwd=repo, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        if process.returncode != 0:
            raise WorklistError("parent must be an ancestor of target commit")
    old = tree_blobs(repo, parent_oid) if parent_oid else {}
    new = tree_blobs(repo, head_oid)

    files = []
    for path, blob in sorted(new.items()):
        language = LANGUAGE_SUFFIXES[Path(path).suffix.lower()]
        unchanged = parent_oid is not None and old.get(path) == blob
        # Git object IDs are not interchangeable with source SHA-256.
        # Streaming avoids materializing arbitrarily large unchanged blobs.
        content_hash, source = read_blob(repo, blob, not unchanged, max_bytes)
        content = ""
        if not unchanged:
            try:
                content = source.decode("utf-8")
            except UnicodeDecodeError as exc:
                raise WorklistError(f"source is not UTF-8: {path!r}") from exc
        files.append({
            "path": path, "language": language, "content_hash": content_hash,
            "content": content,
        })
    if not files:
        raise WorklistError("no supported source files in target commit")

    payload = {
        "repository_id": repository_id,
        "branch": branch,
        "commit_id": head_oid,
        "files": files,
    }
    raw = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    if len(raw) + 1 > max_bytes:
        raise WorklistError(
            f"index request is {len(raw) + 1} bytes, exceeding the "
            f"{max_bytes}-byte server limit; no partial snapshot was emitted"
        )
    return raw


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--git-dir", type=Path, required=True,
                        help="Checkout or Git repository containing committed objects")
    parser.add_argument("--repository-id", required=True,
                        help="Existing tenant-owned SCOS repository UUID")
    parser.add_argument("--branch", required=True, help="SCOS branch name")
    parser.add_argument("--commit", required=True, help="Immutable target Git commit/ref")
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--initial", action="store_true",
                        help="Initial snapshot: transmit all supported code")
    source.add_argument("--parent",
                        help="Prior commit already indexed as the active SCOS snapshot")
    parser.add_argument("--max-bytes", type=int, default=DEFAULT_LIMIT,
                        help="Fail atomically if request exceeds server's 4 MiB maximum")
    args = parser.parse_args()
    try:
        payload = build(args.git_dir.resolve(), args.repository_id, args.branch,
                        args.commit, args.parent, args.initial, args.max_bytes)
    except WorklistError as exc:
        print(f"scos_git_worklist: {exc}", file=sys.stderr)
        return 1
    sys.stdout.buffer.write(payload + b"\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
