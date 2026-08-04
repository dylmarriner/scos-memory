import unittest
from scos_execution.reducer import OutputReducer


class OutputReducerTests(unittest.TestCase):
    def test_rewrite(self):
        red = OutputReducer()
        rw = red.rewrite_command("rm -rf /")
        self.assertTrue(rw.is_rewritten)

        out = red.reduce_output("x" * 5000, 100)
        self.assertIn("reduced", out)


if __name__ == "__main__":
    unittest.main()
