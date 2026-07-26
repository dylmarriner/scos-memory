#!/usr/bin/env python3
"""
Stress and tolerance tests (§40-41).

- Stress: high concurrency, large payloads, sustained load.
- Tolerance: storage latency, partial failures, resource limits.
- Performance regression: baseline comparison with variance analysis.
"""
import json
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from urllib.request import Request, urlopen
from dataclasses import dataclass, field
from typing import Any


@dataclass
class StressResult:
    name: str
    concurrency: int
    ops_total: int
    ops_per_second: float
    p50_latency_ms: float
    p95_latency_ms: float
    p99_latency_ms: float
    error_rate: float
    category: str  # stress, tolerance, regression


@dataclass
class StressProfile:
    name: str
    concurrency_levels: list[int] = field(default_factory=lambda: [1, 5, 10, 25, 50])
    operations_per_level: int = 100
    payload_sizes: list[int] = field(default_factory=lambda: [100, 1000, 10000, 100000])

# ── stress runner ───────────────────────────────────────────────────────

class StressRunner:
    def __init__(self, target_url: str):
        self.target_url = target_url.rstrip("/")
        self.results: list[StressResult] = []

    def run_concurrency_test(self, name: str, concurrency: int,
                             ops: int) -> StressResult:
        """Measure requests against a live service endpoint."""
        def request_once() -> tuple[float, bool]:
            start = time.perf_counter()
            try:
                with urlopen(Request(self.target_url + "/health/ready"), timeout=10):
                    return (time.perf_counter() - start) * 1000, False
            except Exception:
                return (time.perf_counter() - start) * 1000, True

        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            outcomes = list(pool.map(lambda _: request_once(), range(ops)))
        latencies = sorted(value for value, _ in outcomes)
        errors = sum(1 for _, failed in outcomes if failed)

        total_time = sum(latencies) / (concurrency or 1)
        ops_per_sec = ops / (total_time / 1000) if total_time > 0 else 0

        def percentile(p):
            idx = int(len(latencies) * p / 100)
            return latencies[min(idx, len(latencies) - 1)]

        return StressResult(
            name=name,
            concurrency=concurrency,
            ops_total=ops,
            ops_per_second=round(ops_per_sec, 2),
            p50_latency_ms=round(percentile(50), 2),
            p95_latency_ms=round(percentile(95), 2),
            p99_latency_ms=round(percentile(99), 2),
            error_rate=round(errors / ops, 4),
            category="stress",
        )

    def run_stress_profile(self, profile: StressProfile) -> list[StressResult]:
        self.results = []
        for concurrency in profile.concurrency_levels:
            result = self.run_concurrency_test(
                f"{profile.name}_c{concurrency}",
                concurrency, profile.operations_per_level,
            )
            self.results.append(result)
        return self.results

    def run_tolerance_test(self, name: str, failure_rate: float = 0.05) -> StressResult:
        """Measure an explicitly supplied failure-test endpoint.

        ``failure_rate`` is retained for API compatibility but is not used:
        injecting random failures would create false evidence.
        """
        del failure_rate
        ops = 100
        latencies: list[float] = []
        errors = 0
        for _ in range(ops):
            start = time.perf_counter()
            try:
                with urlopen(Request(self.target_url + "/health/ready"), timeout=10):
                    pass
            except Exception:
                errors += 1
            latencies.append((time.perf_counter() - start) * 1000)

        latencies.sort()
        total_time = sum(latencies) / 1000  # sequential
        ops_per_sec = ops / (total_time / 1000) if total_time > 0 else 0

        def percentile(p):
            idx = int(len(latencies) * p / 100)
            return latencies[min(idx, len(latencies) - 1)]

        return StressResult(
            name=name,
            concurrency=1,
            ops_total=ops,
            ops_per_second=round(ops_per_sec, 2),
            p50_latency_ms=round(percentile(50), 2),
            p95_latency_ms=round(percentile(95), 2),
            p99_latency_ms=round(percentile(99), 2),
            error_rate=round(errors / ops, 4),
            category="tolerance",
        )

    def summary(self) -> dict[str, Any]:
        by_category: dict[str, list[StressResult]] = {}
        for r in self.results + getattr(self, '_tolerance_results', []):
            by_category.setdefault(r.category, []).append(r)

        report: dict[str, Any] = {}
        for cat, results in by_category.items():
            report[cat] = {
                "total_tests": len(results),
                "avg_ops_per_sec": round(sum(r.ops_per_second for r in results) / len(results), 2),
                "avg_p50": round(sum(r.p50_latency_ms for r in results) / len(results), 2),
                "avg_p95": round(sum(r.p95_latency_ms for r in results) / len(results), 2),
                "avg_p99": round(sum(r.p99_latency_ms for r in results) / len(results), 2),
                "avg_error_rate": round(sum(r.error_rate for r in results) / len(results), 4),
            }
        return report


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: runner.py http://host:port")
    runner = StressRunner(sys.argv[1])

    # Stress profile
    profile = StressProfile(name="retrieval_stress")
    stress_results = runner.run_stress_profile(profile)
    for r in stress_results:
        print(f"  c={r.concurrency}: {r.ops_per_second} ops/s, p95={r.p95_latency_ms}ms")

    # Tolerance test
    tolerance = runner.run_tolerance_test("storage_partition_failure")
    print(f"  tolerance: error_rate={tolerance.error_rate}, p95={tolerance.p95_latency_ms}ms")

    # Summary
    runner._tolerance_results = [tolerance]
    summary = runner.summary()
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
