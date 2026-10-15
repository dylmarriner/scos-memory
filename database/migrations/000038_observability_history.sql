BEGIN;

-- Historic telemetry for the transparency dashboard. The observability
-- service keeps only a bounded ring of raw points (10k), which at fabric
-- request rates covers minutes. These tables hold durable aggregates so the
-- dashboard can chart days and weeks of real history and reconstruct the
-- service-to-service flow graph for any past window.
--
-- One row per (resolution, bucket, metric, service, caller, route). caller and
-- route are '' for metrics that are not inbound request measures. The latency
-- histogram uses fixed log2 millisecond buckets (see
-- services/observability/cmd/server/rollup.go) so percentiles can be merged
-- across buckets exactly, which averaged percentiles cannot.
CREATE TABLE IF NOT EXISTS observability_rollups (
    resolution text NOT NULL CHECK (resolution IN ('1m', '1h')),
    bucket_start timestamptz NOT NULL,
    name text NOT NULL,
    service text NOT NULL DEFAULT '',
    caller text NOT NULL DEFAULT '',
    route text NOT NULL DEFAULT '',
    unit text NOT NULL DEFAULT '',
    sample_count bigint NOT NULL CHECK (sample_count >= 0),
    value_sum double precision NOT NULL,
    value_min double precision NOT NULL,
    value_max double precision NOT NULL,
    value_last double precision NOT NULL,
    error_count bigint NOT NULL DEFAULT 0 CHECK (error_count >= 0),
    histogram bigint[] NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resolution, bucket_start, name, service, caller, route)
);
CREATE INDEX IF NOT EXISTS observability_rollups_name_time_idx
    ON observability_rollups (resolution, name, bucket_start);

-- Benchmark runs recorded by benchmarks/run_deployed.py and
-- benchmarks/run_acceptance.py. Each row is one real measured run; the
-- dashboard charts their history. Nothing here is synthesised by the service.
CREATE TABLE IF NOT EXISTS observability_benchmark_runs (
    run_id text PRIMARY KEY,
    suite text NOT NULL,
    recorded_at timestamptz NOT NULL,
    run jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS observability_benchmark_runs_suite_time_idx
    ON observability_benchmark_runs (suite, recorded_at DESC);

ALTER TABLE observability_rollups ENABLE ROW LEVEL SECURITY;
ALTER TABLE observability_rollups FORCE ROW LEVEL SECURITY;
ALTER TABLE observability_benchmark_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE observability_benchmark_runs FORCE ROW LEVEL SECURITY;

-- Fabric-level telemetry, not tenant data: owned by the observability
-- service identity, the same pattern as extension_manager_state.
CREATE POLICY observability_rollups_service_access ON observability_rollups
    FOR ALL USING (NULLIF(current_setting('scos.service_id', true), '') = 'observability')
    WITH CHECK (NULLIF(current_setting('scos.service_id', true), '') = 'observability');
CREATE POLICY observability_benchmark_runs_service_access ON observability_benchmark_runs
    FOR ALL USING (NULLIF(current_setting('scos.service_id', true), '') = 'observability')
    WITH CHECK (NULLIF(current_setting('scos.service_id', true), '') = 'observability');

COMMIT;
