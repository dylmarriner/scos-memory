import unittest
from main import run_maintenance_cycle


class IndexMaintenanceTests(unittest.TestCase):
    def test_maintenance_cycle(self):
        res = run_maintenance_cycle()
        self.assertEqual(res["status"], "success")
        self.assertTrue(res["fts_vacuumed"])


if __name__ == "__main__":
    unittest.main()
