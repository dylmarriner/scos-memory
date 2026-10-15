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

type journalStore struct {
	db        *sql.DB
	validator *eventValidator
}

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

type appendChunkRequest struct {
	MessageID     string `json:"message_id"`
	ChunkSequence int    `json:"chunk_sequence"`
	Content       string `json:"content"`
}

type appendChunkResponse struct {
	TurnID        string `json:"turn_id"`
	MessageID     string `json:"message_id"`
	ChunkSequence int    `json:"chunk_sequence"`
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

type startResponseResponse struct {
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type turnRecord struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id"`
	Sequence    int64      `json:"sequence"`
	Status      string     `json:"status"`
	ActorID     string     `json:"actor_id"`
	AgentID     *string    `json:"agent_id"`
	AcceptedAt  time.Time  `json:"accepted_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type listTurnsResponse struct {
	Turns []turnRecord `json:"turns"`
	Limit int          `json:"limit"`
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

func (s journalStore) listTurns(ctx context.Context, rc requestContext, limit int) (listTurnsResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return listTurnsResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return listTurnsResponse{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.session_id, t.sequence, t.status, t.actor_id, t.agent_id,
		       t.accepted_at, t.started_at, t.completed_at
		FROM turns t
		JOIN sessions s ON s.id = t.session_id
		WHERE s.tenant_id = $1
		ORDER BY t.accepted_at DESC, t.id DESC
		LIMIT $2`, rc.TenantID, limit)
	if err != nil {
		return listTurnsResponse{}, err
	}
	defer rows.Close()

	result := listTurnsResponse{Turns: make([]turnRecord, 0), Limit: limit}
	for rows.Next() {
		var record turnRecord
		var agentID sql.NullString
		var startedAt, completedAt sql.NullTime
		if err = rows.Scan(&record.ID, &record.SessionID, &record.Sequence, &record.Status, &record.ActorID, &agentID, &record.AcceptedAt, &startedAt, &completedAt); err != nil {
			return listTurnsResponse{}, err
		}
		if agentID.Valid {
			record.AgentID = &agentID.String
		}
		if startedAt.Valid {
			record.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			record.CompletedAt = &completedAt.Time
		}
		result.Turns = append(result.Turns, record)
	}
	if err = rows.Err(); err != nil {
		return listTurnsResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return listTurnsResponse{}, err
	}
	return result, nil
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
	if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, operationID, "operation.prepared", "operation/"+operationID, rc.PrincipalID, map[string]any{"tool_name": req.ToolName, "tool_version": req.ToolVersion}); err != nil {
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
	if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, id, "session.created", "session/"+id, rc.PrincipalID, map[string]string{"namespace_id": req.NamespaceID}); err != nil {
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
	if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, turnID, "turn.message_accepted", "turn/"+turnID, rc.PrincipalID, map[string]any{"session_id": sessionID, "message_id": messageID, "role": req.Role, "sequence": sequence}); err != nil {
		return appendMessageResponse{}, err
	}
	// principal_id lets workers act as the turn's author under RLS.
	jobPayload, err := json.Marshal(map[string]string{"turn_id": turnID, "message_id": messageID, "session_id": sessionID, "principal_id": rc.PrincipalID})
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

func (s journalStore) appendChunk(ctx context.Context, rc requestContext, turnID, idempotencyKey string, req appendChunkRequest) (appendChunkResponse, error) {
	if idempotencyKey == "" {
		return appendChunkResponse{}, fmt.Errorf("Idempotency-Key is required")
	}
	if strings.TrimSpace(req.MessageID) == "" || req.ChunkSequence < 0 || strings.TrimSpace(req.Content) == "" {
		return appendChunkResponse{}, fmt.Errorf("message_id, non-negative chunk_sequence and content are required")
	}
	requestJSON, err := json.Marshal(req)
	if err != nil {
		return appendChunkResponse{}, err
	}
	hash := fmt.Sprintf("%x", sha256Bytes(requestJSON))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return appendChunkResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return appendChunkResponse{}, err
	}
	var previousHash, previousResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`, rc.TenantID, rc.PrincipalID, idempotencyKey).Scan(&previousHash, &previousResponse)
	if err == nil {
		if previousHash != hash {
			return appendChunkResponse{}, fmt.Errorf("Idempotency-Key was already used with different input")
		}
		var result appendChunkResponse
		if json.Unmarshal([]byte(previousResponse), &result) != nil {
			return appendChunkResponse{}, fmt.Errorf("stored idempotency result is invalid")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return appendChunkResponse{}, err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages m JOIN turns t ON t.id=m.turn_id WHERE m.id=$1 AND m.turn_id=$2 AND m.tenant_id=$3 AND m.role='assistant' AND t.actor_id=$4 AND t.status IN ('accepted','running','interrupted'))`, req.MessageID, turnID, rc.TenantID, rc.PrincipalID).Scan(&exists); err != nil {
		return appendChunkResponse{}, err
	}
	if !exists {
		return appendChunkResponse{}, fmt.Errorf("assistant message or active turn not found")
	}
	chunkHash := fmt.Sprintf("%x", sha256Bytes([]byte(req.Content)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_chunks (message_id,chunk_sequence,content,content_hash) VALUES ($1,$2,$3,$4)`, req.MessageID, req.ChunkSequence, req.Content, chunkHash); err != nil {
		return appendChunkResponse{}, err
	}
	if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, turnID, "assistant_response_chunk_committed", "turn/"+turnID, rc.PrincipalID, map[string]any{"message_id": req.MessageID, "chunk_sequence": req.ChunkSequence}); err != nil {
		return appendChunkResponse{}, err
	}
	result := appendChunkResponse{TurnID: turnID, MessageID: req.MessageID, ChunkSequence: req.ChunkSequence}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return appendChunkResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id,principal_id,key,operation_type,request_hash,status,response_reference,expires_at) VALUES ($1,$2,$3,'append_chunk',$4,'succeeded',$5,now()+interval '24 hours')`, rc.TenantID, rc.PrincipalID, idempotencyKey, hash, string(resultJSON)); err != nil {
		return appendChunkResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return appendChunkResponse{}, err
	}
	return result, nil
}

