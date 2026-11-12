package service

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"ta-payment-back/internal/telemetry"
)

func (c *Container) RunOperationalMetrics(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := c.Pool.Stat()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			slog.Info("runtime_metrics", "db_max", s.MaxConns(), "db_acquired", s.AcquiredConns(),
				"db_idle", s.IdleConns(), "db_acquires_total", s.AcquireCount(),
				"db_empty_acquires_total", s.EmptyAcquireCount(), "db_acquire_duration_total_ms", s.AcquireDuration().Milliseconds(),
				"heap_bytes", m.HeapAlloc, "goroutines", runtime.NumGoroutine(), "gc_cycles_total", m.NumGC)
			if c.Cfg.AsyncNotifications {
				qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				var pending, failed int64
				var oldest float64
				err := c.Pool.QueryRow(qctx, `SELECT count(*) FILTER (WHERE state IN ('pending','processing')),count(*) FILTER (WHERE state='failed'), COALESCE(EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE state IN ('pending','processing'))),0)::float8 FROM notification_outbox WHERE state<>'sent'`).Scan(&pending, &failed, &oldest)
				cancel()
				if err == nil {
					telemetry.NotificationPending.Set(float64(pending))
					telemetry.NotificationFailed.Set(float64(failed))
					telemetry.NotificationOldest.Set(oldest)
					slog.Info("notification_queue_metrics", "pending", pending, "failed", failed)
				} else {
					slog.ErrorContext(ctx, "notification_queue_metrics_failed", "error", err)
				}
			}
		}
	}
}
