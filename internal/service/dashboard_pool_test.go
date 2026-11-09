package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGraduateDashboardReleasesRowsBeforeEnrichment(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	cfg := f.Pool.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	svc := &DashboardService{pool: pool}
	rows, err := svc.TaOverview(ctx, f.TAID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || len(rows) == 0 {
		t.Fatalf("dashboard exhausted its one-connection pool: %v", ctx.Err())
	}
}
