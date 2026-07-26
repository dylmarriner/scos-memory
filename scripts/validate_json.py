from __future__ import annotations

import json
from pathlib import Path


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    paths = sorted((root / "schemas").rglob("*.json"))
    if not paths:
        raise SystemExit("no JSON schemas found")

    for path in paths:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
        if not isinstance(value, dict) or "$schema" not in value:
            raise SystemExit(f"invalid schema document: {path}")
        print(f"validated {path.relative_to(root)}")


if __name__ == "__main__":
    main()
