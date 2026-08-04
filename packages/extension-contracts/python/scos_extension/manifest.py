"""Python extension contracts package (§22)."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field


@dataclass
class Manifest:
    package_id: str
    name: str
    version: str
    extension_type: str
    publisher: str
    signature: str = ""
    required_permissions: list[str] = field(default_factory=list)

    def verify_signature(self, public_key: str) -> bool:
        if not self.signature:
            return False
        raw = f"{self.package_id}:{self.version}:{self.publisher}:{public_key}"
        expected = hashlib.sha256(raw.encode("utf-8")).hexdigest()
        return self.signature == expected
