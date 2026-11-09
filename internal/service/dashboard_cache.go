package service

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dashboardCacheKey struct{}
type ledgerCacheKey struct { Pool *pgxpool.Pool; Course uuid.UUID; Rate PayRate; CTE string }
type dashboardCache struct { sync.Mutex; ledgers map[ledgerCacheKey][]taSlotCost }

func withDashboardCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, dashboardCacheKey{}, &dashboardCache{ledgers: make(map[ledgerCacheKey][]taSlotCost)})
}

func cachedLedger(ctx context.Context, key ledgerCacheKey) ([]taSlotCost, bool) {
	c, ok := ctx.Value(dashboardCacheKey{}).(*dashboardCache)
	if !ok { return nil, false }
	c.Lock(); defer c.Unlock()
	v, found := c.ledgers[key]
	return append([]taSlotCost(nil), v...), found
}
func cacheLedger(ctx context.Context, key ledgerCacheKey, costs []taSlotCost) {
	if c, ok := ctx.Value(dashboardCacheKey{}).(*dashboardCache); ok {
		c.Lock(); defer c.Unlock()
		c.ledgers[key] = append([]taSlotCost(nil), costs...)
	}
}
