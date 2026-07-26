package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type journalStore struct{ db *sql.DB }

type requestContext struct {
	TenantID    string
	PrincipalID string
}

type createSessionRequest struct {
	NamespaceID string `json:"namespace_id"`
}

type createSessionResponse struct {
	ID string `json:"id"`
}

type appendMessageRequest struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type appendMessageResponse struct {
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Sequence  int64  `json:"sequence"`
}

type completeTurnRequest struct {
	Content string         `json:"content"`
	State   map[string]any `json:"state"`
}

type completeTurnResponse struct {
	TurnID       string `json:"turn_id"`
	MessageID    string `json:"message_id"`
	CheckpointID string `json:"checkpoint_id"`
}

type prepareOperationRequest struct {
	ToolName             string         `json:"tool_name"`
	ToolVersion          string         `json:"tool_version"`
	Input                map[string]any `json:"input"`
	ExpectedEffects      map[string]any `json:"expected_effects"`
	RetryPolicy          map[string]any `json:"retry_policy"`
	ReconciliationMethod string         `json:"reconciliation_method"`
}

type prepareOperationResponse struct {
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
}

func (s journalStore) prepareOperation(ctx context.Context, rc requestContext, idempotencyKey string, req prepareOperationRequest) (prepareOperationResponse, error) {
	if idempotencyKey == "" {
		return prepareOperationResponse{}, fmt.Errorf("Idempotency-Key is required")
	}
	if strings.TrimSpace(req.ToolName) == "" || strings.TrimSpace(req.ToolVersion) == "" {
		return prepareOperationResponse{}, fmt.Errorf("tool_name and tool_version are required")
	}
	requestJSON, err := json.Marshal(req.Input)
	if err != nil {
		return prepareOperationResponse{}, err
	}
	requestHash := fmt.Sprintf("%x", sha256Bytes(requestJSON))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return prepareOperationResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return prepareOperationResponse{}, err
	}
	var oldHash, oldResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`, rc.TenantID, rc.PrincipalID, idempotencyKey).Scan(&oldHash, &oldResponse)
	if err == nil {
		if oldHash != requestHash {
			return prepareOperationResponse{}, fmt.Errorf("Idempotency-Key was already used with different input")
		}
		var result prepareOperationResponse
		if json.Unmarshal([]byte(oldResponse), &result) != nil {
			return prepareOperationResponse{}, fmt.Errorf("stored idempotency result is invalid")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return prepareOperationResponse{}, err
	}
	operationID, err := newID()
	if err != nil {
		return prepareOperationResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations (id,tenant_id,tool_name,tool_version,request_hash,idempotency_key,status,request) VALUES ($1,$2,$3,$4,$5,$6,'prepared',$7)`, operationID, rc.TenantID, req.ToolName, req.ToolVersion, requestHash, idempotencyKey, requestJSON); err != nil {
		return prepareOperationResponse{}, err
	}
	props, err := json.Marshal(map[string]any{"read_only": false, "externally_side_effecting": true, "reconcilable": req.ReconciliationMethod != ""})
	if err != nil {
		return prepareOperationResponse{}, err
	}
	expected, err := json.Marshal(req.ExpectedEffects)
	if err != nil {
		return prepareOperationResponse{}, err
	}
	retry, err := json.Marshal(req.RetryPolicy)
	if err != nil {
		return prepareOperationResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tool_executions (id,operation_id,tool_identity,declared_properties,input_hash,expected_effects,retry_policy,reconciliation_method) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, operationID, operationID, req.ToolName+"@"+req.ToolVersion, props, requestHash, expected, retry, req.ReconciliationMethod); err != nil {
		return prepareOperationResponse{}, err
	}
	if err = appendEvent(ctx, tx, rc.TenantID, operationID, "operation.prepared", "operation/"+operationID, rc.PrincipalID, map[string]any{"tool_name": req.ToolName, "tool_version": req.ToolVersion}); err != nil {
		return prepareOperationResponse{}, err
	}
	result := prepareOperationResponse{OperationID: operationID, Status: "prepared"}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return prepareOperationResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id,principal_id,key,operation_type,request_hash,status,response_reference,expires_at) VALUES ($1,$2,$3,'prepare_operation',$4,'succeeded',$5,now()+interval '24 hours')`, rc.TenantID, rc.PrincipalID, idempotencyKey, requestHash, string(resultJSON)); err != nil {
		return prepareOperationResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return prepareOperationResponse{}, err
	}
	return result, nil
}

func setPrincipalContext(ctx context.Context, tx *sql.Tx, rc requestContext) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, rc.TenantID, rc.PrincipalID)
	return err
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

func (s journalStore) createSession(ctx context.Context, rc requestContext, idempotencyKey string, req createSessionRequest) (createSessionResponse, error) {
	if idempotencyKey == "" {
		return createSessionResponse{}, fmt.Errorf("Idempotency-Key is required")
	}
	if strings.TrimSpace(req.NamespaceID) == "" {
		return createSessionResponse{}, fmt.Errorf("namespace_id is required")
	}
	requestHash := fmt.Sprintf("%x", sha256Bytes([]byte(req.NamespaceID)))
	id, err := newID()
	if err != nil {
		return createSessionResponse{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return createSessionResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return createSessionResponse{}, err
	}
	var previousHash, previousResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`, rc.TenantID, rc.PrincipalID, idempotencyKey).Scan(&previousHash, &previousResponse)
	if err == nil {
		if previousHash != requestHash {
			return createSessionResponse{}, fmt.Errorf("Idempotency-Key was already used with different input")
		}
		var result createSessionResponse
		if json.Unmarshal([]byte(previousResponse), &result) != nil {
			return createSessionResponse{}, fmt.Errorf("stored idempotency result is invalid")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return createSessionResponse{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions (id, tenant_id, namespace_id, created_by) VALUES ($1,$2,$3,$4)`, id, rc.TenantID, req.NamespaceID, rc.PrincipalID)
	if err != nil {
		return createSessionResponse{}, err
	}
	if err = appendEvent(ctx, tx, rc.TenantID, id, "session.created", "session/"+id, rc.PrincipalID, map[string]string{"namespace_id": req.NamespaceID}); err != nil {
		return createSessionResponse{}, err
	}
	result := createSessionResponse{ID: id}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return createSessionResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id,principal_id,key,operation_type,request_hash,status,response_reference,expires_at) VALUES ($1,$2,$3,'create_session',$4,'succeeded',$5,now()+interval '24 hours')`, rc.TenantID, rc.PrincipalID, idempotencyKey, requestHash, string(resultJSON)); err != nil {
		return createSessionResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return createSessionResponse{}, err
	}
	return result, nil
}

func (s journalStore) appendMessage(ctx context.Context, rc requestContext, sessionID, idempotencyKey string, req appendMessageRequest) (appendMessageResponse, error) {
	if idempotencyKey == "" {
		return appendMessageResponse{}, fmt.Errorf("Idempotency-Key is required")
	}
	if req.Role != "user" && req.Role != "assistant" && req.Role != "tool" {
		return appendMessageResponse{}, fmt.Errorf("unsupported role")
	}
	if strings.TrimSpace(req.Content) == "" {
		return appendMessageResponse{}, fmt.Errorf("content is required")
	}
	turnID, err := newID()
	if err != nil {
		return appendMessageResponse{}, err
	}
	messageID, err := newID()
	if err != nil {
		return appendMessageResponse{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return appendMessageResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return appendMessageResponse{}, err
	}
	hash := fmt.Sprintf("%x", sha256Bytes([]byte(req.Role+"\x00"+req.Content)))
	var previousHash, previousResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`, rc.TenantID, rc.PrincipalID, idempotencyKey).Scan(&previousHash, &previousResponse)
	if err == nil {
		if previousHash != hash {
			return appendMessageResponse{}, fmt.Errorf("Idempotency-Key was already used with different input")
		}
		var result appendMessageResponse
		if json.Unmarshal([]byte(previousResponse), &result) != nil {
			return appendMessageResponse{}, fmt.Errorf("stored idempotency result is invalid")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return appendMessageResponse{}, err
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `UPDATE sessions SET last_sequence=last_sequence+1, updated_at=now() WHERE id=$1 AND tenant_id=$2 RETURNING last_sequence`, sessionID, rc.TenantID).Scan(&sequence); err != nil {
		return appendMessageResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO turns (id, session_id, sequence, actor_id) VALUES ($1,$2,$3,$4)`, turnID, sessionID, sequence, rc.PrincipalID); err != nil {
		return appendMessageResponse{}, err
	}
	contentHash := fmt.Sprintf("%x", sha256Bytes([]byte(req.Content)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id,tenant_id,session_id,turn_id,role,content,content_hash) VALUES ($1,$2,$3,$4,$5,$6,$7)`, messageID, rc.TenantID, sessionID, turnID, req.Role, req.Content, contentHash); err != nil {
		return appendMessageResponse{}, err
	}
	if err = appendEvent(ctx, tx, rc.TenantID, turnID, "turn.message_accepted", "turn/"+turnID, rc.PrincipalID, map[string]any{"session_id": sessionID, "message_id": messageID, "role": req.Role, "sequence": sequence}); err != nil {
		return appendMessageResponse{}, err
	}
	jobPayload, err := json.Marshal(map[string]string{"turn_id": turnID, "message_id": messageID})
	if err != nil {
		return appendMessageResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbox_jobs (tenant_id,operation_id,job_type,payload) VALUES ($1,$2,$3,$4)`, rc.TenantID, turnID, "turn.accepted", jobPayload); err != nil {
		return appendMessageResponse{}, err
	}
	result := appendMessageResponse{TurnID: turnID, MessageID: messageID, Sequence: sequence}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return appendMessageResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id,principal_id,key,operation_type,request_hash,status,response_reference,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now()+interval '24 hours')`, rc.TenantID, rc.PrincipalID, idempotencyKey, "append_message", hash, "succeeded", string(resultJSON)); err != nil {
		return appendMessageResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return appendMessageResponse{}, err
	}
	return result, nil
}

func (s journalStore) completeTurn(ctx context.Context, rc requestContext, turnID, idempotencyKey string, req completeTurnRequest) (completeTurnResponse, error) {
	if idempotencyKey == "" {
		return completeTurnResponse{}, fmt.Errorf("Idempotency-Key is required")
	}
	if strings.TrimSpace(req.Content) == "" {
		return completeTurnResponse{}, fmt.Errorf("content is required")
	}
	state, err := json.Marshal(req.State)
	if err != nil {
		return completeTurnResponse{}, err
	}
	hash := fmt.Sprintf("%x", sha256Bytes(append(append([]byte(req.Content), 0), state...)))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return completeTurnResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return completeTurnResponse{}, err
	}
	var previousHash, previousResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`, rc.TenantID, rc.PrincipalID, idempotencyKey).Scan(&previousHash, &previousResponse)
	if err == nil {
		if previousHash != hash {
			return completeTurnResponse{}, fmt.Errorf("Idempotency-Key was already used with different input")
		}
		var result completeTurnResponse
		if json.Unmarshal([]byte(previousResponse), &result) != nil {
			return completeTurnResponse{}, fmt.Errorf("stored idempotency result is invalid")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return completeTurnResponse{}, err
	}
	messageID, err := newID()
	if err != nil {
		return completeTurnResponse{}, err
	}
	checkpointID, err := newID()
	if err != nil {
		return completeTurnResponse{}, err
	}
	var sessionID string
	err = tx.QueryRowContext(ctx, `SELECT session_id FROM turns WHERE id=$1 AND actor_id=$2 AND status IN ('accepted','running','interrupted') FOR UPDATE`, turnID, rc.PrincipalID).Scan(&sessionID)
	if err != nil {
		return completeTurnResponse{}, err
	}
	contentHash := fmt.Sprintf("%x", sha256Bytes([]byte(req.Content)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id,tenant_id,session_id,turn_id,role,content,content_hash) VALUES ($1,$2,$3,$4,'assistant',$5,$6)`, messageID, rc.TenantID, sessionID, turnID, req.Content, contentHash); err != nil {
		return completeTurnResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE turns SET status='completed', completed_at=now() WHERE id=$1`, turnID); err != nil {
		return completeTurnResponse{}, err
	}
	stateHash := fmt.Sprintf("%x", sha256Bytes(state))
	if err = appendEvent(ctx, tx, rc.TenantID, turnID, "turn.completed", "turn/"+turnID, rc.PrincipalID, map[string]any{"session_id": sessionID, "message_id": messageID, "checkpoint_id": checkpointID}); err != nil {
		return completeTurnResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO checkpoints (id,tenant_id,session_id,turn_id,last_event_sequence,state,content_hash) VALUES ($1,$2,$3,$4,(SELECT last_sequence FROM aggregate_sequences WHERE aggregate_id=$4),$5,$6)`, checkpointID, rc.TenantID, sessionID, turnID, state, stateHash); err != nil {
		return completeTurnResponse{}, err
	}
	jobPayload, err := json.Marshal(map[string]string{"turn_id": turnID, "message_id": messageID, "checkpoint_id": checkpointID})
	if err != nil {
		return completeTurnResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbox_jobs (tenant_id,operation_id,job_type,payload) VALUES ($1,$2,'turn.completed',$3)`, rc.TenantID, turnID+":completed", jobPayload); err != nil {
		return completeTurnResponse{}, err
	}
	result := completeTurnResponse{TurnID: turnID, MessageID: messageID, CheckpointID: checkpointID}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return completeTurnResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id,principal_id,key,operation_type,request_hash,status,response_reference,expires_at) VALUES ($1,$2,$3,'complete_turn',$4,'succeeded',$5,now()+interval '24 hours')`, rc.TenantID, rc.PrincipalID, idempotencyKey, hash, string(resultJSON)); err != nil {
		return completeTurnResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return completeTurnResponse{}, err
	}
	return result, nil
}

func appendEvent(ctx context.Context, tx *sql.Tx, tenantID, aggregateID, eventType, subject, actorID string, data any) error {
	id, err := newID()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var sequence int64
	err = tx.QueryRowContext(ctx, `INSERT INTO aggregate_sequences (aggregate_id,last_sequence) VALUES ($1,1) ON CONFLICT (aggregate_id) DO UPDATE SET last_sequence=aggregate_sequences.last_sequence+1, updated_at=now() RETURNING last_sequence`, aggregateID).Scan(&sequence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events (id,tenant_id,aggregate_id,aggregate_sequence,source,event_type,subject,occurred_at,data_schema,actor_id,data) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, tenantID, aggregateID, sequence, "scos.memory-gateway", eventType, subject, time.Now().UTC(), "https://schemas.scos.local/events/event-envelope.schema.json", actorID, payload)
	return err
}

func sha256Bytes(value []byte) []byte { sum := sha256.Sum256(value); return sum[:] }