func (s journalStore) startResponse(ctx context.Context, rc requestContext, turnID, idempotencyKey string) (startResponseResponse, error) {
	if idempotencyKey == "" {
		return startResponseResponse{}, fmt.Errorf("Idempotency-Key is required")
	}
	hash := fmt.Sprintf("%x", sha256Bytes([]byte(turnID)))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return startResponseResponse{}, err
	}
	defer tx.Rollback()
	if err = setPrincipalContext(ctx, tx, rc); err != nil {
		return startResponseResponse{}, err
	}
	var previousHash, previousResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`, rc.TenantID, rc.PrincipalID, idempotencyKey).Scan(&previousHash, &previousResponse)
	if err == nil {
		if previousHash != hash {
			return startResponseResponse{}, fmt.Errorf("Idempotency-Key was already used with different input")
		}
		var result startResponseResponse
		if json.Unmarshal([]byte(previousResponse), &result) != nil {
			return startResponseResponse{}, fmt.Errorf("stored idempotency result is invalid")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return startResponseResponse{}, err
	}
	var sessionID string
	if err = tx.QueryRowContext(ctx, `SELECT session_id FROM turns WHERE id=$1 AND actor_id=$2 AND status IN ('accepted','running','interrupted') FOR UPDATE`, turnID, rc.PrincipalID).Scan(&sessionID); err != nil {
		return startResponseResponse{}, err
	}
	var messageID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE turn_id=$1 AND tenant_id=$2 AND role='assistant' ORDER BY committed_at DESC LIMIT 1`, turnID, rc.TenantID).Scan(&messageID)
	if err == sql.ErrNoRows {
		messageID, err = newID()
		if err != nil {
			return startResponseResponse{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id,tenant_id,session_id,turn_id,role,content,content_hash) VALUES ($1,$2,$3,$4,'assistant','',$5)`, messageID, rc.TenantID, sessionID, turnID, fmt.Sprintf("%x", sha256Bytes(nil))); err != nil {
			return startResponseResponse{}, err
		}
	} else if err != nil {
		return startResponseResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE turns SET status='running', started_at=COALESCE(started_at,now()) WHERE id=$1`, turnID); err != nil {
		return startResponseResponse{}, err
	}
	if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, turnID, "assistant_response_started", "turn/"+turnID, rc.PrincipalID, map[string]string{"message_id": messageID}); err != nil {
		return startResponseResponse{}, err
	}
	result := startResponseResponse{TurnID: turnID, MessageID: messageID, Status: "running"}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return startResponseResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id,principal_id,key,operation_type,request_hash,status,response_reference,expires_at) VALUES ($1,$2,$3,'start_response',$4,'succeeded',$5,now()+interval '24 hours')`, rc.TenantID, rc.PrincipalID, idempotencyKey, hash, string(resultJSON)); err != nil {
		return startResponseResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return startResponseResponse{}, err
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
	var existingMessageID string
	var existingFinalized sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id, finalized_at FROM messages WHERE turn_id=$1 AND tenant_id=$2 AND role='assistant' ORDER BY committed_at DESC LIMIT 1`, turnID, rc.TenantID).Scan(&existingMessageID, &existingFinalized)
	contentHash := fmt.Sprintf("%x", sha256Bytes([]byte(req.Content)))
	switch {
	case err == sql.ErrNoRows:
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id,tenant_id,session_id,turn_id,role,content,content_hash,finalized_at) VALUES ($1,$2,$3,$4,'assistant',$5,$6,now())`, messageID, rc.TenantID, sessionID, turnID, req.Content, contentHash); err != nil {
			return completeTurnResponse{}, err
		}
	case err != nil:
		return completeTurnResponse{}, err
	case !existingFinalized.Valid:
		// The existing row is still the unfinalized start_response draft row:
		// this is the draft-to-final transition, not a mutation of
		// settled history, so it is finalized in place.
		messageID = existingMessageID
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET content=$1, content_hash=$2, finalized_at=now() WHERE id=$3`, req.Content, contentHash, messageID); err != nil {
			return completeTurnResponse{}, err
		}
	default:
		// The existing row was already finalized (e.g. a turn re-opened for
		// retry after a crash). Never overwrite settled content: append a new
		// superseding message row instead.
		messageID, err = newID()
		if err != nil {
			return completeTurnResponse{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id,tenant_id,session_id,turn_id,role,content,content_hash,finalized_at,supersedes_message_id) VALUES ($1,$2,$3,$4,'assistant',$5,$6,now(),$7)`, messageID, rc.TenantID, sessionID, turnID, req.Content, contentHash, existingMessageID); err != nil {
			return completeTurnResponse{}, err
		}
		if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, turnID, "message.superseded", "message/"+existingMessageID, rc.PrincipalID, map[string]string{"superseded_by": messageID}); err != nil {
			return completeTurnResponse{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE turns SET status='completed', completed_at=now() WHERE id=$1`, turnID); err != nil {
		return completeTurnResponse{}, err
	}
	stateHash := fmt.Sprintf("%x", sha256Bytes(state))
	if _, err = appendEvent(ctx, tx, s.validator, rc.TenantID, turnID, "turn.completed", "turn/"+turnID, rc.PrincipalID, map[string]any{"session_id": sessionID, "message_id": messageID, "checkpoint_id": checkpointID}); err != nil {
		return completeTurnResponse{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO checkpoints (id,tenant_id,session_id,turn_id,last_event_sequence,state,content_hash) VALUES ($1,$2,$3,$4,(SELECT last_sequence FROM aggregate_sequences WHERE aggregate_id=$4),$5,$6)`, checkpointID, rc.TenantID, sessionID, turnID, state, stateHash); err != nil {
		return completeTurnResponse{}, err
	}
	jobPayload, err := json.Marshal(map[string]string{"turn_id": turnID, "message_id": messageID, "checkpoint_id": checkpointID, "session_id": sessionID, "principal_id": rc.PrincipalID})
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

// appendEvent persists a new event for aggregateID within tx and returns the
// event's own ID so callers can thread it as the causationID of a
// subsequently-appended, causally-dependent event.
//
// Every event carries a correlation ID (the aggregate it belongs to, so all
// events for a turn/session/operation can be threaded together) and a
// causation ID (the immediately preceding event on the same aggregate, if
// one exists). Genuine root events - the first event ever recorded for an
// aggregate - legitimately have no upstream cause, so causation ID is left
// null only in that case, per GOVERNANCE.md section 12.
func appendEvent(ctx context.Context, tx *sql.Tx, validator *eventValidator, tenantID, aggregateID, eventType, subject, actorID string, data any) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	var dataObject map[string]any
	if err := json.Unmarshal(payload, &dataObject); err != nil || dataObject == nil {
		return "", fmt.Errorf("event data must be a JSON object")
	}
	var causationID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM events WHERE aggregate_id=$1 ORDER BY aggregate_sequence DESC LIMIT 1`, aggregateID).Scan(&causationID); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	envelope := map[string]any{
		"specversion":     "1.0",
		"id":              id,
		"source":          "scos.memory-gateway",
		"type":            "scos." + eventType + ".v1",
		"subject":         subject,
		"time":            time.Now().UTC().Format(time.RFC3339Nano),
		"datacontenttype": "application/json",
		"dataschema":      eventEnvelopeSchemaURI,
		"tenantid":        tenantID,
		"correlationid":   aggregateID,
		"causationid":     nullableString(causationID),
		"actorid":         actorID,
		"data":            dataObject,
	}
	if validator == nil {
		validator, err = newEventValidator("")
		if err != nil {
			return "", err
		}
	}
	if err := validator.validate(envelope); err != nil {
		return "", err
	}
	var sequence int64
	err = tx.QueryRowContext(ctx, `INSERT INTO aggregate_sequences (aggregate_id,last_sequence) VALUES ($1,1) ON CONFLICT (aggregate_id) DO UPDATE SET last_sequence=aggregate_sequences.last_sequence+1, updated_at=now() RETURNING last_sequence`, aggregateID).Scan(&sequence)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events (id,tenant_id,aggregate_id,aggregate_sequence,source,event_type,subject,occurred_at,data_schema,correlation_id,causation_id,actor_id,data) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, tenantID, aggregateID, sequence, "scos.memory-gateway", eventType, subject, time.Now().UTC(), "https://schemas.scos.local/events/event-envelope.schema.json", aggregateID, causationID, actorID, payload)
	if err != nil {
		return "", err
	}
	return id, nil
}

func nullableString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func sha256Bytes(value []byte) []byte { sum := sha256.Sum256(value); return sum[:] }
