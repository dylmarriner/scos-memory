package servicestate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository stores a service's projection as one atomic document.
// PostgreSQL is authoritative; local files are never required by this API.
type Repository struct{ DB *pgxpool.Pool }

func New(ctx context.Context, databaseURL string) (*Repository, error) {
	if err := database.RequireTLS(databaseURL); err != nil {
		return nil, err
	}
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Repository{DB: db}, nil
}

func (r *Repository) Close() {
	if r != nil && r.DB != nil {
		r.DB.Close()
	}
}

func (r *Repository) Load(ctx context.Context, service string, dst any) error {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('scos.service_id', $1, true)`, service); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT state FROM service_state WHERE service=$1`, service).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

func (r *Repository) Save(ctx context.Context, service string, state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.service_id', $1, true)`, service); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO service_state (service,state,updated_at) VALUES ($1,$2,now()) ON CONFLICT (service) DO UPDATE SET state=EXCLUDED.state, updated_at=EXCLUDED.updated_at`, service, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
