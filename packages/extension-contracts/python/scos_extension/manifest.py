"""Python extension contracts package (§22)."""

from __future__ import annotations

import base64
import binascii
import json
from dataclasses import dataclass, field

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey


@dataclass
class HookDef:
    hook_id: str
    stage: str
    priority: int = 0
    timeout_ms: int = 0


@dataclass
class SkillDef:
    skill_id: str
    name: str
    description: str = ""
    parameters: list[str] = field(default_factory=list)


@dataclass
class Manifest:
    package_id: str
    name: str
    version: str
    extension_type: str
    publisher: str
    signature: str = ""
    required_permissions: list[str] = field(default_factory=list)
    hooks: list[HookDef] = field(default_factory=list)
    skills: list[SkillDef] = field(default_factory=list)

    def signing_payload(self) -> bytes:
        """Bytes a publisher signs; byte-identical to the Go package's
        Manifest.SigningPayload (sorted keys, compact, no HTML escaping)."""
        doc = {
            "package_id": self.package_id,
            "name": self.name,
            "version": self.version,
            "type": self.extension_type,
            "publisher": self.publisher,
            "required_permissions": list(self.required_permissions),
            "hooks": [
                {"hook_id": h.hook_id, "stage": h.stage, "priority": h.priority, "timeout_ms": h.timeout_ms}
                for h in self.hooks
            ],
            "skills": [
                {"skill_id": s.skill_id, "name": s.name, "description": s.description, "parameters": list(s.parameters)}
                for s in self.skills
            ],
        }
        text = json.dumps(doc, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
        # Go's encoder escapes these two line terminators even with HTML
        # escaping off; match it so signatures interoperate.
        return text.replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode("utf-8")

    def sign(self, private_key: Ed25519PrivateKey) -> None:
        self.signature = base64.b64encode(private_key.sign(self.signing_payload())).decode("ascii")

    def verify_signature(self, public_key: str) -> bool:
        """True when signature is a valid Ed25519 signature of
        signing_payload() under the base64-encoded public key."""
        try:
            key = Ed25519PublicKey.from_public_bytes(base64.b64decode(public_key.strip(), validate=True))
            sig = base64.b64decode(self.signature, validate=True)
        except (ValueError, binascii.Error):
            return False
        try:
            key.verify(sig, self.signing_payload())
        except InvalidSignature:
            return False
        return True
