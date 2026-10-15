package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// requestIdentity is the authenticated tenant/principal pair a request acts
// as, resolved from claims or internal headers per the convention used by
// hermes-provider, retrieval-orchestrator and autonomous-control-plane.
type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16])), nil
}

// isUUID reports whether s is a UUID in either form PostgreSQL's uuid type
// accepts from this fabric's callers: canonical 8-4-4-4-12 hex, or the 32
// bare hex digits autonomous-control-plane's identifiers use. Every tool
// and operation identifier in this service's tables is a uuid column, so
// anything else is a malformed request rather than a database error.
func isUUID(s string) bool {
	switch len(s) {
	case 36:
		for i, c := range s {
			switch i {
			case 8, 13, 18, 23:
				if c != '-' {
					return false
				}
			default:
				if !isHexDigit(c) {
					return false
				}
			}
		}
		return true
	case 32:
		for _, c := range s {
			if !isHexDigit(c) {
				return false
			}
		}
		return true
	}
	return false
}

func isHexDigit(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func sha256Hex(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func setPrincipalContext(ctx context.Context, tx *sql.Tx, id requestIdentity) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, id.TenantID, id.PrincipalID)
	return err
}

// idempotentReplay looks up a previously recorded response for key. found is
// false when no prior execution exists, in which case the caller must
// perform the operation and call recordIdempotent to store the result.
func idempotentReplay(ctx context.Context, tx *sql.Tx, id requestIdentity, key, requestHash string, out any) (found bool, err error) {
	var storedHash, storedResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`,
		id.TenantID, id.PrincipalID, key).Scan(&storedHash, &storedResponse)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	if storedHash != requestHash {
		return false, fmt.Errorf("key %q was already recorded with different input", key)
	}
	if storedResponse != "" && out != nil {
		if jsonErr := json.Unmarshal([]byte(storedResponse), out); jsonErr != nil {
			return false, fmt.Errorf("stored idempotency result is invalid: %w", jsonErr)
		}
	}
	return true, nil
}

func recordIdempotent(ctx context.Context, tx *sql.Tx, id requestIdentity, key, operationType, requestHash string, response any) error {
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO idempotency_keys (tenant_id, principal_id, key, operation_type, request_hash, status, response_reference, expires_at)
		VALUES ($1,$2,$3,$4,$5,'succeeded',$6, now() + interval '24 hours')
	`, id.TenantID, id.PrincipalID, key, operationType, requestHash, string(body))
	return err
}

// registerTool inserts a new tool_definitions row in state 'proposed'.
// Registration is not idempotent-replay-guarded on the tool identity itself
// (tenant_id, name) is unique, so a duplicate name surfaces as a constraint
// violation the caller maps to 409 -- the Idempotency-Key header still
// governs replay of the exact same registration request.
func registerTool(ctx context.Context, tx *sql.Tx, id requestIdentity, tool *ToolDefinition) error {
	generated, err := newID()
	if err != nil {
		return err
	}
	tool.ID = generated
	tool.State = StateProposed
	tool.CurrentState = StateProposed
	props, err := json.Marshal(tool.Props)
	if err != nil {
		return err
	}
	row := tx.QueryRowContext(ctx, `
		INSERT INTO tool_definitions (id, tenant_id, name, description, version, properties, state, owner_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6,'proposed',$7)
		RETURNING created_at, updated_at
	`, tool.ID, id.TenantID, tool.Name, tool.Description, tool.Version, props, id.PrincipalID)
	var createdAt, updatedAt string
	if err := row.Scan(&createdAt, &updatedAt); err != nil {
		return err
	}
	tool.CreatedAt, tool.UpdatedAt = createdAt, updatedAt
	tool.Owner = id.PrincipalID
	return nil
}

