#!/usr/bin/env python3
"""Import the memories other AI agents keep on a machine into SCOS.

Two steps, so a machine without SCOS access can still contribute:

  collect  reads the known agent memory stores under a home directory and
           writes one JSON object per memory to stdout (nothing leaves the
           machine). Each carries its origin (agent@host:path), the time the
           source recorded it, the memory network it belongs to and its scope.
  import   sends collected memories to SCOS through the MCP stdio bridge with
           scos_memory_import, at most 200 per call, and prints a summary.

  import_agent_memories.py collect [--home DIR] [--host NAME] > memories.jsonl
  import_agent_memories.py import memories.jsonl [--bridge PATH] [--dry-run]

What is read (only curated memory, never chat transcripts, caches, skills,
plugins or an agent's persona prompt):
  Claude Code  ~/.claude/CLAUDE.md, ~/.claude/projects/*/memory/*.md
  Codex        ~/.codex/AGENTS.md, ~/.codex/memories/{MEMORY,memory_summary}.md
  Hermes       ~/.hermes/{MEMORY,USER}.md, ~/.hermes/memories/{MEMORY,USER}.md
  OpenClaw     ~/.openclaw/workspace/{MEMORY,USER}.md, workspace/memory/*.md,
               ~/.openclaw/self-improve/memory.md
  Gemini, Qwen, OpenCode   their global instruction files
  Windsurf     ~/.codeium/windsurf/memories/*.md
  others       ~/.antigravity_memory/MEMORY.md, ~/.od/memory/MEMORY.md,
               ~/coding/memory.md, ~/self-improving/memory.md,
               ~/self-improvement/memory.md
"""
import argparse
import datetime as dt
import hashlib
import json
import os
import re
import socket
import subprocess
import sys
from pathlib import Path

BATCH = 200
MAX_UNIT = 900
MIN_UNIT = 12

CLAUDE_TYPES = {"user": "entity", "feedback": "procedure", "project": "world", "reference": "world"}
DAILY = re.compile(r"^(\d{4}-\d{2}-\d{2})")
BULLET = re.compile(r"^\s*(?:[-*+]|\d+[.)])\s+(.*)$")
HEADING = re.compile(r"^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$")
FRONTMATTER = re.compile(r"\A---\n(.*?)\n---\n?", re.S)
COMMENT = re.compile(r"<!--.*?-->", re.S)
INCLUDE = re.compile(r"^@\S+$")
INDEX_LINK = re.compile(r"^\[[^\]]+\]\([^)\s]+\.md\)")
EMPHASIS = re.compile(r"(\*\*|__)(.+?)\1")


