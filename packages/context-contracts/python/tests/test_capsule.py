import unittest
from scos_context.capsule import ContextSegment, SegmentClass, generate_restoration_key


class ContextCapsuleTests(unittest.TestCase):
    def test_key_generation(self):
        seg = ContextSegment("s1", SegmentClass.SYSTEM_MANDATORY, "Rules", 10)
        key = generate_restoration_key("sess-1", [seg])
        self.assertTrue(len(key) == 32)


if __name__ == "__main__":
    unittest.main()
