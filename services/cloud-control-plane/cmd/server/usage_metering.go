package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Usage metering for the hosted MCP proxy. Every proxied JSON-RPC tools/call
// is classified as a memory read, a memory write or neither, and counted only
// when the gateway's response for that call id is a success — a tool error is
// not usage. bytes_stored is measured from the tenant's live memory content
// rather than accumulated, because storage is a stock, not a flow.

var memoryReadTools = map[string]bool{
	"scos_recall":         true,
	"scos_retrieve":       true,
	"scos_explain":        true,
	"scos_user_model":     true,
	"scos_memory_graph":   true,
	"scos_memory_summary": true,
	"scos_fact_history":   true,
	"scos_mg_turns_list":  true,
}

var memoryWriteTools = map[string]bool{
	"scos_remember":          true,
	"scos_graphify_ingest":   true,
	"scos_correct":           true,
	"scos_correct_batch":     true,
	"scos_forget":            true,
	"scos_forget_batch":      true,
	"scos_forget_all":        true,
	"scos_fact_feedback":     true,
	"scos_link_evidence":     true,
	"scos_mg_session_create": true,
	"scos_mg_message_append": true,
	"scos_mg_chunk_append":   true,
	"scos_mg_response_start": true,
	"scos_mg_turn_complete":  true,
}

// maxMeteredBody bounds how much of a request/response is inspected for
// metering. Larger bodies are still proxied in full; they are just counted
// as API requests only.
const maxMeteredBody = 1 << 20

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
	Result *struct {
		IsError bool `json:"isError"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

func decodeRPC(raw []byte) []rpcMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var batch []rpcMessage
		if json.Unmarshal(raw, &batch) == nil {
			return batch
		}
		return nil
	}
	var one rpcMessage
	if json.Unmarshal(raw, &one) == nil {
		return []rpcMessage{one}
	}
	return nil
}

// meteredCalls maps JSON-RPC call ids to their memory class ("read" or
// "write") for the tools/call requests in body.
func meteredCalls(body []byte) map[string]string {
	calls := map[string]string{}
	for _, msg := range decodeRPC(body) {
		if msg.Method != "tools/call" || len(msg.ID) == 0 {
			continue
		}
		switch {
		case memoryReadTools[msg.Params.Name]:
			calls[string(msg.ID)] = "read"
		case memoryWriteTools[msg.Params.Name]:
			calls[string(msg.ID)] = "write"
		}
	}
	return calls
}

// successfulIDs returns the ids of successful JSON-RPC responses in a
// gateway response body, which is either application/json or a
// text/event-stream of JSON-RPC messages.
func successfulIDs(body []byte, contentType string) map[string]bool {
	ok := map[string]bool{}
	collect := func(raw []byte) {
		for _, msg := range decodeRPC(raw) {
			if len(msg.ID) == 0 || len(msg.Error) > 0 && string(msg.Error) != "null" || msg.Result == nil || msg.Result.IsError {
				continue
			}
			ok[string(msg.ID)] = true
		}
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 64*1024), maxMeteredBody)
		var data []byte
		flush := func() {
			if len(data) > 0 {
				collect(data)
				data = nil
			}
		}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
			}
		}
		flush()
		return ok
	}
	collect(body)
	return ok
}

// countMemoryOps returns the successful read and write counts.
func countMemoryOps(requestBody, responseBody []byte, contentType string) (reads, writes int64) {
	calls := meteredCalls(requestBody)
	if len(calls) == 0 {
		return 0, 0
	}
	for id := range successfulIDs(responseBody, contentType) {
		switch calls[id] {
		case "read":
			reads++
		case "write":
			writes++
		}
	}
	return reads, writes
}

func (s *server) recordMemoryOps(ctx context.Context, tenantID string, reads, writes int64) error {
	if reads == 0 && writes == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cloud_usage_daily(tenant_id,usage_date,memory_reads,memory_writes) VALUES($1,current_date,$2,$3)
		ON CONFLICT (tenant_id,usage_date) DO UPDATE SET memory_reads=cloud_usage_daily.memory_reads+EXCLUDED.memory_reads, memory_writes=cloud_usage_daily.memory_writes+EXCLUDED.memory_writes`,
		tenantID, reads, writes)
	return err
}

// measureBytesStored sums the content of the current version of every memory
// the principal can see in its tenant, under the same RLS context the data
// plane uses, and records it as today's snapshot.
func (s *server) measureBytesStored(ctx context.Context, tenantID, principalID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, tenantID, principalID); err != nil {
		return 0, err
	}
	var bytesStored int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(octet_length(v.content)),0)
		FROM memory_items i
		JOIN memory_versions v ON v.id = i.current_version_id
		WHERE i.tenant_id = $1 AND i.status IN ('candidate', 'active', 'quarantined')`, tenantID).Scan(&bytesStored); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cloud_usage_daily(tenant_id,usage_date,bytes_stored) VALUES($1,current_date,$2)
		ON CONFLICT (tenant_id,usage_date) DO UPDATE SET bytes_stored=EXCLUDED.bytes_stored`, tenantID, bytesStored); err != nil {
		return 0, err
	}
	return bytesStored, nil
}
