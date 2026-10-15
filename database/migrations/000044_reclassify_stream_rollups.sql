BEGIN;

-- Server-sent event streams were recorded as scos.http.server.request with
-- their connection lifetime as the value, so every open dashboard showed up
-- as a ~10s request in the latency percentiles and "worst p95". Services now
-- record those responses as scos.http.server.stream (packages/telemetry).
-- Move the history already captured under the request measure to the stream
-- measure so past windows report the same latency the service reports now.
-- The routes are the fabric's SSE endpoints: memory-intelligence /v1/stream
-- and /v1/transparency/stream, and transparency-projection's
-- /v1/transparency/stream. A stream's lifetime has no latency histogram.
INSERT INTO observability_rollups
    (resolution, bucket_start, name, service, caller, route, unit,
     sample_count, value_sum, value_min, value_max, value_last, error_count, histogram, updated_at)
SELECT resolution, bucket_start, 'scos.http.server.stream', service, caller, route, unit,
       sample_count, value_sum, value_min, value_max, value_last, error_count, '{}', now()
  FROM observability_rollups
 WHERE name = 'scos.http.server.request'
   AND route IN ('/v1/stream', '/v1/transparency/stream')
ON CONFLICT (resolution, bucket_start, name, service, caller, route) DO UPDATE SET
    sample_count = observability_rollups.sample_count + EXCLUDED.sample_count,
    value_sum = observability_rollups.value_sum + EXCLUDED.value_sum,
    value_min = LEAST(observability_rollups.value_min, EXCLUDED.value_min),
    value_max = GREATEST(observability_rollups.value_max, EXCLUDED.value_max),
    error_count = observability_rollups.error_count + EXCLUDED.error_count,
    updated_at = now();

DELETE FROM observability_rollups
 WHERE name = 'scos.http.server.request'
   AND route IN ('/v1/stream', '/v1/transparency/stream');

COMMIT;