func loadTool(ctx context.Context, tx *sql.Tx, tenantID, toolID string) (*ToolDefinition, error) {
	var t ToolDefinition
	var props []byte
	var createdAt, updatedAt string
	err := tx.QueryRowContext(ctx, `
		SELECT id, name, description, version, properties, state, COALESCE(owner_principal_id::text,''), created_at, updated_at
		FROM tool_definitions WHERE id=$1 AND tenant_id=$2
	`, toolID, tenantID).Scan(&t.ID, &t.Name, &t.Description, &t.Version, &props, &t.State, &t.Owner, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(props, &t.Props); err != nil {
		return nil, err
	}
	t.CurrentState = t.State
	t.CreatedAt, t.UpdatedAt = createdAt, updatedAt
	return &t, nil
}

func loadToolByName(ctx context.Context, tx *sql.Tx, tenantID, name string) (*ToolDefinition, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM tool_definitions WHERE tenant_id=$1 AND name=$2`, tenantID, name).Scan(&id)
	if err != nil {
		return nil, err
	}
	return loadTool(ctx, tx, tenantID, id)
}

func listTools(ctx context.Context, db *sql.DB, tenantID string) ([]*ToolDefinition, error) {
	// tool_definitions and operations are under FORCE RLS: read inside a
	// transaction carrying the tenant context.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, description, version, properties, state, COALESCE(owner_principal_id::text,''), created_at, updated_at
		FROM tool_definitions WHERE tenant_id=$1 ORDER BY created_at ASC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tools []*ToolDefinition
	for rows.Next() {
		var t ToolDefinition
		var props []byte
		var createdAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Version, &props, &t.State, &t.Owner, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(props, &t.Props); err != nil {
			return nil, err
		}
		t.CurrentState = t.State
		t.CreatedAt, t.UpdatedAt = createdAt, updatedAt
		tools = append(tools, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return tools, tx.Commit()
}

func transitionTool(ctx context.Context, tx *sql.Tx, tenantID, toolID string, newState ToolState) error {
	res, err := tx.ExecContext(ctx, `UPDATE tool_definitions SET state=$1, updated_at=now() WHERE id=$2 AND tenant_id=$3`, newState, toolID, tenantID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// startOperation inserts an operations row plus its bound tool_executions
// row, recording the tool's declared properties at execution time (per
// database/migrations/000002_governance_tables.sql's tool_executions
// design: a snapshot bound to the operation, independent of later changes
// to the tool catalog) together with the governance decision: the plan,
// the risk the caller declared, the risk governance decided on, and
// whether the caller confirmed the operation. The row also records the
// principal that started it and, when claimed is set (an operation its
// executor prepares for itself), that its executor has already taken it;
// an operation started unclaimed waits for its executor to claim it with
// claimStartedOperation.
func startOperation(ctx context.Context, tx *sql.Tx, id requestIdentity, opID, toolName, toolVersion, idempotencyKey string, input any, declaredProps ToolProperties, decision operationDecision, claimed bool) (*ToolOperation, error) {
	requestBody, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	requestHash := sha256Hex(string(requestBody))
	var startedAt string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO operations (id, tenant_id, tool_name, tool_version, request_hash, idempotency_key, status, request)
		VALUES ($1,$2,$3,$4,$5,$6,'executing',$7)
		RETURNING created_at
	`, opID, id.TenantID, toolName, toolVersion, requestHash, idempotencyKey, requestBody).Scan(&startedAt)
	if err != nil {
		return nil, fmt.Errorf("insert operation: %w", err)
	}
	execID, err := newID()
	if err != nil {
		return nil, err
	}
	declared, err := json.Marshal(declaredProps)
	if err != nil {
		return nil, err
	}
	plan, err := json.Marshal(decision.Plan)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tool_executions (id, operation_id, tool_identity, declared_properties, input_hash, plan, requested_risk_level, risk_level, confirmed, started_by, executor_claimed_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,CASE WHEN $11::boolean THEN now() END)
	`, execID, opID, toolName, declared, requestHash, plan, decision.RequestedRisk, decision.EffectiveRisk, decision.Confirmed, id.PrincipalID, claimed); err != nil {
		return nil, fmt.Errorf("insert tool execution: %w", err)
	}
	planCopy := decision.Plan
	return &ToolOperation{
		ID: opID, ToolID: toolName, State: StateExecuting, Input: input, StartedAt: startedAt,
		Plan: &planCopy, RequestedRisk: decision.RequestedRisk, RiskLevel: decision.EffectiveRisk,
		Confirmed: decision.Confirmed, DeclaredProperties: &declaredProps,
	}, nil
}

// startedOperation is an existing operation as an executor sees it when it
// asks to run under it: the operation with its recorded governance
// decision, the principal that started it, and whether an executor has
// already taken it.
type startedOperation struct {
	*ToolOperation
	StartedBy string
	Claimed   bool
}

// loadStartedOperation reads opID for an executor's claim, locking its
// tool_executions row so concurrent claims of one operation serialise.
// sql.ErrNoRows means no operation by that ID exists in the tenant.
func loadStartedOperation(ctx context.Context, tx *sql.Tx, tenantID, opID string) (*startedOperation, error) {
	op, err := loadOperation(ctx, tx, tenantID, opID)
	if err != nil {
		return nil, err
	}
	started := &startedOperation{ToolOperation: op}
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(started_by,''), executor_claimed_at IS NOT NULL
		FROM tool_executions WHERE operation_id=$1
		FOR UPDATE
	`, opID).Scan(&started.StartedBy, &started.Claimed)
	if err == sql.ErrNoRows {
		// An operation without a governance snapshot was never started
		// through decideOperation; it can be claimed by no executor.
		started.Claimed = true
		return started, nil
	}
	if err != nil {
		return nil, err
	}
	return started, nil
}

