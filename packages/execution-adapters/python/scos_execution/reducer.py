"""Python execution proxy output reducer and command sanitizer (§20.6).

Command risk is graded by the same policy file the Go package embeds
(``packages/execution-adapters/risk_policy.json``) with the same algorithm;
both are tested against ``risk_cases.json``.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

RISK_READ = "read"
RISK_WRITE = "write"
RISK_TEST = "test"
RISK_DESTRUCTIVE = "destructive"

POLICY_PATH = Path(__file__).resolve().parents[2] / "risk_policy.json"


@dataclass
class CommandRewrite:
    original_command: str
    rewritten_command: str
    is_rewritten: bool
    reason: str = ""


_RANK = {RISK_READ: 0, RISK_TEST: 1, RISK_WRITE: 2, RISK_DESTRUCTIVE: 3}
_ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")

# Stands in for a command name produced by a command substitution.
_DYNAMIC_COMMAND_NAME = "$(\u2026)"


@lru_cache(maxsize=1)
def _policy() -> dict:
    policy = json.loads(POLICY_PATH.read_text(encoding="utf-8"))
    if not policy.get("destructive_programs") or not policy.get("read_programs") or policy.get("max_depth", 0) <= 0:
        raise RuntimeError(f"{POLICY_PATH} is missing destructive or read programs or max_depth")
    return policy


def _max(a: str, b: str) -> str:
    return b if _RANK[b] > _RANK[a] else a


def classify_risk(command: str) -> str:
    """Grade a shell command as read, test, write or destructive.

    The same fail-closed algorithm as ClassifyRisk in the Go package: a
    command is read only when every part of it runs a program the policy
    names as read-only; any program the policy does not name is write.
    """
    return _classify(_policy(), command.strip(), 0)


def _classify(p: dict, command: str, depth: int) -> str:
    if depth > p["max_depth"]:
        return RISK_DESTRUCTIVE
    if any(m in command for m in p["fork_bomb_markers"]):
        return RISK_DESTRUCTIVE
    grade = RISK_READ
    for words, redirects in _lex_shell(command):
        grade = _max(grade, _classify_segment(p, words, redirects, depth))
    return grade


def _classify_segment(p: dict, words: list[str], redirects: list[str], depth: int) -> str:
    grade = RISK_READ
    for target in redirects:
        if any(target.startswith(prefix) for prefix in p["block_device_prefixes"]):
            return RISK_DESTRUCTIVE
        if target.startswith("&") or target in p["harmless_redirect_targets"]:
            continue
        grade = _max(grade, RISK_WRITE)
    return _max(grade, _classify_words(p, words, depth))


def _classify_words(p: dict, words: list[str], depth: int) -> str:
    if depth > p["max_depth"]:
        return RISK_DESTRUCTIVE
    while words and _ASSIGNMENT.match(words[0]):
        words = words[1:]
    if not words:
        return RISK_READ
    # A program named by expansion ($x, ${RM:-rm}, a command substitution) is
    # only known when the shell runs it, so it is graded as the worst it
    # could be.
    if "$" in words[0] or "`" in words[0]:
        return RISK_DESTRUCTIVE
    prog, args = words[0].rsplit("/", 1)[-1].lower() or words[0], words[1:]
    if prog in p["privileged_launchers"]:
        return RISK_DESTRUCTIVE
    # eval runs its arguments as a command line.
    if prog == "eval":
        if not args:
            return RISK_READ
        return _max(RISK_READ, _classify(p, " ".join(args), depth + 1))
    if prog in p["launchers"]:
        return _classify_words(p, _unwrap_launcher(prog, p["launchers"][prog], args), depth + 1)
    if prog in p["shells"]:
        for i, a in enumerate(args):
            if a == "-c" and i + 1 < len(args):
                return _max(RISK_READ, _classify(p, args[i + 1], depth + 1))
        return RISK_WRITE
    line = " ".join([prog, *args]).lower()
    if any(line == tc or line.startswith(tc + " ") for tc in p["test_commands"]):
        return RISK_TEST
    if prog in p["interpreters"]:
        if len(args) == 1 and args[0] in p["interpreter_read_args"]:
            return RISK_READ
        return RISK_WRITE
    if prog in p["destructive_programs"] or any(prog.startswith(x) for x in p["destructive_program_prefixes"]):
        return RISK_DESTRUCTIVE
    if prog in p["http_clients"]:
        return _classify_http(p, args)
    if prog == "find":
        return _classify_find(p, args, depth)
    if prog == "sed":
        return RISK_WRITE if any(a in p["sed_inplace_flags"] or a.startswith("-i") for a in args) else RISK_READ
    if prog == "chmod":
        for a in args:
            if a in p["chmod_destructive_flags"] or (a.startswith("-") and not a.startswith("--") and "R" in a):
                return RISK_DESTRUCTIVE
        return RISK_WRITE
    if prog in p["subcommands"]:
        return _classify_subcommand(p["subcommands"][prog], args)
    if prog in p["test_programs"]:
        return RISK_TEST
    if prog in p["write_programs"]:
        return RISK_WRITE
    if prog in p["read_programs"]:
        return RISK_READ
    return RISK_WRITE


def _unwrap_launcher(prog: str, spec: dict, args: list[str]) -> list[str]:
    positional = spec.get("skip_positional", 0)
    skip_with_value = spec.get("skip_flags_with_value", [])
    i = 0
    while i < len(args):
        a = args[i]
        if a.startswith("-"):
            i += 2 if a in skip_with_value else 1
        elif prog == "env" and _ASSIGNMENT.match(a):
            i += 1
        elif positional > 0:
            positional -= 1
            i += 1
        else:
            return args[i:]
    return []


def _classify_subcommand(spec: dict, args: list[str]) -> str:
    i = 0
    while i < len(args) and args[i].startswith("-"):
        i += 2 if args[i] in spec.get("global_flags_with_value", []) else 1
    if any(a in spec.get("destructive_flags", []) for a in args):
        return RISK_DESTRUCTIVE
    if i >= len(args):
        return RISK_READ
    sub, rest = args[i].lower(), args[i + 1:]
    if sub in spec.get("nested", {}):
        return _classify_subcommand(spec["nested"][sub], rest)
    if sub in spec.get("destructive", []):
        return RISK_DESTRUCTIVE
    if sub in spec.get("test", []):
        return RISK_TEST
    if sub in spec.get("read", []):
        return RISK_READ
    return RISK_WRITE


def _classify_http(p: dict, args: list[str]) -> str:
    grade = RISK_READ
    for i, a in enumerate(args):
        method = ""
        for f in p["http_method_flags"]:
            if a == f and i + 1 < len(args):
                method = args[i + 1]
            elif f.startswith("--") and a.startswith(f + "="):
                method = a[len(f) + 1:]
            elif not f.startswith("--") and a.startswith(f) and len(a) > len(f):
                method = a[len(f):]
        if method.lower() in p["http_mutating_methods"]:
            return RISK_DESTRUCTIVE
        name = a.split("=", 1)[0] if a.startswith("--") and "=" in a[1:] else a
        if name in p["http_data_flags"]:
            return RISK_DESTRUCTIVE
        if name in p["http_output_flags"]:
            grade = RISK_WRITE
    return grade


def _classify_find(p: dict, args: list[str], depth: int) -> str:
    grade = RISK_READ
    i = 0
    while i < len(args):
        if args[i] in p["find_destructive_flags"]:
            return RISK_DESTRUCTIVE
        if args[i] in p["find_exec_flags"]:
            j = i + 1
            while j < len(args) and args[j] not in (";", "+"):
                j += 1
            grade = _max(grade, _classify_words(p, args[i + 1:j], depth + 1))
            i = j
        i += 1
    return grade


def _lex_shell(s: str) -> list[tuple[list[str], list[str]]]:
    """Split a command line into simple commands, as lexShell in Go does."""
    UNQUOTED, SINGLE, DOUBLE = 0, 1, 2
    segs: list[tuple[list[str], list[str]]] = []
    words: list[str] = []
    redirects: list[str] = []
    word: list[str] = []
    state = {"in_word": False, "out": False, "in": False}
    quote = UNQUOTED
    stack: list[tuple[bool, int, bool]] = []

    def end_word() -> None:
        if not state["in_word"]:
            return
        w = "".join(word)
        word.clear()
        state["in_word"] = False
        if state["out"]:
            redirects.append(w)
            state["out"] = False
        elif state["in"]:
            state["in"] = False
        else:
            words.append(w)

    def end_seg() -> None:
        nonlocal words, redirects
        end_word()
        state["out"] = state["in"] = False
        if words or redirects:
            segs.append((words, redirects))
        words, redirects = [], []

    def add(c: str) -> None:
        word.append(c)
        state["in_word"] = True

    def at_command_name() -> bool:
        # Nothing but assignments precedes this position in the command.
        return not state["in_word"] and all(_ASSIGNMENT.match(w) for w in words)

    def close_substitution(frame: tuple[bool, int, bool]) -> int:
        # A substitution that produced the command name leaves a placeholder
        # name, so the command is graded as one whose program is only known
        # at run time.
        if frame[2]:
            word.append(_DYNAMIC_COMMAND_NAME)
            state["in_word"] = True
        return frame[1]

    i, n = 0, len(s)
    while i < n:
        c = s[i]
        if quote == SINGLE:
            if c == "'":
                quote = UNQUOTED
            else:
                add(c)
            i += 1
            continue
        if c == "\\" and i + 1 < n:
            add(s[i + 1])
            i += 2
            continue
        if c == "$" and i + 1 < n and s[i + 1] == "(":
            head = at_command_name()
            end_seg()
            stack.append((False, quote, head))
            quote = UNQUOTED
            i += 2
            continue
        if c == "`":
            if stack and stack[-1][0]:
                end_seg()
                quote = close_substitution(stack.pop())
            else:
                head = at_command_name()
                end_seg()
                stack.append((True, quote, head))
                quote = UNQUOTED
            i += 1
            continue
        if quote == DOUBLE:
            if c == '"':
                quote = UNQUOTED
            else:
                add(c)
            i += 1
            continue
        if c == "'":
            quote = SINGLE
            state["in_word"] = True
        elif c == '"':
            quote = DOUBLE
            state["in_word"] = True
        elif c in " \t\r":
            end_word()
        elif c in ";\n|&(":
            end_seg()
        elif c == ")":
            end_seg()
            if stack and not stack[-1][0]:
                quote = close_substitution(stack.pop())
        elif c == ">":
            if state["in_word"] and "".join(word).isdigit():
                word.clear()
                state["in_word"] = False
            end_word()
            while i + 1 < n and s[i + 1] in ">|":
                i += 1
            if i + 1 < n and s[i + 1] == "&":
                i += 1
                while i + 1 < n and s[i + 1] not in " ;|\n":
                    i += 1
                i += 1
                continue
            state["out"] = True
        elif c == "<":
            end_word()
            while i + 1 < n and s[i + 1] == "<":
                i += 1
            state["in"] = True
        else:
            add(c)
        i += 1
    end_seg()
    return segs


class OutputReducer:
    def rewrite_command(self, command: str) -> CommandRewrite:
        """Block destructive commands; pass every other command through.

        Destructive commands must go through the execution-proxy service,
        which requires explicit confirmation and governance authorisation.
        """
        if not command.strip():
            raise ValueError("command is empty")
        if classify_risk(command) == RISK_DESTRUCTIVE:
            return CommandRewrite(
                original_command=command,
                rewritten_command="",
                is_rewritten=True,
                reason="destructive command blocked: route it through the execution proxy with explicit confirmation",
            )
        return CommandRewrite(original_command=command, rewritten_command=command, is_rewritten=False)

    def reduce_output(self, output: str, max_bytes: int = 2048) -> str:
        if max_bytes <= 0 or len(output) <= max_bytes:
            return output
        half = max_bytes // 2
        return output[:half] + "\n... [output reduced by execution-proxy] ...\n" + output[-half:]
