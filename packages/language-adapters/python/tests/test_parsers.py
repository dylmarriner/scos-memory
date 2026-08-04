import unittest
from scos_lang.parsers import PythonASTAdapter


class ASTAdapterTests(unittest.TestCase):
    def test_parse(self):
        src = "class User:\n    def process(self):\n        pass\n"
        adapter = PythonASTAdapter()
        symbols = adapter.parse_symbols(src)
        self.assertEqual(len(symbols), 2)
        names = [s.name for s in symbols]
        self.assertIn("User", names)
        self.assertIn("process", names)


if __name__ == "__main__":
    unittest.main()
