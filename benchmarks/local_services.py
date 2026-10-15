"""Run real SCOS services from this checkout for benchmark measurement.

The acceptance programme measures the services themselves, not Python
re-implementations of them. ``LocalServiceStack`` builds the named service
binaries from the working tree with ``go build``, starts a disposable
PostgreSQL (pgvector image, TLS on, every migration and policy applied the
way ``scripts/scos-migrate`` applies them) when a service needs one, launches
each binary on a loopback port with freshly generated credentials, and waits
for ``/health/ready``. Everything is torn down on ``close()``.

When a prerequisite is missing (no Go toolchain, no Docker, an image that
cannot start) the stack raises ``StackUnavailable`` carrying the reason, so a
benchmark can record the domain as not run instead of reporting a result it
never measured.
"""
from __future__ import annotations

import json
import os
import pathlib
import secrets
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[1]
POSTGRES_IMAGE = "pgvector/pgvector:pg17"
DATABASE_SERVICES = {"code-intelligence", "tool-governance", "execution-proxy"}
# execution-proxy authorises every execution with tool-governance first.
DEPENDENCIES = {"execution-proxy": ["tool-governance"]}
ADDR_ENV = {
    "context-optimisation": "SCOS_CONTEXT_HTTP_ADDR",
    "code-intelligence": "SCOS_CODE_INTELLIGENCE_HTTP_ADDR",
    "tool-governance": "SCOS_TOOL_GOVERNANCE_HTTP_ADDR",
    "execution-proxy": "SCOS_EXECUTION_PROXY_HTTP_ADDR",
}