// claimStartedOperation hands an unclaimed operation to its executor and
// records the risk the executor's plan was decided at. It reports false
// when another executor claimed the operation first.
func claimStartedOperation(ctx context.Context, tx *sql.Tx, opID, effectiveRisk string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE tool_executions SET executor_claimed_at=now(), risk_level=$2
		WHERE operation_id=$1 AND executor_claimed_at IS NULL
	`, opID, effectiveRisk)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// operationExists reports whether opID already names an operation in the
// tenant, so a duplicate prepare is refused rather than failing on the
// primary key.
func operationExists(ctx context.Context, tx *sql.Tx, tenantID, opID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE id=$1 AND tenant_id=$2)`, opID, tenantID).Scan(&exists)
	return exists, err
}

// loadBuiltinTool reads a fabric built-in tool from the migration-seeded
// builtin_tool_definitions catalog. sql.ErrNoRows means name is not a
// built-in tool.
func loadBuiltinTool(ctx context.Context, tx *sql.Tx, name string) (*ToolDefinition, error) {
	var t ToolDefinition
	var props []byte
	err := tx.QueryRowContext(ctx, `SELECT name, description, version, properties FROM builtin_tool_definitions WHERE name=$1`, name).
		Scan(&t.Name, &t.Description, &t.Version, &props)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(props, &t.Props); err != nil {
		return nil, fmt.Errorf("decode built-in tool %q properties: %w", name, err)
	}
	return &t, nil
}

// materialiseBuiltinTool gives the tenant its own authorised copy of a
// built-in tool, with the catalog's version and declared properties --
// never anything the caller supplied. A concurrent prepare that already
// materialised it is not an error; the tenant's row is returned either way.
func materialiseBuiltinTool(ctx context.Context, tx *sql.Tx, tenantID string, builtin *ToolDefinition) (*ToolDefinition, error) {
	generated, err := newID()
	if err != nil {
		return nil, err
	}
	props, err := json.Marshal(builtin.Props)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tool_definitions (id, tenant_id, name, description, version, properties, state)
		VALUES ($1,$2,$3,$4,$5,$6,'authorised')
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, generated, tenantID, builtin.Name, builtin.Description, builtin.Version, props); err != nil {
		return nil, err
	}
	return loadToolByName(ctx, tx, tenantID, builtin.Name)
}

// operationGovernanceColumns is the tool_executions half of an operation
// read: the declared properties and governance decision recorded when the
// operation was started.
const operationGovernanceColumns = `te.declared_properties, te.plan, COALESCE(te.requested_risk_level,''), COALESCE(te.risk_level,''), COALESCE(te.confirmed,false)`