def iso(ts: float) -> str:
    return dt.datetime.fromtimestamp(ts, dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def project_scope(encoded: str) -> str:
    """The project a Claude Code memory directory belongs to.

    ~/.claude/projects/<cwd with / replaced by -> loses where a directory
    name with dashes starts, so the project is everything after the last
    known parent of project checkouts (DEVELOPMENT, DEVELOPMENT1, a drive
    ending in ext4); otherwise the last segment.
    """
    parts = [p for p in encoded.split("-") if p]
    for i in range(len(parts) - 1, -1, -1):
        if parts[i] in ("DEVELOPMENT", "DEVELOPMENT1") or parts[i].endswith("ext4"):
            tail = parts[i + 1:]
            return "-".join(tail) if tail else "global"
    return parts[-1] if parts else "global"


def split_sentences(text: str):
    """Pieces of at most MAX_UNIT characters, broken at sentence ends."""
    if len(text) <= MAX_UNIT:
        return [text]
    out, cur = [], ""
    for sentence in re.split(r"(?<=[.!?])\s+", text):
        while len(sentence) > MAX_UNIT:
            if cur:
                out.append(cur)
                cur = ""
            out.append(sentence[:MAX_UNIT])
            sentence = sentence[MAX_UNIT:]
        if cur and len(cur) + 1 + len(sentence) > MAX_UNIT:
            out.append(cur)
            cur = sentence
        else:
            cur = f"{cur} {sentence}".strip()
    if cur:
        out.append(cur)
    return out


def units(markdown: str):
    """Standalone memories in a markdown memory file.

    Each bullet is one memory; so is each paragraph. A bullet's nearest
    heading is kept as context when the bullet does not already say it.
    Fenced code, tables, HTML comments, @file include lines, index lines
    that point at another memory file and the Hermes entry separator (a
    line holding only §) are not memories.
    """
    text = COMMENT.sub("", FRONTMATTER.sub("", markdown))
    heading, para, fenced, found = "", [], False, []

    def flush():
        if para:
            joined = " ".join(s.strip() for s in para).strip()
            if joined:
                found.append(joined)
            para.clear()

    for line in text.splitlines():
        if line.strip().startswith("```"):
            flush()
            fenced = not fenced
            continue
        if fenced:
            continue
        h = HEADING.match(line)
        if h:
            flush()
            heading = h.group(2).strip()
            continue
        stripped = line.strip()
        if not stripped or stripped == "§" or stripped.startswith("|") or set(stripped) <= set("-=*_ ") or INCLUDE.match(stripped):
            flush()
            continue
        b = BULLET.match(line)
        if b:
            flush()
            item = b.group(1).strip()
            if INDEX_LINK.match(item):
                continue  # points at another memory file, imported on its own
            if heading and len(item) < 160 and heading.lower() not in item.lower():
                item = f"{heading}: {item}"
            found.append(item)
            continue
        if INDEX_LINK.match(stripped):
            flush()
            continue
        para.append(stripped)
    flush()
    result = []
    for u in found:
        u = re.sub(r"\s+", " ", EMPHASIS.sub(r"\2", u)).strip()
        if len(u) >= MIN_UNIT:
            result.extend(split_sentences(u))
    return result


def claude_memory(path: Path):
    """A Claude Code memory file: frontmatter name/description/type, then the body."""
    raw = path.read_text(errors="replace")
    meta = {}
    m = FRONTMATTER.match(raw)
    if m:
        for line in m.group(1).splitlines():
            if ":" in line:
                k, v = line.split(":", 1)
                meta[k.strip()] = v.strip().strip('"')
    network = CLAUDE_TYPES.get(meta.get("type", ""), "world")
    pieces = units(raw)
    if meta.get("description") and not any(meta["description"] in p for p in pieces):
        pieces.insert(0, meta["description"])
    return network, pieces


def sources(home: Path):
    """(agent, path, network, scope, kind) for every memory store present."""
    h = home
    fixed = [
        ("claude-code", h / ".claude/CLAUDE.md", "procedure", "global", "md"),
        ("codex", h / ".codex/AGENTS.md", "procedure", "global", "md"),
        ("codex", h / ".codex/memories/MEMORY.md", "world", "global", "md"),
        ("codex", h / ".codex/memories/memory_summary.md", "world", "global", "md"),
        ("hermes", h / ".hermes/MEMORY.md", "world", "global", "md"),
        ("hermes", h / ".hermes/USER.md", "entity", "global", "md"),
        ("hermes", h / ".hermes/memories/MEMORY.md", "world", "global", "md"),
        ("hermes", h / ".hermes/memories/USER.md", "entity", "global", "md"),
        ("openclaw", h / ".openclaw/workspace/MEMORY.md", "world", "global", "md"),
        ("openclaw", h / ".openclaw/workspace/USER.md", "entity", "global", "md"),
        ("openclaw", h / ".openclaw/self-improve/memory.md", "procedure", "global", "md"),
        ("gemini", h / ".gemini/GEMINI.md", "procedure", "global", "md"),
        ("qwen", h / ".qwen/QWEN.md", "procedure", "global", "md"),
        ("opencode", h / ".config/opencode/AGENTS.md", "procedure", "global", "md"),
        ("antigravity", h / ".antigravity_memory/MEMORY.md", "world", "global", "md"),
        ("od", h / ".od/memory/MEMORY.md", "world", "global", "md"),
        ("agent-notes", h / "coding/memory.md", "procedure", "global", "md"),
        ("agent-notes", h / "self-improving/memory.md", "procedure", "global", "md"),
        ("agent-notes", h / "self-improvement/memory.md", "procedure", "global", "md"),
    ]
    for entry in fixed:
        if entry[1].is_file():
            yield entry
    for p in sorted((h / ".claude/projects").glob("*/memory/*.md")):
        if p.name != "MEMORY.md":  # the index of the other files
            yield ("claude-code", p, None, project_scope(p.parent.parent.name), "claude")
    for p in sorted((h / ".openclaw/workspace/memory").glob("*.md")):
        yield ("openclaw", p, "experience", "global", "md")
    for p in sorted((h / ".codeium/windsurf/memories").glob("*.md")):
        yield ("windsurf", p, "procedure", "global", "md")


def collect(home: Path, host: str):
    seen = set()
    for agent, path, network, scope, kind in sources(home):
        try:
            if kind == "claude":
                network, pieces = claude_memory(path)
            else:
                pieces = units(path.read_text(errors="replace"))
        except OSError as e:
            print(f"skip {path}: {e}", file=sys.stderr)
            continue
        day = DAILY.match(path.name)
        observed = f"{day.group(1)}T00:00:00Z" if day else iso(path.stat().st_mtime)
        rel = "~/" + str(path.relative_to(home)) if path.is_relative_to(home) else str(path)
        for piece in pieces:
            key = hashlib.sha256(re.sub(r"\W+", " ", piece.lower()).strip().encode()).hexdigest()
            if key in seen:
                continue
            seen.add(key)
            yield {"content": piece, "origin": f"{agent}@{host}:{rel}", "network": network,
                   "scope": scope, "observed_at": observed}


def bridge_call(bridge: str, batches):
    init = {"jsonrpc": "2.0", "id": 0, "method": "initialize",
            "params": {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "memory-import", "version": "1"}}}
    msgs = [init] + [{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call",
                      "params": {"name": "scos_memory_import", "arguments": {"items": b}}} for i, b in enumerate(batches)]
    proc = subprocess.run([sys.executable, bridge], input="\n".join(json.dumps(m) for m in msgs) + "\n",
                          capture_output=True, text=True, timeout=3600,
                          env={**os.environ, "SCOS_AGENT_NAME": os.environ.get("SCOS_AGENT_NAME", "memory-import")})
    replies = {}
    for line in proc.stdout.splitlines():
        if line.strip().startswith("{"):
            r = json.loads(line)
            replies[r.get("id")] = r
    return replies


