package main

import (
	"context"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Capability outcome learning (§21.6). The platform records start from their
// declared priors; every executed route stage then folds its real outcome
// into the registry so ranking reflects observed performance. Learned values
// are persisted in capability_registry and restored at startup.

// capabilitiesForSystem returns the registry records a plan stage exercises.
func capabilitiesForSystem(system string) []string {
	var ids []string
	for _, cap := range routingRegistry.List() {
		if strings.HasPrefix(cap.CapabilityID, system+".") {
			ids = append(ids, cap.CapabilityID)
		}
	}
	return ids
}

// recordStageOutcome is called once per executed stage with the stage's
// result: whether it succeeded and how complete its output was.
func (c *controlPlane) recordStageOutcome(ctx context.Context, req RouteRequest, system string, succeeded bool, quality float64) {
	ids := capabilitiesForSystem(system)
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if err := routingRegistry.RecordOutcome(id, succeeded, quality); err != nil {
			log.Printf("record capability outcome %s: %v", id, err)
		}
	}
	if c == nil || c.db == nil {
		return
	}
	if err := c.persistCapabilityObservations(ctx, req.ActorID, ids); err != nil {
		log.Printf("persist capability observations: %v", err)
	}
}

func (c *controlPlane) persistCapabilityObservations(ctx context.Context, principalID string, ids []string) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, principalID); err != nil {
		return err
	}
	for _, id := range ids {
		cap, ok := routingRegistry.Get(id)
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO capability_registry (capability_key, version, capability_type, supported_intents,
				required_permissions, maximum_data_classification, side_effect_class, idempotency_support,
				reconciliation_support, data_locality, health_state, quality_score, historical_success_rate, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now())
			ON CONFLICT (capability_key, version) DO UPDATE SET
				health_state = EXCLUDED.health_state,
				quality_score = EXCLUDED.quality_score,
				historical_success_rate = EXCLUDED.historical_success_rate,
				generation = capability_registry.generation + 1,
				updated_at = now()`,
			cap.CapabilityID, cap.Version, string(cap.CapabilityType), cap.SupportedIntents,
			cap.RequiredPermissions, cap.MaximumDataClassification, string(cap.SideEffectClass), cap.IdempotencySupport,
			cap.ReconciliationSupport, cap.DataLocality, cap.HealthState, cap.QualityScore, cap.HistoricalSuccessRate); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// restoreCapabilityObservations loads learned values at startup. principalID
// is the control plane's service principal used to satisfy RLS.
func (c *controlPlane) restoreCapabilityObservations(ctx context.Context, principalID string) error {
	if c == nil || c.db == nil {
		return nil
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, principalID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT capability_key, version, historical_success_rate, quality_score FROM capability_registry WHERE historical_success_rate IS NOT NULL AND quality_score IS NOT NULL`)
	if err != nil {
		return err
	}
	type observed struct {
		id, version      string
		success, quality float64
	}
	var loaded []observed
	for rows.Next() {
		var o observed
		if err := rows.Scan(&o.id, &o.version, &o.success, &o.quality); err != nil {
			rows.Close()
			return err
		}
		loaded = append(loaded, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil && err != pgx.ErrNoRows {
		return err
	}
	for _, o := range loaded {
		cap, ok := routingRegistry.Get(o.id)
		// Observations only carry over within the same capability version:
		// a new version starts again from its declared prior.
		if !ok || cap.Version != o.version {
			continue
		}
		if err := routingRegistry.RestoreObserved(o.id, o.success, o.quality); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
