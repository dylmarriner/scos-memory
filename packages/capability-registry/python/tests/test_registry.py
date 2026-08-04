import unittest
from scos_capability.registry import CapabilityRecord, CapabilityRegistry


class CapabilityRegistryTests(unittest.TestCase):
    def test_registry(self):
        reg = CapabilityRegistry()
        rec = CapabilityRecord("cap-1", "Memory Recall", "memory", "Recalls memory items")
        reg.register(rec)
        self.assertEqual(reg.get("cap-1").name, "Memory Recall")
        self.assertEqual(len(reg.list_healthy("memory")), 1)


if __name__ == "__main__":
    unittest.main()
