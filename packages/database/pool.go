package database

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The fabric runs every governed service as its own process against a single
// Postgres instance. On a small host (the reference deployment is a Raspberry
// Pi 5 sized to fit a 4 GB budget) the connection count, not the query load, is
// what exhausts memory first: each Postgres backend costs several megabytes of
// private memory, and the library defaults are wildly generous for this shape.
//
//   - database/sql defaults to an UNBOUNDED number of open connections, so a
//     burst of concurrent requests across a dozen services can open hundreds of
//     backends and OOM the box.
//   - pgxpool defaults MaxConns to max(4, runtime.NumCPU()) and never retires
//     idle connections, so twelve services pin dozens of backends open forever.
//
// These helpers apply a single conservative budget to both driver styles. The
// defaults are sized for that reference host; raise them via the SCOS_DB_*
// environment variables on a larger deployment.
const (
	defaultMaxConns        = 3
	defaultMaxIdleConns    = 1
	defaultMaxConnIdleTime = 2 * time.Minute
	defaultMaxConnLifetime = 30 * time.Minute
	defaultHealthCheck     = 1 * time.Minute
)

// PoolLimits is the resolved per-process connection budget.
type PoolLimits struct {
	MaxConns        int32
	MaxIdleConns    int32
	MaxConnIdleTime time.Duration
	MaxConnLifetime time.Duration
	HealthCheck     time.Duration
}

// PoolLimitsFromEnvironment resolves the budget from SCOS_DB_MAX_CONNS,
// SCOS_DB_MAX_IDLE_CONNS, SCOS_DB_CONN_IDLE_TIME and SCOS_DB_CONN_LIFETIME,
// falling back to the small-host defaults. Invalid or non-positive values are
// ignored rather than fatal: a malformed tuning knob must never stop a service
// from starting.
func PoolLimitsFromEnvironment() PoolLimits {
	limits := PoolLimits{
		MaxConns:        defaultMaxConns,
		MaxIdleConns:    defaultMaxIdleConns,
		MaxConnIdleTime: defaultMaxConnIdleTime,
		MaxConnLifetime: defaultMaxConnLifetime,
		HealthCheck:     defaultHealthCheck,
	}
	if v := positiveInt("SCOS_DB_MAX_CONNS"); v > 0 {
		limits.MaxConns = int32(v)
	}
	if v := positiveInt("SCOS_DB_MAX_IDLE_CONNS"); v > 0 {
		limits.MaxIdleConns = int32(v)
	}
	if v := positiveDuration("SCOS_DB_CONN_IDLE_TIME"); v > 0 {
		limits.MaxConnIdleTime = v
	}
	if v := positiveDuration("SCOS_DB_CONN_LIFETIME"); v > 0 {
		limits.MaxConnLifetime = v
	}
	if limits.MaxIdleConns > limits.MaxConns {
		limits.MaxIdleConns = limits.MaxConns
	}
	return limits
}

func positiveInt(key string) int {
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func positiveDuration(key string) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// TunePoolConfig applies the resolved budget to a pgxpool configuration.
// Callers that need per-service RuntimeParams should ParseConfig, set their
// params, call this, and then hand the config to pgxpool.NewWithConfig.
func TunePoolConfig(config *pgxpool.Config) *pgxpool.Config {
	if config == nil {
		return config
	}
	limits := PoolLimitsFromEnvironment()
	config.MaxConns = limits.MaxConns
	config.MinConns = 0
	config.MaxConnIdleTime = limits.MaxConnIdleTime
	config.MaxConnLifetime = limits.MaxConnLifetime
	config.HealthCheckPeriod = limits.HealthCheck
	return config
}

// NewPool is a drop-in replacement for pgxpool.New that applies the budget.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, TunePoolConfig(config))
}

// TuneSQLDB applies the budget to a database/sql handle. Without this the
// standard library will open an unbounded number of Postgres backends.
func TuneSQLDB(db *sql.DB) *sql.DB {
	if db == nil {
		return db
	}
	limits := PoolLimitsFromEnvironment()
	db.SetMaxOpenConns(int(limits.MaxConns))
	db.SetMaxIdleConns(int(limits.MaxIdleConns))
	db.SetConnMaxIdleTime(limits.MaxConnIdleTime)
	db.SetConnMaxLifetime(limits.MaxConnLifetime)
	return db
}
