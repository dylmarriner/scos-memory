"""Python code language AST parser adapters (§19.5)."""

from __future__ import annotations

import ast
from dataclasses import dataclass


@dataclass
class SymbolDef:
    name: str
    kind: str
    start_line: int
    end_line: int


class PythonASTAdapter:
    def parse_symbols(self, source: str) -> list[SymbolDef]:
        symbols: list[SymbolDef] = []
        try:
            tree = ast.parse(source)
            for node in ast.walk(tree):
                if isinstance(node, ast.FunctionDef):
                    symbols.append(SymbolDef(node.name, "function", node.lineno, getattr(node, "end_lineno", node.lineno)))
                elif isinstance(node, ast.ClassDef):
                    symbols.append(SymbolDef(node.name, "class", node.lineno, getattr(node, "end_lineno", node.lineno)))
        except SyntaxError:
            pass
        return symbols
