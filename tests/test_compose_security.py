import pathlib
import re
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
COMPOSE = (ROOT / "deploy/compose/docker-compose.yml").read_text()
ENV_EXAMPLE = (ROOT / ".env.example").read_text()


class ComposeSecurityTests(unittest.TestCase):
    def test_no_plaintext_host_ports_or_password_fallbacks(self):
        # Host publication is intentional for the Tailscale-facing services
        # and the loopback-bound creator console.  Reject only unbound or
        # plaintext-style mappings that would expose services on all hosts.
        for line in COMPOSE.splitlines():
            # Check published host:container ports only.  `extra_hosts` also
            # uses a colon, but it is an internal DNS mapping rather than a
            # host port publication and has a different shape.
            if line.strip().startswith('- "') and re.search(r":\d+:\d+", line):
                self.assertRegex(
                    line,
                    r"\$\{SCOS_(?:TAILSCALE_IP|CLOUD_BIND_ADDRESS)",
                    line,
                )
        self.assertNotIn("0.0.0.0:", COMPOSE)
        self.assertNotIn("POSTGRES_PORT", COMPOSE)
        self.assertNotIn("change-me", COMPOSE)
        self.assertNotIn("sslmode=disable", COMPOSE)

    def test_database_and_backup_security_are_explicit(self):
        self.assertIn("POSTGRES_PASSWORD:?", COMPOSE)
        self.assertIn("SCOS_DATABASE_URL:?", COMPOSE)
        self.assertIn("wal_level=replica", COMPOSE)
        self.assertIn("archive_mode=on", COMPOSE)
        self.assertIn("SCOS_BACKUP_PROVIDER_VERIFIED", COMPOSE)
        self.assertIn("SCOS_BACKUP_OFF_HOST_VERIFIED", COMPOSE)
        self.assertIn("SCOS_SECRET_MANAGER", COMPOSE)
        self.assertIn("SCOS_BACKUP_ENCRYPTION_KEY_REF", COMPOSE)
        self.assertNotIn("SCOS_BACKUP_ENCRYPTION_KEY:", COMPOSE)

    def test_generated_tls_private_key_is_not_world_readable(self):
        self.assertIn("chmod 600 /etc/scos/tls/tls.key", COMPOSE)
        self.assertNotIn("chmod 644 /etc/scos/tls/tls.key", COMPOSE)

    def test_compose_requires_all_service_credentials(self):
        for variable in (
            "SCOS_INTERNAL_SERVICE_TOKEN",
            "SCOS_CONTROL_PLANE_TOKEN",
            "SCOS_SECURITY_ISSUER_SECRET",
        ):
            self.assertIn(f"{variable}:?", COMPOSE)
            self.assertIn(f"{variable}=", ENV_EXAMPLE)

    def test_example_declares_required_variables_once(self):
        declarations = re.findall(r"^([A-Z][A-Z0-9_]*)=", ENV_EXAMPLE, re.MULTILINE)
        for variable in (
            "SCOS_INTERNAL_SERVICE_TOKEN",
            "SCOS_CONTROL_PLANE_TOKEN",
            "SCOS_SECURITY_ISSUER_SECRET",
            "POSTGRES_PASSWORD",
            "SCOS_DATABASE_URL",
            "SCOS_AGENT_ENDPOINT",
        ):
            self.assertEqual(declarations.count(variable), 1, variable)
        self.assertNotRegex(ENV_EXAMPLE, r"^DATABASE_URL=", re.MULTILINE)

    def test_example_does_not_ship_a_password_or_disable_tls(self):
        self.assertNotIn("change-me", ENV_EXAMPLE)
        self.assertNotIn("sslmode=disable", ENV_EXAMPLE)
        self.assertIn("POSTGRES_PASSWORD=", ENV_EXAMPLE)
        self.assertIn("SCOS_BACKUP_PROVIDER=disabled", ENV_EXAMPLE)


if __name__ == "__main__":
    unittest.main()
