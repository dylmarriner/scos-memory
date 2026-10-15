#!/usr/bin/env python3
"""Fail-closed client for benchmarks against a deployed SCOS code service.

The deployed runner is deliberately separate from the fixture benchmark. It
requires an explicitly configured endpoint, bearer token, tenant and
principal, and never falls back to local data when the remote service fails.
"""
from __future__ import annotations

import json
import os
import ssl
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class BenchmarkConfig:
    endpoint: str
    token: str
    tenant_id: str
    principal_id: str
    repository_path: str = "scos-benchmark-fixtures"
    timeout_seconds: float = 10.0
    # A service reached directly (not through an ingress that adds it) is
    # wrapped in auth.ControlPlaneHandler and needs the control-plane token.
    control_plane_token: str = ""
    ca_file: str = ""

    def __post_init__(self) -> None:
        endpoint = self.endpoint.rstrip("/")
        if not endpoint.startswith(("https://", "http://")):
            raise RuntimeError("SCOS_BENCHMARK_ENDPOINT must be an HTTP(S) URL")
        parsed = urllib.parse.urlsplit(endpoint)
        if parsed.username or parsed.password:
            raise RuntimeError("SCOS_BENCHMARK_ENDPOINT must not contain userinfo")
        if not all((self.token.strip(), self.tenant_id.strip(), self.principal_id.strip())):
            raise RuntimeError("benchmark token, tenant and principal are required")
        object.__setattr__(self, "endpoint", endpoint)

    def metadata(self) -> dict[str, Any]:
        parsed = urllib.parse.urlsplit(self.endpoint)
        public_endpoint = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, "", "", ""))
        return {
            "endpoint": public_endpoint,
            "tenant_id": self.tenant_id,
            "principal_id": self.principal_id,
            "repository_path": self.repository_path,
            "timeout_seconds": self.timeout_seconds,
            "control_plane_token_configured": bool(self.control_plane_token),
        }


def config_from_environment() -> BenchmarkConfig:
    required = {
        "SCOS_BENCHMARK_ENDPOINT": os.getenv("SCOS_BENCHMARK_ENDPOINT", "").strip(),
        "SCOS_BENCHMARK_TOKEN": os.getenv("SCOS_BENCHMARK_TOKEN", "").strip(),
        "SCOS_BENCHMARK_TENANT_ID": os.getenv("SCOS_BENCHMARK_TENANT_ID", "").strip(),
        "SCOS_BENCHMARK_PRINCIPAL_ID": os.getenv("SCOS_BENCHMARK_PRINCIPAL_ID", "").strip(),
    }
    for name, value in required.items():
        if not value:
            raise RuntimeError(f"{name} is required for a deployed benchmark")
    try:
        timeout = float(os.getenv("SCOS_BENCHMARK_TIMEOUT_SECONDS", "10"))
    except ValueError as exc:
        raise RuntimeError("SCOS_BENCHMARK_TIMEOUT_SECONDS must be numeric") from exc
    if timeout <= 0:
        raise RuntimeError("SCOS_BENCHMARK_TIMEOUT_SECONDS must be positive")
    return BenchmarkConfig(
        endpoint=required["SCOS_BENCHMARK_ENDPOINT"],
        token=required["SCOS_BENCHMARK_TOKEN"],
        tenant_id=required["SCOS_BENCHMARK_TENANT_ID"],
        principal_id=required["SCOS_BENCHMARK_PRINCIPAL_ID"],
        repository_path=os.getenv("SCOS_BENCHMARK_REPOSITORY_PATH", "scos-benchmark-fixtures").strip(),
        timeout_seconds=timeout,
        control_plane_token=os.getenv("SCOS_BENCHMARK_CONTROL_PLANE_TOKEN", "").strip(),
        ca_file=os.getenv("SCOS_BENCHMARK_CA_FILE", os.getenv("SCOS_TLS_CA_FILE", "")).strip(),
    )


class DeployedCodeClient:
    def __init__(self, config: BenchmarkConfig):
        self.config = config
        self.context = ssl.create_default_context(cafile=config.ca_file) if config.ca_file else None

    def _request(self, method: str, path: str, payload: dict[str, Any] | None = None) -> dict[str, Any]:
        body = None if payload is None else json.dumps(payload, sort_keys=True).encode("utf-8")
        headers = {
            "Accept": "application/json",
            "Authorization": f"Bearer {self.config.token}",
            "Content-Type": "application/json",
            "X-SCOS-Tenant-ID": self.config.tenant_id,
            "X-SCOS-Principal-ID": self.config.principal_id,
        }
        if self.config.control_plane_token:
            headers["X-SCOS-Control-Plane-Token"] = self.config.control_plane_token
        request = urllib.request.Request(
            f"{self.config.endpoint}/{path.lstrip('/')}",
            data=body,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=self.config.timeout_seconds, context=self.context) as response:
                if response.status < 200 or response.status >= 300:
                    raise RuntimeError(f"deployed benchmark HTTP status {response.status}")
                raw = response.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace").strip()[:300]
            raise RuntimeError(f"deployed benchmark request failed: {method} {path}: HTTP {exc.code}: {detail}") from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise RuntimeError(f"deployed benchmark request failed: {method} {path}") from exc
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise RuntimeError(f"deployed benchmark returned invalid JSON: {method} {path}") from exc
        if not isinstance(value, dict):
            raise RuntimeError(f"deployed benchmark returned a non-object: {method} {path}")
        return value

    def health(self) -> dict[str, Any]:
        return self._request("GET", "/health/ready")

    def version(self) -> dict[str, Any]:
        value = self._request("GET", "/v1/version")
        if not isinstance(value.get("version"), str) or not value["version"].strip():
            raise RuntimeError("deployed version response missing version")
        return value

    def release(self) -> dict[str, Any]:
        value = self._request("GET", "/health/version")
        for key in ("source_sha", "build_time"):
            if not isinstance(value.get(key), str) or not value[key].strip():
                raise RuntimeError(f"deployed release response missing {key}")
        return value

    def ensure_repository(self) -> str:
        value = self._request("POST", "/v2/code/repos", {"canonical_path": self.config.repository_path})
        repository_id = value.get("repository_id")
        if not isinstance(repository_id, str) or not repository_id:
            raise RuntimeError("deployed repository response missing repository_id")
        return repository_id

    def index(self, repository_id: str, branch: str, commit_id: str,
              files: list[dict[str, Any]], extractor_version: str = "benchmark-v1",
              configuration_hash: str = "benchmark-config-v1") -> dict[str, Any]:
        value = self._request("POST", "/v2/code/index", {
            "repository_id": repository_id,
            "branch": branch,
            "commit_id": commit_id,
            "extractor_version": extractor_version,
            "configuration_hash": configuration_hash,
            "files": files,
        })
        if not isinstance(value.get("snapshot_id"), str) or not value["snapshot_id"]:
            raise RuntimeError("deployed index response missing snapshot_id")
        return value

    def query(self, repository_id: str, branch: str, query: str) -> list[dict[str, Any]]:
        value = self._request("POST", "/v2/code/query", {
            "repository_id": repository_id,
            "branch": branch,
            "query": query,
        })
        results = value.get("results")
        if not isinstance(results, list) or not all(isinstance(item, dict) for item in results):
            raise RuntimeError("deployed query response missing results")
        return results

    def snapshot_diff(self, snapshot_id: str) -> dict[str, Any]:
        return self._request("POST", "/v2/code/snapshots/diff", {"snapshot_id": snapshot_id})
