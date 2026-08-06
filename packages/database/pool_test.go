package database

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPoolLimitsFromEnvironmentDefaults(t *testing.T) {
	limits := PoolLimitsFromEnvironment()
	if limits.MaxConns != defaultMaxConns {
		t.Fatalf("MaxConns = %d, want %d", limits.MaxConns, defaultMaxConns)
	}
	if limits.MaxIdleConns != defaultMaxIdleConns {
		t.Fatalf("MaxIdleConns = %d, want %d", limits.MaxIdleConns, defaultMaxIdleConns)
	}
	if limits.MaxConnIdleTime != defaultMaxConnIdleTime {
		t.Fatalf("MaxConnIdleTime = %s, want %s", limits.MaxConnIdleTime, defaultMaxConnIdleTime)
	}
}

func TestPoolLimitsFromEnvironmentOverrides(t *testing.T) {
	t.Setenv("SCOS_DB_MAX_CONNS", "8")
	t.Setenv("SCOS_DB_MAX_IDLE_CONNS", "4")
	t.Setenv("SCOS_DB_CONN_IDLE_TIME", "45s")
	t.Setenv("SCOS_DB_CONN_LIFETIME", "10m")

	limits := PoolLimitsFromEnvironment()
	if limits.MaxConns != 8 || limits.MaxIdleConns != 4 {
		t.Fatalf("conns = %d/%d, want 8/4", limits.MaxConns, limits.MaxIdleConns)
	}
	if limits.MaxConnIdleTime != 45*time.Second {
		t.Fatalf("MaxConnIdleTime = %s, want 45s", limits.MaxConnIdleTime)
	}
	if limits.MaxConnLifetime != 10*time.Minute {
		t.Fatalf("MaxConnLifetime = %s, want 10m", limits.MaxConnLifetime)
	}
}

// A malformed tuning knob must fall back to the default rather than stopping a
// service from starting, and must never yield a zero or negative pool size --
// database/sql treats MaxOpenConns <= 0 as unlimited, which is the exact
// failure mode these helpers exist to prevent.
func TestPoolLimitsFromEnvironmentRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"0", "-3", "many", " "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SCOS_DB_MAX_CONNS", value)
			t.Setenv("SCOS_DB_CONN_IDLE_TIME", value)

			limits := PoolLimitsFromEnvironment()
			if limits.MaxConns != defaultMaxConns {
				t.Fatalf("MaxConns = %d, want the default %d", limits.MaxConns, defaultMaxConns)
			}
			if limits.MaxConnIdleTime != defaultMaxConnIdleTime {
				t.Fatalf("MaxConnIdleTime = %s, want the default %s", limits.MaxConnIdleTime, defaultMaxConnIdleTime)
			}
		})
	}
}

// Idle connections are held open, so an idle budget above the total budget
// would be silently meaningless in pgxpool and rejected by database/sql.
func TestPoolLimitsClampsIdleToMax(t *testing.T) {
	t.Setenv("SCOS_DB_MAX_CONNS", "2")
	t.Setenv("SCOS_DB_MAX_IDLE_CONNS", "9")

	if limits := PoolLimitsFromEnvironment(); limits.MaxIdleConns != 2 {
		t.Fatalf("MaxIdleConns = %d, want it clamped to 2", limits.MaxIdleConns)
	}
}

func TestTunePoolConfigAppliesLimits(t *testing.T) {
	t.Setenv("SCOS_DB_MAX_CONNS", "5")

	config, err := pgxpool.ParseConfig("postgres://scos@postgres:5432/scos_memory?sslmode=require")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	config.ConnConfig.RuntimeParams["scos.service_id"] = "test-service"

	TunePoolConfig(config)

	if config.MaxConns != 5 {
		t.Fatalf("MaxConns = %d, want 5", config.MaxConns)
	}
	if config.MinConns != 0 {
		t.Fatalf("MinConns = %d, want 0 so idle services hold no backends", config.MinConns)
	}
	if config.MaxConnIdleTime == 0 || config.MaxConnLifetime == 0 {
		t.Fatal("idle connections must be retired, not held forever")
	}
	// Tuning must not disturb caller-supplied connection settings.
	if got := config.ConnConfig.RuntimeParams["scos.service_id"]; got != "test-service" {
		t.Fatalf("scos.service_id = %q, want it preserved", got)
	}
}

func TestTuneNilIsSafe(t *testing.T) {
	if TunePoolConfig(nil) != nil {
		t.Fatal("TunePoolConfig(nil) should return nil")
	}
	if TuneSQLDB(nil) != nil {
		t.Fatal("TuneSQLDB(nil) should return nil")
	}
}

func TestTuneSQLDBBoundsOpenConnections(t *testing.T) {
	t.Setenv("SCOS_DB_MAX_CONNS", "3")
	t.Setenv("SCOS_DB_MAX_IDLE_CONNS", "1")

	// sql.Open does not dial, so this needs no live database.
	db, err := sql.Open("pgx", "postgres://scos@postgres:5432/scos_memory?sslmode=require")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	TuneSQLDB(db)

	stats := db.Stats()
	if stats.MaxOpenConnections != 3 {
		t.Fatalf("MaxOpenConnections = %d, want 3 (0 would mean unlimited)", stats.MaxOpenConnections)
	}
}