class StackUnavailable(RuntimeError):
    """A prerequisite for running the real service is missing."""


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def _run(args: list[str], *, timeout: float, cwd: pathlib.Path | None = None,
         input_text: str | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(args, cwd=cwd, input=input_text, capture_output=True, text=True,
                          timeout=timeout, check=False)


def source_sha() -> str:
    try:
        sha = _run(["git", "rev-parse", "HEAD"], cwd=ROOT, timeout=10).stdout.strip()
        dirty = _run(["git", "status", "--porcelain"], cwd=ROOT, timeout=10).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return "unknown"
    if not sha:
        return "unknown"
    return sha + ("+dirty" if dirty else "")


class DisposablePostgres:
    """A throwaway pgvector container with the full schema applied."""

    def __init__(self, workdir: pathlib.Path):
        self.workdir = workdir
        self.name = f"scos-benchmark-pg-{uuid.uuid4().hex[:12]}"
        self.admin_password = secrets.token_hex(16)
        self.app_password = secrets.token_hex(16)
        self.port = 0
        self.started = False

    def _psql(self, sql: str | None = None, file: str | None = None, timeout: float = 120) -> str:
        args = ["docker", "exec", "-i", self.name, "psql", "-q", "-At", "-U", "scos", "-d", "scos",
                "-v", "ON_ERROR_STOP=1", "-v", f"app_password={self.app_password}"]
        args += ["-f", file] if file else ["-c", sql or ""]
        done = _run(args, timeout=timeout)
        if done.returncode != 0:
            raise StackUnavailable(f"database setup failed ({file or sql}): {done.stderr.strip()[-400:]}")
        return done.stdout.strip()

    def start(self) -> None:
        if shutil.which("docker") is None:
            raise StackUnavailable("docker is not installed; the service needs a PostgreSQL database")
        probe = _run(["docker", "info", "--format", "{{.ServerVersion}}"], timeout=30)
        if probe.returncode != 0:
            raise StackUnavailable("docker daemon is not reachable: " + probe.stderr.strip()[-200:])
        database_dir = ROOT / "database"
        done = _run([
            "docker", "run", "-d", "--rm", "--name", self.name,
            "-e", "POSTGRES_USER=scos", "-e", f"POSTGRES_PASSWORD={self.admin_password}", "-e", "POSTGRES_DB=scos",
            "-p", "127.0.0.1::5432", "-v", f"{database_dir}:/scos-database:ro",
            POSTGRES_IMAGE,
            "-c", "ssl=on", "-c", "ssl_cert_file=/etc/ssl/certs/ssl-cert-snakeoil.pem",
            "-c", "ssl_key_file=/etc/ssl/private/ssl-cert-snakeoil.key",
        ], timeout=300)
        if done.returncode != 0:
            raise StackUnavailable("cannot start PostgreSQL container: " + done.stderr.strip()[-400:])
        self.started = True
        deadline = time.monotonic() + 90
        while True:
            # The entrypoint restarts the server once after initdb; wait for
            # a server that accepts TCP connections from the host side.
            ready = _run(["docker", "exec", self.name, "pg_isready", "-h", "127.0.0.1", "-U", "scos", "-d", "scos"], timeout=15)
            if ready.returncode == 0:
                probe = _run(["docker", "exec", self.name, "psql", "-h", "127.0.0.1", "-U", "scos", "-d", "scos", "-Atc", "SELECT 1"],
                             timeout=15)
                if probe.returncode == 0:
                    break
            if time.monotonic() > deadline:
                raise StackUnavailable("PostgreSQL container did not become ready")
            time.sleep(1)
        mapping = _run(["docker", "port", self.name, "5432/tcp"], timeout=15).stdout.strip().splitlines()
        if not mapping:
            raise StackUnavailable("PostgreSQL container published no port")
        self.port = int(mapping[0].rsplit(":", 1)[1])
        self._psql("CREATE TABLE IF NOT EXISTS scos_schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())")
        files = sorted((database_dir / "migrations").glob("*.sql")) + sorted((database_dir / "policies").glob("*.sql"))
        for path in files:
            self._psql(file=f"/scos-database/{path.parent.name}/{path.name}", timeout=300)
            self._psql(f"INSERT INTO scos_schema_migrations(version) VALUES ('{path.stem}')")

    def create_identity(self, label: str) -> tuple[str, str]:
        tenant_id, principal_id = str(uuid.uuid4()), str(uuid.uuid4())
        self._psql(f"INSERT INTO tenants (id, name) VALUES ('{tenant_id}', 'benchmark {label}')")
        self._psql("INSERT INTO principals (id, tenant_id, kind, external_id, display_name) "
                   f"VALUES ('{principal_id}', '{tenant_id}', 'service', 'benchmark-{label}', 'benchmark {label}')")
        return tenant_id, principal_id

    @property
    def app_url(self) -> str:
        return f"postgres://scos_app:{self.app_password}@127.0.0.1:{self.port}/scos?sslmode=require"

    def stop(self) -> None:
        if self.started:
            _run(["docker", "rm", "-f", self.name], timeout=60)
            self.started = False


class ServiceProcess:
    def __init__(self, name: str, binary: pathlib.Path, env: dict[str, str], port: int, log_path: pathlib.Path):
        self.name, self.binary, self.env, self.port, self.log_path = name, binary, env, port, log_path
        self.process: subprocess.Popen | None = None
        self.url = f"http://127.0.0.1:{port}"

    def start(self, timeout: float = 60) -> None:
        log = open(self.log_path, "wb")
        self.process = subprocess.Popen([str(self.binary)], env=self.env, stdout=log, stderr=subprocess.STDOUT,
                                        cwd=self.log_path.parent)
        log.close()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise StackUnavailable(f"{self.name} exited during startup: {self.log_tail()}")
            try:
                with urllib.request.urlopen(self.url + "/health/ready", timeout=2) as response:
                    if response.status == 200:
                        return
            except (urllib.error.URLError, OSError):
                pass
            time.sleep(0.25)
        raise StackUnavailable(f"{self.name} did not become ready: {self.log_tail()}")

    def log_tail(self) -> str:
        try:
            return self.log_path.read_text(encoding="utf-8", errors="replace")[-600:]
        except OSError:
            return ""

    def stop(self) -> None:
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)


