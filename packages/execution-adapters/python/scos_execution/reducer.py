"""Python execution proxy output reducer and command safety policy (§20.6).

Mirrors packages/execution-adapters (Go): commands are parsed as POSIX shell
and refused when any simple command in them is destructive at the system
level, including commands nested in sh -c / eval / $(...) or behind
sudo/env/nohup-style wrappers.
"""

from __future__ import annotations

import posixpath
import re
import shlex
from dataclasses import dataclass


@dataclass
class CommandRewrite:
    original_command: str
    rewritten_command: str
    is_rewritten: bool
    reason: str = ""


class CommandBlockedError(ValueError):
    """Raised for a command that must not run at all."""


REDUCTION_MARKER = "\n... [output reduced by execution-proxy] ...\n"
MAX_NESTING = 4

_BLOCK_DEVICE = re.compile(r"^/dev/(sd[a-z]+|hd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|mmcblk\d+|disk\d+|dm-\d+|md\d+)")
_FUNC_DEF = re.compile(r"([A-Za-z_:][\w:]*)\(\)\{([^}]*)\}")
_DURATION = re.compile(r"^\d+(\.\d+)?[smhd]?$")
_SUBSTITUTION = re.compile(r"\$\(((?:[^()]|\([^()]*\))*)\)|`([^`]*)`")
_CRITICAL = {
    "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc",
    "/root", "/sbin", "/srv", "/sys", "/usr", "/var", "~", "$HOME", "${HOME}",
}
_WRAPPERS = {"sudo", "doas", "env", "nohup", "time", "command", "exec", "nice", "ionice", "busybox", "stdbuf", "timeout"}
_SHELLS = {"sh", "bash", "zsh", "dash", "ksh"}
_SEPARATORS = {";", "&", "&&", "|", "||", ";;", "|&", "(", ")"}
_REDIRECTS = {">", ">>", "1>", "2>", ">|"}


def _is_critical(target: str) -> bool:
    t = target.rstrip("/")
    if t == "":
        return True
    if t.endswith("/*"):
        t = t[:-2]
    if t in ("", "*"):
        return True
    if t in _CRITICAL:
        return True
    return t.startswith("/") and posixpath.normpath(t) in _CRITICAL


def _is_fork_bomb(cmdline: str) -> bool:
    compact = "".join(cmdline.split())
    return any(f"{m.group(1)}|{m.group(1)}&" in m.group(2) for m in _FUNC_DEF.finditer(compact))


def _split_commands(cmdline: str) -> list[list[str]]:
    lexer = shlex.shlex(cmdline.replace("\n", ";"), posix=True, punctuation_chars=";&|()<>")
    lexer.whitespace_split = True
    lexer.commenters = ""
    commands: list[list[str]] = []
    words: list[str] = []
    for token in lexer:  # raises ValueError on unbalanced quotes
        if token in _SEPARATORS:
            if words:
                commands.append(words)
            words = []
        else:
            words.append(token)
    if words:
        commands.append(words)
    return commands


def _strip_wrappers(words: list[str]) -> list[str]:
    while words:
        w = words[0]
        if "=" in w and not w.startswith(("=", "-")):
            words = words[1:]
        elif posixpath.basename(w) in _WRAPPERS:
            name = posixpath.basename(w)
            words = words[1:]
            while words and (words[0].startswith("-") or (name == "timeout" and _DURATION.match(words[0]))):
                words = words[1:]
        else:
            break
    return words


def _command_reason(words: list[str], depth: int) -> str:
    for i, w in enumerate(words):
        target = ""
        if w in _REDIRECTS and i + 1 < len(words):
            target = words[i + 1]
        elif w.startswith(">"):
            target = w.lstrip(">")
        if _BLOCK_DEVICE.match(target):
            return f"redirect onto block device {target}"
    words = _strip_wrappers(words)
    if not words:
        return ""
    name, args = posixpath.basename(words[0]), words[1:]
    if name in _SHELLS:
        for i, a in enumerate(args):
            if (a == "-c" or (a.startswith("-") and not a.startswith("--") and "c" in a)) and i + 1 < len(args):
                return destructive_reason(args[i + 1], depth + 1)
    elif name == "eval":
        return destructive_reason(" ".join(args), depth + 1)
    elif name == "rm":
        recursive, targets = False, []
        for a in args:
            if a == "--no-preserve-root":
                return "rm --no-preserve-root"
            if a == "--recursive" or (a.startswith("-") and not a.startswith("--") and set(a) & {"r", "R"}):
                recursive = True
            elif not a.startswith("-"):
                targets.append(a)
        if recursive:
            for t in targets:
                if _is_critical(t):
                    return f"recursive delete of {t}"
    elif name in ("chmod", "chown", "chgrp"):
        recursive = any(a in ("-R", "--recursive") or (a.startswith("-") and not a.startswith("--") and "R" in a) for a in args)
        targets = [a for a in args if not a.startswith("-")]
        if recursive:
            for t in targets[1:]:
                if _is_critical(t):
                    return f"recursive {name} of {t}"
    elif name in ("mkfs", "mke2fs", "mkswap", "wipefs") or name.startswith("mkfs."):
        return f"{name} formats a filesystem"
    elif name == "dd":
        for a in args:
            if a.startswith("of=") and _BLOCK_DEVICE.match(a[3:]):
                return f"dd onto block device {a[3:]}"
    elif name == "shred":
        for a in args:
            if _BLOCK_DEVICE.match(a):
                return f"shred of block device {a}"
    return ""


def destructive_reason(cmdline: str, depth: int = 0) -> str:
    """Why cmdline must not run, or "" when it is allowed."""
    if depth > MAX_NESTING:
        return "command nesting too deep to verify"
    if _is_fork_bomb(cmdline):
        return "fork bomb"
    for m in _SUBSTITUTION.finditer(cmdline):
        if reason := destructive_reason(m.group(1) if m.group(1) is not None else m.group(2), depth + 1):
            return reason
    try:
        commands = _split_commands(cmdline)
    except ValueError as error:
        return f"unparseable command: {error}"
    for words in commands:
        if reason := _command_reason(words, depth):
            return reason
    return ""


class OutputReducer:
    def rewrite_command(self, command: str) -> CommandRewrite:
        """Return command unchanged when safe; raise CommandBlockedError
        when it is destructive."""
        if reason := destructive_reason(command):
            raise CommandBlockedError(f"command blocked by execution safety policy: {reason}")
        return CommandRewrite(original_command=command, rewritten_command=command, is_rewritten=False)

    def reduce_output(self, output: str, max_bytes: int = 2048) -> str:
        """Keep head and tail within max_bytes of UTF-8, cutting on
        character boundaries."""
        raw = output.encode("utf-8")
        if max_bytes <= 0 or len(raw) <= max_bytes:
            return output
        half = max_bytes // 2
        head = raw[:half].decode("utf-8", "ignore")
        tail = raw[len(raw) - half:].decode("utf-8", "ignore")
        return head + REDUCTION_MARKER + tail
