package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"ta-payment-back/internal/telemetry"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(url, "pool_max_conns=") {
		cfg.MaxConns = 20
	}
	if !strings.Contains(url, "pool_min_conns=") {
		cfg.MinConns = 2
	}
	// URL pool parameters are honored; environment overrides are explicit.
	if err := configurePool(cfg); err != nil {
		return nil, err
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.ConnConfig.Tracer = telemetry.DBTracer{}

	// Pin every session to Thailand civil time so CURRENT_DATE agrees with the
	// Go side (see internal/timeutil). CURRENT_DATE decides money: the monthly
	// write lock (resolvePeriodState), the reminder window and the auto-close
	// sweep (SubmissionPeriodService) all compare against it. A stock Postgres
	// image runs UTC, which reports the previous calendar day between 00:00 and
	// 07:00 Bangkok — long enough for a TA to write into a month the system
	// should already have frozen.
	//
	// Set as a connection runtime parameter rather than via ALTER DATABASE so
	// the guarantee travels with the application: a fresh database, a
	// developer's local container, or a managed instance whose default zone we
	// do not control all behave identically.
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Bangkok"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// configurePool rejects malformed capacity settings instead of quietly running
// with a different connection budget. PgBouncer is not required.
func configurePool(cfg *pgxpool.Config) error {
	for _, setting := range []struct {
		name string
		dst  *int32
	}{
		{"DB_POOL_MAX_CONNS", &cfg.MaxConns},
		{"DB_POOL_MIN_CONNS", &cfg.MinConns},
	} {
		if raw := os.Getenv(setting.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > 200 {
				return fmt.Errorf("invalid %s", setting.name)
			}
			*setting.dst = int32(n)
		}
	}
	if cfg.MaxConns < 1 || cfg.MinConns > cfg.MaxConns {
		return fmt.Errorf("invalid database pool capacity")
	}
	return nil
}