class LocalServiceStack:
    """Real service binaries from this checkout on loopback ports."""

    def __init__(self, services: list[str]):
        wanted: list[str] = []
        for name in services:
            for dependency in DEPENDENCIES.get(name, []):
                if dependency not in wanted:
                    wanted.append(dependency)
            if name not in wanted:
                wanted.append(name)
        unknown = [name for name in wanted if name not in ADDR_ENV]
        if unknown:
            raise ValueError(f"no launch recipe for {unknown}")
        self.services = wanted
        self.workdir = pathlib.Path(tempfile.mkdtemp(prefix="scos-benchmark-stack-"))
        self.internal_token = secrets.token_hex(24)
        self.control_plane_token = secrets.token_hex(24)
        self.postgres: DisposablePostgres | None = None
        self.processes: dict[str, ServiceProcess] = {}
        self.tenant_id = ""
        self.principal_id = ""
        self.workspace = self.workdir / "workspace"
        self.binaries: dict[str, pathlib.Path] = {}

    def __enter__(self) -> "LocalServiceStack":
        try:
            self.start()
        except BaseException:
            self.close()
            raise
        return self

    def __exit__(self, *_exc: Any) -> None:
        self.close()

    def _build(self) -> None:
        if shutil.which("go") is None:
            raise StackUnavailable("the Go toolchain is not installed; service binaries cannot be built")
        bindir = self.workdir / "bin"
        bindir.mkdir(parents=True, exist_ok=True)
        for name in self.services:
            target = bindir / name
            done = _run(["go", "build", "-o", str(target), f"./services/{name}/cmd/server"], cwd=ROOT, timeout=900)
            if done.returncode != 0:
                raise StackUnavailable(f"go build {name} failed: {done.stderr.strip()[-600:]}")
            self.binaries[name] = target

    def _env(self, name: str, port: int) -> dict[str, str]:
        env = {key: os.environ[key] for key in ("PATH", "HOME", "LANG", "TZ") if key in os.environ}
        env.update({
            "SCOS_INTERNAL_SERVICE_TOKEN": self.internal_token,
            "SCOS_CONTROL_PLANE_TOKEN": self.control_plane_token,
            "SCOS_SERVICE_IDENTITY": f"benchmark-{name}",
            ADDR_ENV[name]: f"127.0.0.1:{port}",
            "SCOS_WORKSPACE_ROOT": str(self.workspace),
        })
        if name in DATABASE_SERVICES and self.postgres is not None:
            env["DATABASE_URL"] = self.postgres.app_url
        if name == "context-optimisation":
            env["SCOS_CONTEXT_SOURCE_STORE"] = str(self.workdir / "context-sources")
        if name == "code-intelligence":
            env["SCOS_CODE_INTELLIGENCE_STORE_PATH"] = str(self.workdir / "code-intelligence-v1.json")
        if name == "execution-proxy":
            env["SCOS_EXECUTION_PROXY_STORE_PATH"] = str(self.workdir / "execution-proxy-state.json")
            env["SCOS_PROXY_RAW_DIR"] = str(self.workdir / "execution-proxy-raw")
            env["SCOS_TOOL_GOVERNANCE_URL"] = self.processes["tool-governance"].url
        return env

    def start(self) -> None:
        self.workspace.mkdir(parents=True, exist_ok=True)
        self._build()
        if any(name in DATABASE_SERVICES for name in self.services):
            self.postgres = DisposablePostgres(self.workdir)
            self.postgres.start()
            self.tenant_id, self.principal_id = self.postgres.create_identity(uuid.uuid4().hex[:8])
        else:
            self.tenant_id, self.principal_id = str(uuid.uuid4()), str(uuid.uuid4())
        for name in self.services:
            port = _free_port()
            process = ServiceProcess(name, self.binaries[name], self._env(name, port), port,
                                     self.workdir / f"{name}.log")
            self.processes[name] = process
            process.start()

    def url(self, name: str) -> str:
        return self.processes[name].url

    def command_env(self, name: str) -> dict[str, str]:
        """The environment the named service runs with, which is also what a
        command it executes inherits."""
        return dict(self.processes[name].env)

    def headers(self) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {self.internal_token}",
            "X-SCOS-Control-Plane-Token": self.control_plane_token,
            "X-SCOS-Tenant-ID": self.tenant_id,
            "X-SCOS-Principal-ID": self.principal_id,
            "Content-Type": "application/json",
            "Accept": "application/json",
        }

    def metadata(self) -> dict[str, Any]:
        return {
            "services": {name: {"url": process.url, "binary_built_from": source_sha()}
                         for name, process in self.processes.items()},
            "database": POSTGRES_IMAGE if self.postgres is not None else None,
        }

    def close(self) -> None:
        for process in reversed(list(self.processes.values())):
            process.stop()
        if self.postgres is not None:
            self.postgres.stop()
        shutil.rmtree(self.workdir, ignore_errors=True)


class HTTPError(RuntimeError):
    def __init__(self, status: int, body: str):
        super().__init__(f"HTTP {status}: {body[:300]}")
        self.status = status
        self.body = body


def request_json(method: str, url: str, headers: dict[str, str], payload: Any = None, timeout: float = 180,
                 context: ssl.SSLContext | None = None) -> Any:
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout, context=context) as response:
            raw = response.read()
    except urllib.error.HTTPError as exc:
        raise HTTPError(exc.code, exc.read().decode("utf-8", "replace")) from exc
    return json.loads(raw.decode("utf-8"))