func applyOperationGovernance(op *ToolOperation, declared, plan []byte, requested, effective string, confirmed bool) error {
	if len(declared) > 0 {
		var props ToolProperties
		if err := json.Unmarshal(declared, &props); err != nil {
			return fmt.Errorf("decode declared properties: %w", err)
		}
		op.DeclaredProperties = &props
	}
	if len(plan) > 0 && string(plan) != "{}" {
		var p OperationPlan
		if err := json.Unmarshal(plan, &p); err != nil {
			return fmt.Errorf("decode plan: %w", err)
		}
		op.Plan = &p
	}
	op.RequestedRisk, op.RiskLevel, op.Confirmed = requested, effective, confirmed
	return nil
}

func loadOperation(ctx context.Context, tx *sql.Tx, tenantID, opID string) (*ToolOperation, error) {
	var op ToolOperation
	var request, response, declared, plan []byte
	var requested, effective string
	var confirmed bool
	var startedAt string
	var completedAt sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT o.id, o.tool_name, o.status, o.request, o.response, o.created_at, o.updated_at, `+operationGovernanceColumns+`
		FROM operations o LEFT JOIN tool_executions te ON te.operation_id = o.id
		WHERE o.id=$1 AND o.tenant_id=$2
	`, opID, tenantID).Scan(&op.ID, &op.ToolID, &op.State, &request, &response, &startedAt, &completedAt, &declared, &plan, &requested, &effective, &confirmed)
	if err != nil {
		return nil, err
	}
	if err := applyOperationGovernance(&op, declared, plan, requested, effective, confirmed); err != nil {
		return nil, err
	}
	if len(request) > 0 {
		_ = json.Unmarshal(request, &op.Input)
	}
	if len(response) > 0 {
		var wrapped operationResponse
		if err := json.Unmarshal(response, &wrapped); err == nil {
			op.Output, op.Error = wrapped.Output, wrapped.Error
		}
	}
	op.StartedAt = startedAt
	if op.State == StateSucceeded || op.State == StateFailed || op.State == StateReconciled || op.State == StateCancelled {
		if completedAt.Valid {
			op.CompletedAt = completedAt.String
		}
	}
	return &op, nil
}

// operationResponse is the shape stored in operations.response, wrapping
// both the tool's output and any error so a single jsonb column can carry
// either outcome.
type operationResponse struct {
	Output any    `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

func listOperations(ctx context.Context, db *sql.DB, tenantID string) ([]*ToolOperation, error) {
	// tool_definitions and operations are under FORCE RLS: read inside a
	// transaction carrying the tenant context.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT o.id, o.tool_name, o.status, o.request, o.response, o.created_at, `+operationGovernanceColumns+`
		FROM operations o LEFT JOIN tool_executions te ON te.operation_id = o.id
		WHERE o.tenant_id=$1 ORDER BY o.created_at ASC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []*ToolOperation
	for rows.Next() {
		var op ToolOperation
		var request, response, declared, plan []byte
		var requested, effective string
		var confirmed bool
		var startedAt string
		if err := rows.Scan(&op.ID, &op.ToolID, &op.State, &request, &response, &startedAt, &declared, &plan, &requested, &effective, &confirmed); err != nil {
			return nil, err
		}
		if err := applyOperationGovernance(&op, declared, plan, requested, effective, confirmed); err != nil {
			return nil, err
		}
		if len(request) > 0 {
			_ = json.Unmarshal(request, &op.Input)
		}
		if len(response) > 0 {
			var wrapped operationResponse
			if err := json.Unmarshal(response, &wrapped); err == nil {
				op.Output, op.Error = wrapped.Output, wrapped.Error
			}
		}
		op.StartedAt = startedAt
		ops = append(ops, &op)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return ops, tx.Commit()
}

// completeOperation moves an executing operation to a terminal (or
// reconciling) state and records its response, using updated_at as the
// completion timestamp since operations has no dedicated completed_at
// column.
func completeOperation(ctx context.Context, tx *sql.Tx, tenantID, opID string, newState ToolState, output any, errMsg string) error {
	response, err := json.Marshal(map[string]any{"output": output, "error": errMsg})
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE operations SET status=$1, response=$2, updated_at=now()
		WHERE id=$3 AND tenant_id=$4
	`, newState, response, opID, tenantID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
