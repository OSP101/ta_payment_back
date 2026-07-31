package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute

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
