package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// otlpExporter emits OTLP/HTTP JSON. It is deliberately a small wire-format
// implementation: this service does not claim to be an OpenTelemetry SDK.
type otlpExporter struct {
	endpoint string
	headers  http.Header
	client   *http.Client
	service  string
	resource map[string]string
}

func newOTLPExporterFromEnvironment() (*otlpExporter, error) {
	raw := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("SCOS_OTLP_ENDPOINT"))
	}
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("OTLP endpoint must be an absolute http(s) URL")
	}
	service := os.Getenv("OTEL_SERVICE_NAME")
	if service == "" {
		service = "scos-observability"
	}
	headers := make(http.Header)
	for _, item := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) == 2 && parts[0] != "" {
			headers.Set(parts[0], parts[1])
		}
	}
	resource := map[string]string{
		"service.name":    service,
		"service.version": version,
	}
	if host := os.Getenv("SCOS_DEPLOYMENT_ENVIRONMENT"); host != "" {
		resource["deployment.environment"] = host
	}
	return &otlpExporter{endpoint: strings.TrimRight(raw, "/"), headers: headers, client: &http.Client{Timeout: 5 * time.Second}, service: service, resource: resource}, nil
}

func (e *otlpExporter) signalURL(signal string) string {
	if strings.HasSuffix(e.endpoint, "/v1/"+signal) {
		return e.endpoint
	}
	return e.endpoint + "/v1/" + signal
}

func (e *otlpExporter) ready(ctx context.Context) error {
	if e == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.endpoint, nil)
	if err != nil {
		return errors.New("OTLP endpoint is invalid")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return errors.New("OTLP endpoint is unreachable")
	}
	defer resp.Body.Close()
	// A collector may return 404/405 for a base URL; the network and URL are
	// still verified. Export requests themselves require a 2xx response.
	return nil
}

func (e *otlpExporter) post(ctx context.Context, signal string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.signalURL(signal), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, values := range e.headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OTLP %s export returned HTTP %d", signal, resp.StatusCode)
	}
	return nil
}

type otlpAttribute struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue string   `json:"stringValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func stringAttributes(values map[string]string) []otlpAttribute {
	result := make([]otlpAttribute, 0, len(values))
	for key, value := range values {
		result = append(result, otlpAttribute{Key: key, Value: otlpAnyValue{StringValue: value}})
	}
	return result
}

func resource(attributes map[string]string) map[string]any {
	return map[string]any{"attributes": stringAttributes(attributes)}
}

func nanos(timestamp string) string {
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "0"
	}
	return strconv.FormatInt(parsed.UnixNano(), 10)
}

func otlpID(value string, bytes int) string {
	clean := strings.ToLower(strings.TrimSpace(value))
	if len(clean) == bytes*2 {
		if _, err := hex.DecodeString(clean); err == nil {
			return clean
		}
	}
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:bytes])
}

func (e *otlpExporter) exportMetric(ctx context.Context, point *MetricPoint) error {
	value := point.Value
	attrs := make(map[string]string, len(point.Labels)+4)
	for key, item := range point.Labels {
		attrs[key] = item
	}
	if point.Service() != "" {
		attrs["service.name"] = point.Service()
	}
	if point.TraceID != "" {
		attrs["trace_id"] = point.TraceID
	}
	if point.SpanID != "" {
		attrs["span_id"] = point.SpanID
	}
	if point.SessionID != "" {
		attrs["session_id"] = point.SessionID
	}
	if point.TurnID != "" {
		attrs["turn_id"] = point.TurnID
	}
	if point.TaskID != "" {
		attrs["task_id"] = point.TaskID
	}
	if point.OperationID != "" {
		attrs["operation_id"] = point.OperationID
	}
	if point.AgentID != "" {
		attrs["agent_id"] = point.AgentID
	}
	if point.Tenant != "" {
		attrs["tenant"] = point.Tenant
	}
	return e.post(ctx, "metrics", map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource": resource(e.resource),
			"scopeMetrics": []any{map[string]any{
				"scope": map[string]string{"name": "scos-observability"},
				"metrics": []any{map[string]any{
					"name": point.Name, "unit": point.Unit,
					"gauge": map[string]any{"dataPoints": []any{map[string]any{
						"timeUnixNano": nanos(point.Timestamp), "asDouble": value,
						"attributes": stringAttributes(attrs),
					}}},
				}},
			}},
		}},
	})
}

func (p *MetricPoint) Service() string {
	if p.Labels == nil {
		return ""
	}
	return p.Labels["service"]
}

func (e *otlpExporter) exportTrace(ctx context.Context, span *TraceSpan) error {
	attrs := make(map[string]string, len(span.Attributes)+5)
	for key, value := range span.Attributes {
		attrs[key] = value
	}
	if span.SessionID != "" {
		attrs["session_id"] = span.SessionID
	}
	if span.TurnID != "" {
		attrs["turn_id"] = span.TurnID
	}
	if span.TaskID != "" {
		attrs["task_id"] = span.TaskID
	}
	if span.OperationID != "" {
		attrs["operation_id"] = span.OperationID
	}
	if span.AgentID != "" {
		attrs["agent_id"] = span.AgentID
	}
	if span.Tenant != "" {
		attrs["tenant"] = span.Tenant
	}
	otlpSpan := map[string]any{
		"traceId": otlpID(span.TraceID, 16), "spanId": otlpID(span.SpanID, 8),
		"name": span.Name, "startTimeUnixNano": nanos(span.StartTime),
		"endTimeUnixNano": nanos(span.EndTime), "kind": 1,
		"attributes": stringAttributes(attrs),
	}
	if span.ParentID != "" {
		otlpSpan["parentSpanId"] = otlpID(span.ParentID, 8)
	}
	return e.post(ctx, "traces", map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": resource(e.resource),
			"scopeSpans": []any{map[string]any{
				"scope": map[string]string{"name": "scos-observability"},
				"spans": []any{otlpSpan},
			}},
		}},
	})
}

func (e *otlpExporter) exportLog(ctx context.Context, entry *LogEntry) error {
	attrs := make(map[string]string, len(entry.Labels)+3)
	for key, value := range entry.Labels {
		attrs[key] = value
	}
	attrs["service.name"] = entry.Service
	if entry.SessionID != "" {
		attrs["session_id"] = entry.SessionID
	}
	if entry.TurnID != "" {
		attrs["turn_id"] = entry.TurnID
	}
	if entry.TaskID != "" {
		attrs["task_id"] = entry.TaskID
	}
	if entry.OperationID != "" {
		attrs["operation_id"] = entry.OperationID
	}
	if entry.AgentID != "" {
		attrs["agent_id"] = entry.AgentID
	}
	if entry.Tenant != "" {
		attrs["tenant"] = entry.Tenant
	}
	logRecord := map[string]any{
		"timeUnixNano": nanos(entry.Timestamp), "severityText": entry.Level,
		"body": otlpAnyValue{StringValue: entry.Message}, "attributes": stringAttributes(attrs),
	}
	if entry.TraceID != "" {
		logRecord["traceId"] = otlpID(entry.TraceID, 16)
	}
	if entry.SpanID != "" {
		logRecord["spanId"] = otlpID(entry.SpanID, 8)
	}
	return e.post(ctx, "logs", map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource": resource(e.resource),
			"scopeLogs": []any{map[string]any{
				"scope":      map[string]string{"name": "scos-observability"},
				"logRecords": []any{logRecord},
			}},
		}},
	})
}

func traceparent(value string) (traceID, spanID string, ok bool) {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || parts[3] == "0000000000000000" {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1] + parts[2] + parts[3]); err != nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}
