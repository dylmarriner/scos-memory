package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/database"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxStoredOutputBytes caps one stored stage output. A larger output is not
// stored at all, never truncated: recovery re-runs its stage instead.
const maxStoredOutputBytes = 64 << 10

// recoveryClaim is the result of trying to claim a run for recovery.
type recoveryClaim struct {
	State    string
	Claimed  bool
	Owner    string
	Attempts int
}

// progress is one durable progress write.
type progress struct {
	state, recoveryStatus, failureDetail string
	// release drops the recovery lease (the recovery has finished).
	release bool
}

// has reports whether the output system's completed stage produced is
// available, so the stage need not run again on recovery.
func (o stageOutputs) has(system string) bool {
	switch system {
	case "memory-gateway":
		return o.Memory != ""
	case "code-intelligence":
		return o.Code != ""
	case "context-optimisation":
		return o.Compiled != ""
	case "agent-router":
		return o.Agent != nil
	}
	return true
}

// inTenantTx runs fn in a transaction scoped to the tenant and principal, as
// the row-level security policies require.
func (c *controlPlane) inTenantTx(ctx context.Context, tenantID, actorID string, fn func(pgx.Tx) error) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, actorID); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// leaseFence is the SQL condition under which the writer holding $owner may
// write a run: no lease, its own lease, or an expired one.
const leaseFence = `(recovery_lease_owner IS NULL OR recovery_lease_owner = @owner OR recovery_lease_expires_at < now())`

// saveProgress durably records the run's state, per-stage status and turn
// durability state. Stage outputs are written separately by saveOutput, once
// each. A recovery owner's write renews its lease; a write from anyone else
// while a live lease is held fails with errLeaseLost.
func (c *controlPlane) saveProgress(ctx context.Context, req RouteRequest, lifecycle lifecycleResponse, owner string, p progress) error {
	stages := append([]stageState(nil), lifecycle.Stages...)
	if c.store != nil {
		if err := c.store.fencedUpdate(req.TenantID, req.ActorID, req.InteractionID, owner, p.release, func(s *lifecycleState) {
			s.State, s.IngressStatus, s.Stages = p.state, "committed", stages
			s.Reply, s.Checkpoint, s.Outbox = lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox
			s.FailureDetail = p.failureDetail
			if p.recoveryStatus != "" {
				s.RecoveryStatus = p.recoveryStatus
			}
			if s.Request == nil {
				request := req
				s.Request = &request
			}
		}); err != nil {
			return err
		}
	}
	if c.db == nil {
		return nil
	}
	record := map[string]any{
		"state": p.state, "ingress_status": "committed", "stages": stages,
		"reply": lifecycle.Reply, "checkpoint": lifecycle.Checkpoint, "outbox": lifecycle.Outbox,
		"failure_detail": p.failureDetail, "updated_at": time.Now().UTC(),
	}
	if p.recoveryStatus != "" {
		record["recovery_status"] = p.recoveryStatus
	}
	checkpoint, err := json.Marshal(record)
	if err != nil {
		return err
	}
	current := "unknown"
	for _, stage := range stages {
		current = stage.System
		if stage.Status != "completed" {
			break
		}
	}
	return c.inTenantTx(ctx, req.TenantID, req.ActorID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_runs SET status=@state, current_stage=@stage, checkpoint=checkpoint || @checkpoint::jsonb,
			recovery_lease_owner = CASE WHEN @release THEN NULL WHEN @owner <> '' THEN @owner ELSE recovery_lease_owner END,
			recovery_lease_expires_at = CASE WHEN @release THEN NULL WHEN @owner <> '' THEN now() + make_interval(secs => @ttl) ELSE recovery_lease_expires_at END
			WHERE tenant_id=@tenant AND interaction_id=@interaction AND `+leaseFence,
			pgx.NamedArgs{"state": p.state, "stage": current, "checkpoint": checkpoint, "release": p.release, "owner": owner,
				"ttl": recoveryLeaseTTL.Seconds(), "tenant": req.TenantID, "interaction": req.InteractionID})
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLeaseLost
		}
		return nil
	})
}

// saveOutput stores the output system's stage just produced, if it is one a
// later stage consumes and it fits within maxStoredOutputBytes.
func (c *controlPlane) saveOutput(ctx context.Context, req RouteRequest, owner, system string, outputs stageOutputs) error {
	var key string
	var value any
	switch system {
	case "memory-gateway":
		key, value = "memory", outputs.Memory
	case "code-intelligence":
		key, value = "code", outputs.Code
	case "context-optimisation":
		key, value = "compiled_context", outputs.Compiled
	case "agent-router":
		key, value = "agent_reply", outputs.Agent
	default:
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > maxStoredOutputBytes {
		return nil
	}
	if c.store != nil {
		if err := c.store.fencedUpdate(req.TenantID, req.ActorID, req.InteractionID, owner, false, func(s *lifecycleState) {
			switch key {
			case "memory":
				s.Outputs.Memory = outputs.Memory
			case "code":
				s.Outputs.Code = outputs.Code
			case "compiled_context":
				s.Outputs.Compiled = outputs.Compiled
			case "agent_reply":
				s.Outputs.Agent = outputs.Agent
			}
		}); err != nil {
			return err
		}
	}
	if c.db == nil {
		return nil
	}
	return c.inTenantTx(ctx, req.TenantID, req.ActorID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_runs SET checkpoint = jsonb_set(
				CASE WHEN jsonb_typeof(checkpoint->'outputs') = 'object' THEN checkpoint ELSE checkpoint || '{"outputs":{}}'::jsonb END,
				ARRAY['outputs', @key::text], @value::jsonb)
			WHERE tenant_id=@tenant AND interaction_id=@interaction AND `+leaseFence,
			pgx.NamedArgs{"key": key, "value": encoded, "owner": owner, "tenant": req.TenantID, "interaction": req.InteractionID})
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLeaseLost
		}
		return nil
	})
}

