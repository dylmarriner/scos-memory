import importlib.util
import os
import pathlib
import stat
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
SPEC = importlib.util.spec_from_file_location("cloud_provision", ROOT / "scripts/cloud_provision.py")
cloud_provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(cloud_provision)

CREDS = {"gateway_url": "https://gw", "bearer_token": "scos_live_secret", "tenant_id": "t", "principal_id": "p"}


class ScosEnvIsPrivate(unittest.TestCase):
    def test_env_file_backup_and_directory_are_owner_only(self):
        previous = os.umask(0o022)
        try:
            with tempfile.TemporaryDirectory() as directory:
                env = pathlib.Path(directory) / ".hermes" / "scos.env"
                cloud_provision.SCOS_ENV = env
                cloud_provision.write_scos_env(CREDS)
                cloud_provision.write_scos_env(dict(CREDS, bearer_token="scos_live_rotated"))
                files = list(env.parent.iterdir())
                self.assertEqual(len(files), 2, files)
                for path in files:
                    self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600, path)
                self.assertEqual(stat.S_IMODE(env.parent.stat().st_mode), 0o700)
                self.assertIn("SCOS_BEARER_TOKEN=scos_live_rotated\n", env.read_text())
        finally:
            os.umask(previous)


if __name__ == "__main__":
    unittest.main()
