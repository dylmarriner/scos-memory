package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const rfc3339 = time.RFC3339

func parseRFC3339OrNow(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}

// requestIdentity is the authenticated tenant/principal pair a request acts
// as, resolved from claims or internal headers per the convention used by
// hermes-provider, autonomous-control-plane and memory-gateway.
type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

func setPrincipalContext(ctx context.Context, tx *sql.Tx, id requestIdentity) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, id.TenantID, id.PrincipalID)
	return err
}

// ensureAdapterCatalogRow upserts the tenant's copy of a boundary's static
// catalog entry (tier, capabilities, supported/unsupported operations) and
// returns its row id, so adapter_instances has a stable foreign key to point
// at. The catalog data itself is compiled into the binary (registerDefaultAdapters)
// rather than caller-writable, so this is a lazy materialization, not a write path.
func ensureAdapterCatalogRow(ctx context.Context, tx *sql.Tx, tenantID string, a *Adapter) (string, error) {
	configuration, err := json.Marshal(map[string]any{
		"name":                   a.Name,
		"tier":                   a.Tier,
		"capabilities":           a.Capabilities,
		"supported_operations":   a.Supported,
		"unsupported_operations": a.Unsupported,
		"tested":                 a.Tested,
	})
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_adapters (tenant_id, adapter_id, boundary, status, configuration)
		VALUES ($1,$2,$3,'enabled',$4)
		ON CONFLICT (tenant_id, adapter_id) DO UPDATE SET boundary = EXCLUDED.boundary, configuration = EXCLUDED.configuration
		RETURNING id
	`, tenantID, a.Boundary, a.Boundary, configuration).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("materialize adapter catalog row: %w", err)
	}
	return id, nil
}

// upsertAgentInstance records (or refreshes) a real agent registration
// against its adapter's catalog row. Each registration becomes one durable
// row keyed by (tenant_id, instance_key) rather than a field inside a
// per-service JSON blob.
func upsertAgentInstance(ctx context.Context, tx *sql.Tx, tenantID, adapterRowID string, reg AgentRegistration) error {
	metadata, err := json.Marshal(map[string]any{
		"name":         reg.Name,
		"boundary":     reg.Boundary,
		"tier":         reg.Tier,
		"adapters":     reg.Adapters,
		"capabilities": reg.Capabilities,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO adapter_instances (tenant_id, adapter_id, agent_id, instance_key, status, last_seen_at, metadata)
		VALUES ($1,$2,NULL,$3,$4,$5,$6)
		ON CONFLICT (tenant_id, instance_key) DO UPDATE SET
			adapter_id = EXCLUDED.adapter_id,
			status = EXCLUDED.status,
			last_seen_at = EXCLUDED.last_seen_at,
			metadata = EXCLUDED.metadata
	`, tenantID, adapterRowID, reg.AgentID, reg.Status, parseRFC3339OrNow(reg.LastSeen), metadata)
	if err != nil {
		return fmt.Errorf("upsert agent instance: %w", err)
	}
	return nil
}

// agentInstanceRow is what a joined adapter_instances/agent_adapters query
// yields back into the AgentRegistration shape the API has always returned.
type agentInstanceRow struct {
	InstanceKey string
	Boundary    string
	Status      string
	LastSeen    sql.NullTime
	Metadata    []byte
}

func scanAgentRegistration(row agentInstanceRow) (*AgentRegistration, error) {
	var meta struct {
		Name         string   `json:"name"`
		Tier         string   `json:"tier"`
		Adapters     []string `json:"adapters"`
		Capabilities []string `json:"capabilities"`
	}
	if len(row.Metadata) > 0 {
		if err := json.Unmarshal(row.Metadata, &meta); err != nil {
			return nil, err
		}
	}
	lastSeen := ""
	if row.LastSeen.Valid {
		lastSeen = row.LastSeen.Time.UTC().Format(rfc3339)
	}
	return &AgentRegistration{
		AgentID:      row.InstanceKey,
		Name:         meta.Name,
		Boundary:     row.Boundary,
		Tier:         meta.Tier,
		Adapters:     meta.Adapters,
		Capabilities: meta.Capabilities,
		Status:       row.Status,
		LastSeen:     lastSeen,
	}, nil
}

func listAgentInstances(ctx context.Context, db *sql.DB, tenantID string) ([]*AgentRegistration, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ai.instance_key, aa.boundary, ai.status, ai.last_seen_at, ai.metadata
		FROM adapter_instances ai
		JOIN agent_adapters aa ON aa.id = ai.adapter_id
		WHERE ai.tenant_id = $1
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AgentRegistration
	for rows.Next() {
		var row agentInstanceRow
		if err := rows.Scan(&row.InstanceKey, &row.Boundary, &row.Status, &row.LastSeen, &row.Metadata); err != nil {
			return nil, err
		}
		reg, err := scanAgentRegistration(row)
		if err != nil {
			return nil, err
		}
		out = append(out, reg)
	}
	return out, rows.Err()
}

func getAgentInstance(ctx context.Context, db *sql.DB, tenantID, agentID string) (*AgentRegistration, bool, error) {
	var row agentInstanceRow
	err := db.QueryRowContext(ctx, `
		SELECT ai.instance_key, aa.boundary, ai.status, ai.last_seen_at, ai.metadata
		FROM adapter_instances ai
		JOIN agent_adapters aa ON aa.id = ai.adapter_id
		WHERE ai.tenant_id = $1 AND ai.instance_key = $2
	`, tenantID, agentID).Scan(&row.InstanceKey, &row.Boundary, &row.Status, &row.LastSeen, &row.Metadata)
	switch {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	reg, err := scanAgentRegistration(row)
	if err != nil {
		return nil, false, err
	}
	return reg, true, nil
}

// upsertEnvelope persists a context envelope for (tenant_id, session_id),
// replacing any prior envelope for that session (the envelope is a live
// working snapshot rebuilt every turn, not an append-only history).
func upsertEnvelope(ctx context.Context, tx *sql.Tx, tenantID string, env ContextEnvelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO context_envelopes (tenant_id, session_id, agent_id, envelope, format, updated_at)
		VALUES ($1,$2,$3,$4,$5, now())
		ON CONFLICT (tenant_id, session_id) DO UPDATE SET
			agent_id = EXCLUDED.agent_id,
			envelope = EXCLUDED.envelope,
			format = EXCLUDED.format,
			updated_at = now()
	`, tenantID, env.SessionID, env.AgentID, payload, env.Format)
	if err != nil {
		return fmt.Errorf("upsert context envelope: %w", err)
	}
	return nil
}

func getEnvelope(ctx context.Context, db *sql.DB, tenantID, sessionID string) (*ContextEnvelope, bool, error) {
	var payload []byte
	err := db.QueryRowContext(ctx, `SELECT envelope FROM context_envelopes WHERE tenant_id=$1 AND session_id=$2`, tenantID, sessionID).Scan(&payload)
	switch {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	var env ContextEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, false, err
	}
	return &env, true, nil
}
