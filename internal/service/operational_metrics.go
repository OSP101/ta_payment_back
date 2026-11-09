package service

import (
	"context"
	"log/slog"
	"runtime"
	"time"
)

func (c *Container) RunOperationalMetrics(ctx context.Context) {
	t := time.NewTicker(30*time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done(): return
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
				err := c.Pool.QueryRow(qctx, `SELECT count(*) FILTER (WHERE state IN ('pending','processing')),count(*) FILTER (WHERE state='failed') FROM notification_outbox WHERE state<>'sent'`).Scan(&pending,&failed)
				cancel()
				if err == nil { slog.Info("notification_queue_metrics", "pending", pending, "failed", failed) }
			}
		}
	}
}