def run_import(path: str, bridge: str, dry_run: bool):
    items = [json.loads(l) for l in open(path) if l.strip()]
    batches = [items[i:i + BATCH] for i in range(0, len(items), BATCH)]
    if dry_run:
        print(json.dumps({"memories": len(items), "calls": len(batches)}))
        return 0
    replies = bridge_call(bridge, batches)
    totals = {"stored": 0, "merged_into_existing": 0, "failed": 0, "contradictions": 0}
    errors = []
    for i in range(len(batches)):
        r = replies.get(i + 1)
        if r is None or "error" in r:
            totals["failed"] += len(batches[i])
            errors.append(f"batch {i}: {(r or {}).get('error', 'no reply')}")
            continue
        content = r["result"]["content"][0]["text"]
        if r["result"].get("isError"):
            totals["failed"] += len(batches[i])
            errors.append(f"batch {i}: {content[:300]}")
            continue
        body = json.loads(content)
        for k in ("stored", "merged_into_existing", "failed"):
            totals[k] += body.get(k, 0)
        for res in body.get("results", []):
            totals["contradictions"] += len(res.get("contradictions") or [])
            if res.get("error"):
                errors.append(f"{res['origin']}: {res['error'][:200]}")
    print(json.dumps({"memories": len(items), **totals}))
    for e in errors[:50]:
        print("ERROR", e, file=sys.stderr)
    return 1 if totals["failed"] else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("collect")
    c.add_argument("--home", default=str(Path.home()))
    c.add_argument("--host", default=socket.gethostname().split(".")[0])
    i = sub.add_parser("import")
    i.add_argument("file")
    i.add_argument("--bridge", default=str(Path.home() / ".hermes/scripts/scos-mcp-bridge.py"))
    i.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    if a.cmd == "collect":
        n = 0
        for item in collect(Path(a.home), a.host):
            print(json.dumps(item, ensure_ascii=False))
            n += 1
        print(f"collected {n} memories from {a.home} on {a.host}", file=sys.stderr)
        return 0
    return run_import(a.file, a.bridge, a.dry_run)


if __name__ == "__main__":
    sys.exit(main())
