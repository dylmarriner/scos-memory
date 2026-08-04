import unittest
from scos_hermes.provider import HermesMemoryProvider


class HermesProviderTests(unittest.TestCase):
    def test_provider(self):
        p = HermesMemoryProvider("prof1", "agent1")
        entry = p.write_memory("theme", "dark")
        self.assertEqual(entry.value, "dark")
        got = p.read_memory("theme")
        self.assertEqual(got.value, "dark")


if __name__ == "__main__":
    unittest.main()
