#!/usr/bin/env python3
"""Generate indexes/CODE_INDEX.md: a structural index of the repository.

Walks services, packages, workers, adapters, integrations, api, server,
plugins, hooks and benchmarks and records what exists -- routes, exported
functions, classes/types, line counts and test counts -- so the tree can be
cross-checked against GOVERNANCE.md without re-reading it.

Symbol extraction is intentionally shallow: it is a structural inventory
(function/class names per component, capped), not a symbol-level code index.
The symbol-aware layer lives in services/code-intelligence (ADR-0007).
"""
import re
import subprocess
from pathlib import Path
from typing import TypedDict

ROOT = Path(__file__).resolve().parent.parent
ROUTE = re.compile(r'HandleFunc\("([^"]+)"')
GOFUNC = re.compile(r'^func (?:\([^)]*\) )?([A-Z]\w+)\(', re.MULTILINE)
GOTYPE = re.compile(r'^type ([A-Z]\w+) ', re.MULTILINE)
TESTFN = re.compile(r'^func (Test\w+)\(', re.MULTILINE)
PYFUNC = re.compile(r'^(?:async )?def ([a-zA-Z_]\w*)\(', re.MULTILINE)
PYCLASS = re.compile(r'^class ([A-Za-z_]\w*)\(?:', re.MULTILINE)
PYTEST = re.compile(r'^def (test_\w*)\(', re.MULTILINE)
TSFUNC = re.compile(r'^(?:export (?:default )?)?(?:async )?function ([A-Za-z_$][\w$]*)\(', re.MULTILINE)
TSARROW = re.compile(r'^export (?:default )?const ([A-Za-z_$][\w$]*) = (?:async )?\(', re.MULTILINE)
TSCLASS = re.compile(r'^(?:export (?:default )?)?(?:abstract )?(?:class|interface|enum) ([A-Za-z_$][\w$]*)', re.MULTILINE)
TSTEST = re.compile(r'^(?:export )?(?:async )?function (test|it)\(', re.MULTILINE)

# Directories that are build output, vendored deps, or VCS internals -- never
# part of the structural inventory.
EXCLUDED_DIRS = {
    "node_modules", ".git", ".next", "dist", "build", "vendor",
    "__pycache__", ".venv", "venv", ".tox", ".mypy_cache", ".pytest_cache",
    ".ruff_cache", "coverage", "htmlcov", "target", "out",
}

CODE_SUFFIXES = (".go", ".py", ".js", ".ts", ".mjs", ".cjs", ".jsx", ".tsx")
TEST_SUFFIXES = ("_test.go", "test_.py", ".test.js", ".test.ts", ".spec.js", ".spec.ts")


def read(p):
    try:
        return p.read_text(errors="replace")
    except OSError:
        return ""


def cap(items, limit=14):
    items = sorted(set(items))
    if len(items) <= limit:
        return items, ""
    return items[:limit], f" (+{len(items) - limit} more)"


class ScanResult(TypedDict):
    files: int
    loc: int
    tests: int
    routes: list[str]
    funcs: list[str]
    more_funcs: str
    classes: list[str]
    more_classes: str


def scan(d) -> ScanResult:
    """Collect routes, functions, classes/types, LOC and test counts for one
    component, walking every code file while skipping excluded dirs."""
    src = loc = tests = 0
    routes, funcs, classes = [], [], []
    for f in sorted(d.rglob("*")):
        if f.is_dir():
            continue
        if any(part in EXCLUDED_DIRS for part in f.parts):
            continue
        if f.suffix not in CODE_SUFFIXES:
            continue
        text = read(f)
        n = text.count("\n")
        if f.name.endswith(TEST_SUFFIXES) or f.name.startswith("test_"):
            tests += len(TESTFN.findall(text)) + len(PYTEST.findall(text)) + len(TSTEST.findall(text))
            continue
        loc += n
        src += 1
        routes += ROUTE.findall(text)
        funcs += GOFUNC.findall(text) + PYFUNC.findall(text) + TSFUNC.findall(text) + TSARROW.findall(text)
        classes += GOTYPE.findall(text) + PYCLASS.findall(text) + TSCLASS.findall(text)
    routes, _ = cap(routes)
    funcs, more_funcs = cap(funcs, 18)
    classes, more_classes = cap(classes, 14)
    result: ScanResult = {
        "files": src,
        "loc": loc,
        "tests": tests,
        "routes": routes,
        "funcs": funcs,
        "more_funcs": more_funcs,
        "classes": classes,
        "more_classes": more_classes,
    }
    return result


def section(title, base):
    out = [f"\n## {title}\n"]
    if not base.is_dir():
        return out
    for d in sorted(p for p in base.iterdir() if p.is_dir()):
        s = scan(d)
        if not s["files"]:
            continue
        out.append(f"### `{base.name}/{d.name}`\n")
        out.append(f"- {s['files']} source files, {s['loc']} LOC, {s['tests']} tests")
        if s["routes"]:
            out.append("- Routes:")
            out += [f"  - `{r}`" for r in s["routes"]]
        if s["funcs"]:
            shown = ", ".join(f"`{f}`" for f in s["funcs"])
            out.append(f"- Functions: {shown}{s['more_funcs']}")
        if s["classes"]:
            shown = ", ".join(f"`{t}`" for t in s["classes"])
            out.append(f"- Classes/types: {shown}{s['more_classes']}")
        out.append("")
    return out


def main():
    try:
        rev = subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=ROOT,
                             capture_output=True, text=True, check=True).stdout.strip()
    except (subprocess.CalledProcessError, OSError):
        rev = "unknown"

    lines = [
        "# Code Index",
        "",
        f"Generated by `scripts/generate_code_index.py` at commit `{rev}`.",
        "Regenerate with `make code-index`. Do not edit by hand.",
        "",
        "Structural inventory only: it records what exists, not whether it",
        "satisfies `GOVERNANCE.md`. Function/class capture is per-component",
        "and capped; the symbol-level index lives in services/code-intelligence.",
    ]
    for title, base in (("Services", ROOT / "services"),
                        ("Packages", ROOT / "packages"),
                        ("Workers", ROOT / "workers"),
                        ("Adapters", ROOT / "adapters"),
                        ("Integrations", ROOT / "integrations"),
                        ("API", ROOT / "api"),
                        ("Server", ROOT / "server"),
                        ("Plugins", ROOT / "plugins"),
                        ("Hooks", ROOT / "hooks"),
                        ("Benchmarks", ROOT / "benchmarks")):
        lines += section(title, base)

    lines.append("\n## Data layer\n")
    for name, pat in (("Migrations", "database/migrations/*.sql"),
                      ("RLS policies", "database/policies/*.sql"),
                      ("Schemas", "schemas/**/*.json"),
                      ("ADRs", "docs/adr/*.md")):
        found = sorted(ROOT.glob(pat))
        lines.append(f"### {name} ({len(found)})\n")
        lines += [f"- `{p.relative_to(ROOT)}`" for p in found]
        lines.append("")

    out = ROOT / "indexes" / "CODE_INDEX.md"
    out.parent.mkdir(exist_ok=True)
    out.write_text("\n".join(lines) + "\n")
    print(f"wrote {out.relative_to(ROOT)} ({len(lines)} lines)")


if __name__ == "__main__":
    main()
