package scheduler

import (
	"context"
	"testing"
	"time"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// OPS-01: dailyClose used to fire only off a 24h ticker that reset on every
// process restart — a deployment that restarts more than once a day would
// never run it at all (5-year audit retention, session/challenge cleanup,
// and is_closed auto-close all live there). This pins that a freshly
// started Scheduler runs dailyClose well within its first minute, not its
// first day.
func TestScheduler_RunsDailyCloseOnStartupNotJustAfter24Hours(t *testing.T) {
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.New(pool)
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), aud, config.Config{}, nil, nil)

	s := New(svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// loop's own 5s startup jitter runs before the first tick/dailyClose —
	// poll rather than sleep exactly 5s so a slow CI runner doesn't flake.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var ran bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS(SELECT 1 FROM scheduler_daily_close_state)`).Scan(&ran); err != nil {
			t.Fatal(err)
		}
		if ran {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("dailyClose did not run within 10 seconds of Scheduler.Start — " +
				"it must run at startup, not wait for a 24h ticker")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