// runState loads the durable state of a run.
func (c *controlPlane) runState(ctx context.Context, tenantID, actorID, interactionID string) (lifecycleState, error) {
	if c.db == nil {
		return c.store.runState(tenantID, actorID, interactionID)
	}
	var state lifecycleState
	err := c.inTenantTx(ctx, tenantID, actorID, func(tx pgx.Tx) error {
		var status, owner string
		var checkpoint []byte
		var expires *time.Time
		if err := tx.QueryRow(ctx, `SELECT status, checkpoint, COALESCE(recovery_lease_owner,''), recovery_lease_expires_at FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`,
			tenantID, interactionID).Scan(&status, &checkpoint, &owner, &expires); err != nil {
			return err
		}
		if len(checkpoint) > 0 {
			if err := json.Unmarshal(checkpoint, &state); err != nil {
				return err
			}
		}
		state.State, state.LeaseOwner, state.LeaseExpiresAt = status, owner, expires
		return nil
	})
	return state, err
}

// claimRunSQL claims one run for recovery (see beginRecovery). It runs under
// the tenant's row-level security context, so it can only match that
// tenant's runs.
const claimRunSQL = `UPDATE orchestration_runs SET status='recovering', current_stage='recovery',
				recovery_lease_owner=@owner, recovery_lease_expires_at=now() + make_interval(secs => @ttl),
				checkpoint = checkpoint || jsonb_build_object('state','recovering','recovery_status','started',
					'recovery_attempts', COALESCE((checkpoint->>'recovery_attempts')::int, 0) + 1)
			WHERE tenant_id=@tenant AND interaction_id=@interaction
			  AND (recovery_lease_expires_at IS NULL OR recovery_lease_expires_at < now())
			  AND COALESCE((checkpoint->>'recovery_permanent')::boolean, false) = false
			  AND (status IN ('interrupted','degraded','failed','recovering')
			       OR (status IN ('running','planned') AND COALESCE((checkpoint->>'updated_at')::timestamptz, '-infinity'::timestamptz) < now() - make_interval(secs => @stale)))
			RETURNING (checkpoint->>'recovery_attempts')::int`

