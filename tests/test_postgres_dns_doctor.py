"""Read-only Docker DNS diagnosis must never expose credentials or mutate containers."""
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest

ROOT = Path(__file__).resolve().parents[1]
MODULE = ROOT / "scripts" / "postgres_dns_doctor.py"
spec = importlib.util.spec_from_file_location("postgres_dns_doctor", MODULE)
doctor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(doctor)


def container(service, networks, *, running=True, health="healthy", project="compose"):
    return {
        "Id": "a" * 64 if service == "postgres" else "b" * 64,
        "Config": {
            "Labels": {"com.docker.compose.project": project, "com.docker.compose.service": service},
            "Env": ["POSTGRES_PASSWORD=must-never-leak"],
        },
        "State": {"Running": running, "Health": {"Status": health}},
        "NetworkSettings": {"Networks": networks},
    }


class FakeDocker:
    def __init__(self, containers, *, dns_rc=0, inspect_rc=0):
        self.containers = containers
        self.dns_rc = dns_rc
        self.inspect_rc = inspect_rc
        self.commands = []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        if command[:2] == ["docker", "ps"]:
            return SimpleNamespace(returncode=0, stdout="\n".join(c["Id"] for c in self.containers)+"\n", stderr="")
        if command[:2] == ["docker", "inspect"]:
            self_formatted = []
            for c in self.containers:
                self_formatted.append(json.dumps({
                    "Id": c["Id"],
                    "Project": c["Config"]["Labels"]["com.docker.compose.project"],
                    "Service": c["Config"]["Labels"]["com.docker.compose.service"],
                    "Running": c["State"]["Running"],
                    "Health": c["State"]["Health"]["Status"],
                    "Networks": c["NetworkSettings"]["Networks"],
                }))
            return SimpleNamespace(returncode=self.inspect_rc, stdout="\n".join(self_formatted), stderr="SECRET=should-not-leak")
        if command[:2] == ["docker", "exec"]:
            return SimpleNamespace(returncode=self.dns_rc, stdout="172.18.0.2 postgres\n" if self.dns_rc == 0 else "", stderr="SECRET=should-not-leak")
        raise AssertionError(f"unexpected command: {command}")


class PostgresDNSDoctorTests(unittest.TestCase):
    def setUp(self):
        self.good = [
            container("postgres", {"compose_default": {"Aliases": ["postgres", "db"]}}),
            container("hermes-provider", {"compose_default": {"Aliases": ["hermes-provider"]}}),
        ]

    def test_shared_network_with_verified_dns_is_positive_only_for_network(self):
        docker = FakeDocker(self.good)
        result = doctor.inspect_compose("compose", dns_probe=True, runner=docker)
        self.assertEqual(result["network_result"], "pass")
        self.assertEqual(result["dns_probe"], "resolved")
        self.assertNotIn("database_ready", result)
        self.assertEqual(len(docker.commands), 3)

    def test_inspect_does_not_request_environment_or_full_container_json(self):
        docker = FakeDocker(self.good)
        doctor.inspect_compose("compose", runner=docker)
        inspect = docker.commands[1]
        self.assertEqual(inspect[:2], ["docker", "inspect"])
        self.assertIn("--format", inspect)
        format_string = inspect[inspect.index("--format") + 1]
        self.assertNotIn(".Config.Env", format_string)
        self.assertNotIn("{{json .}}", format_string)

    def test_disjoint_networks_fail_without_attempting_dns(self):
        bad = [self.good[0], container("hermes-provider", {"other": {"Aliases": ["hermes-provider"]}})]
        docker = FakeDocker(bad)
        result = doctor.inspect_compose("compose", dns_probe=True, runner=docker)
        self.assertEqual(result["network_result"], "fail")
        self.assertIn("no_shared_network", result["issues"])
        self.assertEqual(len(docker.commands), 2)

    def test_missing_postgres_alias_on_shared_network_fails(self):
        bad = [container("postgres", {"compose_default": {"Aliases": ["db"]}}), self.good[1]]
        result = doctor.inspect_compose("compose", runner=FakeDocker(bad))
        self.assertIn("postgres_alias_missing", result["issues"])
        self.assertEqual(result["network_result"], "fail")

    def test_dns_failure_fails_and_does_not_leak_docker_errors(self):
        docker = FakeDocker(self.good, dns_rc=2)
        result = doctor.inspect_compose("compose", dns_probe=True, runner=docker)
        self.assertEqual(result["network_result"], "fail")
        self.assertEqual(result["dns_probe"], "failed")
        self.assertNotIn("must-never-leak", json.dumps(result))
        self.assertNotIn("should-not-leak", json.dumps(result))

    def test_inspect_failure_fails_closed_without_raw_stderr(self):
        result = doctor.inspect_compose("compose", runner=FakeDocker(self.good, inspect_rc=1))
        self.assertEqual(result["network_result"], "indeterminate")
        self.assertIn("docker_inspect_failed", result["issues"])
        self.assertNotIn("SECRET=", json.dumps(result))

    def test_no_probe_does_not_claim_dns_success(self):
        result = doctor.inspect_compose("compose", runner=FakeDocker(self.good))
        self.assertEqual(result["network_result"], "indeterminate")
        self.assertEqual(result["dns_probe"], "not_run")

    def test_rejects_invalid_project_before_invoking_docker(self):
        docker = FakeDocker(self.good)
        with self.assertRaises(ValueError):
            doctor.inspect_compose("--bad/project", runner=docker)
        self.assertEqual(docker.commands, [])

    def test_absent_service_fails_without_destructive_actions(self):
        docker = FakeDocker([self.good[0]])
        result = doctor.inspect_compose("compose", dns_probe=True, runner=docker)
        self.assertIn("hermes_provider_missing", result["issues"])
        self.assertTrue(all(cmd[1] in ("ps", "inspect") for cmd in docker.commands))


if __name__ == "__main__":
    unittest.main()
