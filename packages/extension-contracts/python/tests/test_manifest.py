import base64
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from scos_extension.manifest import HookDef, Manifest, SkillDef

# Shared with packages/extension-contracts/extensions_test.go.
VECTOR_PAYLOAD = (
    '{"hooks":[{"hook_id":"lint","priority":10,"stage":"pre_turn","timeout_ms":500}],'
    '"name":"Linter <é>","package_id":"org.scos.linter","publisher":"scos-team",'
    '"required_permissions":["read:code","write:memory"],'
    '"skills":[{"description":"a\\nb","name":"Format & fix","parameters":[],"skill_id":"fmt"}],'
    '"type":"hook","version":"1.0.0"}'
)
VECTOR_PUBLIC_KEY = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
VECTOR_SIGNATURE = "90IZ72ZkcttOxHT/MpX2NhoUrwglRbEfLpp8ustbbvZnmNTsqOh4Hnzu4nWJ/HhObYcVYJsLSVIJNX6izjfTDw=="


def vector_manifest(signature=""):
    return Manifest(
        "org.scos.linter", "Linter <é>", "1.0.0", "hook", "scos-team", signature,
        ["read:code", "write:memory"],
        [HookDef("lint", "pre_turn", 10, 500)],
        [SkillDef("fmt", "Format & fix", "a\nb")],
    )


class ManifestTests(unittest.TestCase):
    def test_payload_matches_go(self):
        self.assertEqual(vector_manifest().signing_payload().decode("utf-8"), VECTOR_PAYLOAD)

    def test_verifies_go_signature(self):
        self.assertTrue(vector_manifest(VECTOR_SIGNATURE).verify_signature(VECTOR_PUBLIC_KEY))

    def test_sign_matches_go(self):
        m = vector_manifest()
        m.sign(Ed25519PrivateKey.from_private_bytes(bytes(range(32))))
        self.assertEqual(m.signature, VECTOR_SIGNATURE)

    def test_rejects_tampering_and_bad_keys(self):
        m = vector_manifest(VECTOR_SIGNATURE)
        m.required_permissions.insert(0, "admin:*")
        self.assertFalse(m.verify_signature(VECTOR_PUBLIC_KEY))
        good = vector_manifest(VECTOR_SIGNATURE)
        other = base64.b64encode(bytes(32)).decode()
        for key in ("", "not base64!", base64.b64encode(b"short").decode(), other):
            self.assertFalse(good.verify_signature(key), key)
        self.assertFalse(vector_manifest("").verify_signature(VECTOR_PUBLIC_KEY))


if __name__ == "__main__":
    unittest.main()
