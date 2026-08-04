import hashlib
import unittest
from scos_extension.manifest import Manifest


class ManifestTests(unittest.TestCase):
    def test_verify_signature(self):
        pub_key = "key123"
        raw = f"pkg.test:1.0:publisher:{pub_key}"
        sig = hashlib.sha256(raw.encode("utf-8")).hexdigest()

        m = Manifest("pkg.test", "Test Pkg", "1.0", "plugin", "publisher", sig)
        self.assertTrue(m.verify_signature(pub_key))


if __name__ == "__main__":
    unittest.main()
