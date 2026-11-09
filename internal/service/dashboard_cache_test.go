package service

import (
	"context"
	"testing"
	"github.com/google/uuid"
)

func TestDashboardLedgerCacheIsRequestLocalAndCopiesRows(t *testing.T) {
	key := ledgerCacheKey{Course:uuid.New(),CTE:"approved"}
	ctx := withDashboardCache(context.Background())
	rows := []taSlotCost{{Baht:123.45}}
	cacheLedger(ctx,key,rows)
	rows[0].Baht = 0
	got, ok := cachedLedger(ctx,key)
	if !ok || got[0].Baht != 123.45 { t.Fatal("cache aliased source rows") }
	got[0].Baht = 999
	again,_ := cachedLedger(ctx,key)
	if again[0].Baht != 123.45 { t.Fatal("cache exposed mutable financial rows") }
	if _,ok := cachedLedger(withDashboardCache(context.Background()),key); ok { t.Fatal("cache leaked between requests") }
	key.CTE = "forecast"
	if _,ok := cachedLedger(ctx,key); ok { t.Fatal("approved and forecast prices collided") }
}