// beginRecovery claims a run for recovery by owner: in one conditional write
// it checks the run is claimable (see claimable), takes the lease and counts
// the attempt. A run that is not claimable is reported with Claimed false.
func (c *controlPlane) beginRecovery(ctx context.Context, tenantID, actorID, interactionID, owner string) (recoveryClaim, error) {
	if c == nil || (c.db == nil && c.store == nil) {
		return recoveryClaim{}, errors.New("control-plane persistence is unavailable")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(interactionID) == "" {
		return recoveryClaim{}, errors.New("tenant_id, actor_id and interaction_id are required")
	}
	if c.db == nil {
		return c.store.claim(tenantID, actorID, interactionID, owner)
	}
	claim := recoveryClaim{Owner: owner}
	err := c.inTenantTx(ctx, tenantID, actorID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, claimRunSQL,
			pgx.NamedArgs{"owner": owner, "ttl": recoveryLeaseTTL.Seconds(), "stale": staleRunAfter.Seconds(),
				"tenant": tenantID, "interaction": interactionID}).Scan(&claim.Attempts)
		if err == nil {
			claim.State, claim.Claimed = "recovering", true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, tenantID, interactionID).Scan(&claim.State)
	})
	return claim, err
}

// markRecoveryFailed records a failed recovery attempt and releases its
// lease. A permanent failure is never claimed again.
func (c *controlPlane) markRecoveryFailed(ctx context.Context, tenantID, actorID, interactionID, owner string, permanent bool, reason string) error {
	if c.store != nil {
		if err := c.store.fencedUpdate(tenantID, actorID, interactionID, owner, true, func(s *lifecycleState) {
			s.State, s.RecoveryStatus, s.RecoveryFailureReason = "failed", "failed", reason
			s.RecoveryPermanent = s.RecoveryPermanent || permanent
		}); err != nil {
			return err
		}
	}
	if c.db == nil {
		return nil
	}
	return c.inTenantTx(ctx, tenantID, actorID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_runs SET status='failed', recovery_lease_owner=NULL, recovery_lease_expires_at=NULL,
				checkpoint = checkpoint || jsonb_build_object('state','failed','recovery_status','failed',
					'recovery_failure_reason', @reason::text,
					'recovery_permanent', COALESCE((checkpoint->>'recovery_permanent')::boolean, false) OR @permanent)
			WHERE tenant_id=@tenant AND interaction_id=@interaction AND `+leaseFence,
			pgx.NamedArgs{"reason": reason, "permanent": permanent, "owner": owner, "tenant": tenantID, "interaction": interactionID})
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errLeaseLost
		}
		return nil
	})
}

// recoverRun resumes a run claimed by beginRecovery: it reloads the saved
// request, plan and stage state, re-executes every stage that did not
// complete, and leaves the run completed (recovered) or failed. After
// maxRecoveryAttempts failed attempts the run is failed permanently.
func (c *controlPlane) recoverRun(ctx context.Context, tenantID, actorID, interactionID string, claim recoveryClaim) (string, lifecycleResponse, error) {
	stub := RouteRequest{TenantID: tenantID, ActorID: actorID, InteractionID: interactionID}
	exhausted := claim.Attempts >= maxRecoveryAttempts
	fail := func(reason string, cause error) (string, lifecycleResponse, error) {
		// The attempt never reached runPlan, so its start and finish are
		// reported here.
		tally := &hookTally{}
		_ = c.countedHook(ctx, tally, stub, hooksdk.OnRecovery, map[string]any{"interaction_id": interactionID, "stage": "started"})
		_ = c.countedHook(ctx, tally, stub, hooksdk.OnRecovery, map[string]any{"interaction_id": interactionID, "stage": "finished", "outcome": "failed", "reason": reason})
		recordTriggerCoverage(stub, tally, map[string]string{"status": "failed", "phase": "recovery"})
		recordRecoveryOutcome(stub, false, reason)
		stored := reason + ": " + errorText(cause)
		if exhausted {
			stored = "max_recovery_attempts_exceeded: " + stored
		}
		if err := c.markRecoveryFailed(ctx, tenantID, actorID, interactionID, claim.Owner, exhausted, stored); err != nil {
			cause = errors.Join(cause, err)
		}
		return "failed", lifecycleResponse{Status: "failed", InteractionID: interactionID}, cause
	}
	if claim.Attempts > maxRecoveryAttempts {
		return fail("max_recovery_attempts_exceeded", errors.New("recovery attempt limit reached"))
	}
	decision, err := c.decisionForInteraction(ctx, tenantID, actorID, interactionID)
	if err != nil {
		return fail("decision_unavailable", err)
	}
	saved, err := c.runState(ctx, tenantID, actorID, interactionID)
	if err != nil {
		return fail("state_unavailable", err)
	}
	if saved.Request == nil {
		return fail("no_saved_request", errors.New("the run has no saved request to resume from"))
	}
	req := *saved.Request
	if req.TenantID != tenantID || req.ActorID != actorID || req.InteractionID != interactionID {
		return fail("identity_mismatch", errors.New("saved request identity does not match the recovering principal"))
	}
	saved.LeaseOwner = claim.Owner
	_, lifecycle := c.runPlan(ctx, req, decision, &saved)
	if err := c.persistLifecycleDB(ctx, req, decision, lifecycle, claim.Owner); err != nil {
		if errors.Is(err, errLeaseLost) {
			// Another recoverer owns the run; its outcome is the run's outcome.
			recordRecoveryOutcome(req, false, "lease_lost")
			return "superseded", lifecycle, err
		}
		return fail("persist_failed", err)
	}
	recovered := lifecycle.Status == "completed"
	if !recovered && exhausted {
		if err := c.markRecoveryFailed(ctx, tenantID, actorID, interactionID, claim.Owner, true, "max_recovery_attempts_exceeded: resumed run failed"); err != nil {
			return lifecycle.Status, lifecycle, err
		}
	}
	recordRecoveryOutcome(req, recovered, "")
	return lifecycle.Status, lifecycle, nil
}

// Automatic recovery of degraded or failed runs is limited to transient
// failures. A stage failure is transient when its recorded detail shows an
// outage rather than a decision: an HTTP 5xx, 408 or 429 from a dependency,
// or a network or deadline error. Everything else is treated as permanent:
// HTTP 4xx (policy denial, authentication, invalid request), missing
// configuration, missing input such as a proposed tool, failed context
// invariants and lost leases. Retrying those cannot change the outcome.
var transientStatus = regexp.MustCompile(`returned (5\d\d|408|429)\b`)

var transientErrors = []string{
	"connection refused", "connection reset", "broken pipe", "i/o timeout",
	"context deadline exceeded", "timeout awaiting", "no such host", "unexpected eof", ": eof",
}

func transientFailure(detail string) bool {
	if transientStatus.MatchString(detail) {
		return true
	}
	lower := strings.ToLower(detail)
	for _, marker := range transientErrors {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// transientDegradation reports whether every recorded cause of a run's
// degradation is transient. A run with no recorded cause is not retried.
func transientDegradation(s lifecycleState) bool {
	var causes []string
	for _, stage := range s.Stages {
		if stage.Status == "degraded" || stage.Status == "failed" {
			causes = append(causes, stage.Detail)
		}
	}
	if s.FailureDetail != "" {
		causes = append(causes, s.FailureDetail)
	}
	if s.State == "failed" && s.RecoveryFailureReason != "" {
		causes = append(causes, s.RecoveryFailureReason)
	}
	if len(causes) == 0 {
		return false
	}
	for _, cause := range causes {
		if !transientFailure(cause) {
			return false
		}
	}
	return true
}

// autoRecoverable reports whether the sweeper may recover a run on its own.
func autoRecoverable(s lifecycleState, now time.Time) bool {
	if !claimable(s, now) {
		return false
	}
	if s.State == "degraded" || s.State == "failed" {
		return transientDegradation(s)
	}
	return true
}

// Service identity for automatic recovery: downstream services require an
// authenticated tenant and principal, and a sweep has no caller. Each
// outbound call mints a short-lived token for the run's saved identity,
// signed with the issuer secret as hook-sdk does.
type serviceIdentityKey struct{}

type serviceIdentity struct{ tenantID, principalID, secret string }

func withServiceIdentity(ctx context.Context, tenantID, principalID, secret string) context.Context {
	return context.WithValue(ctx, serviceIdentityKey{}, serviceIdentity{tenantID, principalID, secret})
}

// outboundBearer returns the caller's bearer token, or a freshly minted one
// for a service identity placed in ctx.
func outboundBearer(ctx context.Context) string {
	if token := auth.BearerToken(ctx); token != "" {
		return token
	}
	id, ok := ctx.Value(serviceIdentityKey{}).(serviceIdentity)
	if !ok {
		return ""
	}
	token, err := auth.SignClaims(auth.Claims{TenantID: id.tenantID, PrincipalID: id.principalID,
		Subject: "service:autonomous-control-plane", ExpiresAt: time.Now().UTC().Add(time.Minute)}, id.secret)
	if err != nil {
		return ""
	}
	return token
}

// recoverySweeper periodically finds recoverable runs across tenants and
// recovers them through the same lease a manual recovery takes.
type recoverySweeper struct {
	plane *controlPlane
	// discovery reads runs across tenants. It connects as scos_worker, whose
	// scos_worker_access policy grants cross-tenant SELECT; every claim and
	// write still goes through the plane's tenant-scoped connection.
	discovery *pgxpool.Pool
	interval  time.Duration
	owner     string
	secret    string
	batch     int
	// onlyTenants, when set, limits discovery to those tenants. Production
	// sweeps cover every tenant; integration tests share a database.
	onlyTenants []string
}

// newRecoverySweeperFromEnvironment builds the sweeper, or explains why
// automatic recovery is disabled.
//
//	SCOS_CONTROL_PLANE_RECOVERY_SWEEP=off        disables it
//	SCOS_CONTROL_PLANE_RECOVERY_INTERVAL          sweep interval (default 5m)
//	SCOS_CONTROL_PLANE_WORKER_DATABASE_URL        scos_worker connection (required)
//	SCOS_SECURITY_ISSUER_SECRET                   signs the run identity (required)
func newRecoverySweeperFromEnvironment(ctx context.Context, plane *controlPlane) (*recoverySweeper, string) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_RECOVERY_SWEEP"))) {
	case "off", "false", "0", "disabled":
		return nil, "SCOS_CONTROL_PLANE_RECOVERY_SWEEP is off"
	}
	interval := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_RECOVERY_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return nil, "SCOS_CONTROL_PLANE_RECOVERY_INTERVAL must be a positive duration"
		}
		interval = parsed
	}
	if plane.db == nil {
		return nil, "automatic recovery requires the PostgreSQL control state"
	}
	dsn := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_WORKER_DATABASE_URL"))
	if dsn == "" {
		return nil, "SCOS_CONTROL_PLANE_WORKER_DATABASE_URL is not set; cross-tenant discovery needs the scos_worker role"
	}
	secret := strings.TrimSpace(os.Getenv("SCOS_SECURITY_ISSUER_SECRET"))
	if secret == "" {
		return nil, "SCOS_SECURITY_ISSUER_SECRET is not set; recovered runs could not authenticate downstream"
	}
	if err := database.RequireTLS(dsn); err != nil {
		return nil, err.Error()
	}
	pool, err := database.NewPool(ctx, dsn)
	if err != nil {
		return nil, "connect worker database: " + err.Error()
	}
	return newRecoverySweeper(plane, pool, interval, secret), ""
}

func newRecoverySweeper(plane *controlPlane, discovery *pgxpool.Pool, interval time.Duration, secret string) *recoverySweeper {
	host, _ := os.Hostname()
	id, _ := newID()
	return &recoverySweeper{plane: plane, discovery: discovery, interval: interval,
		owner: "sweeper:" + host + ":" + id, secret: secret, batch: 50}
}

func (s *recoverySweeper) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recovered, err := s.sweepOnce(ctx)
			if err != nil {
				logger.Error("recovery sweep failed", "error", err)
			} else if recovered > 0 {
				logger.Info("recovery sweep", "recovered_attempts", recovered)
			}
		}
	}
}

type sweepCandidate struct {
	tenantID, actorID, interactionID string
	state                            lifecycleState
}

// sweepOnce recovers every eligible run it can claim and returns how many
// recovery attempts it ran.
func (s *recoverySweeper) sweepOnce(ctx context.Context) (int, error) {
	rows, err := s.discovery.Query(ctx, `
		SELECT r.tenant_id::text, d.actor_id::text, r.interaction_id, r.status, r.checkpoint,
		       COALESCE(r.recovery_lease_owner,''), r.recovery_lease_expires_at
		FROM orchestration_runs r
		CROSS JOIN LATERAL (SELECT actor_id FROM routing_decisions
		                    WHERE orchestration_run_id = r.id ORDER BY created_at DESC LIMIT 1) d
		WHERE (r.recovery_lease_expires_at IS NULL OR r.recovery_lease_expires_at < now())
		  AND COALESCE((r.checkpoint->>'recovery_permanent')::boolean, false) = false
		  AND (r.status IN ('interrupted','degraded','failed','recovering')
		       OR (r.status IN ('running','planned') AND COALESCE((r.checkpoint->>'updated_at')::timestamptz, '-infinity'::timestamptz) < now() - make_interval(secs => $1)))
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR r.tenant_id::text = ANY($3::text[]))
		ORDER BY r.started_at
		LIMIT $2`, staleRunAfter.Seconds(), s.batch, s.onlyTenants)
	if err != nil {
		return 0, err
	}
	var candidates []sweepCandidate
	for rows.Next() {
		var c sweepCandidate
		var status, owner string
		var checkpoint []byte
		var expires *time.Time
		if err := rows.Scan(&c.tenantID, &c.actorID, &c.interactionID, &status, &checkpoint, &owner, &expires); err != nil {
			rows.Close()
			return 0, err
		}
		if len(checkpoint) > 0 {
			if err := json.Unmarshal(checkpoint, &c.state); err != nil {
				continue
			}
		}
		c.state.State, c.state.LeaseOwner, c.state.LeaseExpiresAt = status, owner, expires
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	attempted := 0
	for _, c := range candidates {
		if !autoRecoverable(c.state, time.Now().UTC()) {
			continue
		}
		runCtx := withServiceIdentity(ctx, c.tenantID, c.actorID, s.secret)
		claim, err := s.plane.beginRecovery(runCtx, c.tenantID, c.actorID, c.interactionID, s.owner)
		if err != nil || !claim.Claimed {
			continue
		}
		attempted++
		if _, _, err := s.plane.recoverRun(runCtx, c.tenantID, c.actorID, c.interactionID, claim); err != nil {
			logger.Warn("automatic recovery failed", "interaction_id", c.interactionID, "error", err)
		}
	}
	return attempted, nil
}
