#!/usr/bin/env python3
"""Read-only SCOS deployment drift audit against a live remote Git branch.

No fetch, checkout, reset, Docker operation, credentials, or deployment actions.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

_SHA = re.compile(r"[0-9a-f]{40}\Z")


class DriftError(RuntimeError):
    """Safe error for unavailable or untrusted revision evidence."""


def _git(repo: Path, arguments: list[str], runner) -> str:
    command = ["git", "-C", str(repo), *arguments]
    try:
        result = runner(
            command,
            capture_output=True,
            text=True,
            timeout=20,
            env={**os.environ, "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0"},
        )
    except (OSError, subprocess.TimeoutExpired, UnicodeError):
        raise DriftError("Git revision query unavailable or timed out") from None
    if result.returncode != 0:
        # Git may embed secrets and repository URLs in stderr.
        raise DriftError("Git revision query failed; verify repository and remote access")
    return result.stdout.strip()


def audit(repo: str | Path, *, runner=subprocess.run) -> dict[str, object]:
    """Compare live release checkout HEAD to origin/main without modifying either.

    Distinct SHAs prove source mismatch but not the direction or number of commits.
    Only tracked worktree modifications are checked. Container/image parity and
    database health are separate gates.
    """
    try:
        root = Path(repo).expanduser().resolve()
    except (OSError, RuntimeError):
        raise DriftError("Release directory is not accessible") from None
    try:
        if not root.is_dir():
            raise DriftError("Release directory is not accessible")
    except (OSError, RuntimeError):
        raise DriftError("Release directory is not accessible") from None
    reported_root = _git(root, ["rev-parse", "--show-toplevel"], runner)
    try:
        resolved_root = Path(reported_root).resolve()
    except (OSError, RuntimeError):
        raise DriftError("Git repository root is not accessible") from None
    if resolved_root != root:
        raise DriftError("Release path must be the Git repository root")

    local_sha = _git(root, ["rev-parse", "HEAD"], runner)
    if not _SHA.fullmatch(local_sha):
        raise DriftError("Local HEAD is not a valid 40-character Git revision")

    dirty_output = _git(root, ["status", "--porcelain", "--untracked-files=no"], runner)
    remote_reply = _git(root, ["ls-remote", "--heads", "origin", "refs/heads/main"], runner)
    pieces = remote_reply.splitlines()
    if len(pieces) != 1:
        raise DriftError("Expected exactly one live origin/main reference")
    parts = pieces[0].split()
    if (len(parts) != 2 or parts[1] != "refs/heads/main"
            or not _SHA.fullmatch(parts[0])):
        raise DriftError("Remote main reference is malformed or unavailable")
    remote_sha = parts[0]
    clean = not bool(dirty_output)
    same = local_sha == remote_sha
    if same and clean:
        status = "aligned"
    elif same:
        status = "modified"
    elif clean:
        status = "drifted"
    else:
        status = "drifted_modified"
    return {
        "status": status,
        "release_sha": local_sha,
        "target_sha": remote_sha,
        "matches_target": same,
        "clean": clean,
        "source_ref": "origin/main",
    }


def exit_code(result: dict[str, object]) -> int:
    """0=aligned, 1=drift/modified, 2=unknown/error."""
    if result["status"] == "aligned":
        return 0
    if result["status"] == "unknown":
        return 2
    return 1


def main(argv: list[str] | None = None, *, runner=subprocess.run) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, help="Path to deployed Git checkout root")
    options = parser.parse_args(argv)
    try:
        report = audit(options.repo, runner=runner)
    except DriftError as error:
        print(json.dumps({"status": "unknown", "reason": str(error)}, sort_keys=True))
        return 2
    print(json.dumps(report, sort_keys=True))
    return exit_code(report)


if __name__ == "__main__":
    sys.exit(main())
