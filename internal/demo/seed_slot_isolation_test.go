package demo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/service"
	"ta-payment-back/internal/testutil"
)

// TestSeedProducesDistinctUserIDsAcrossSlots ปักหมุดสิ่งที่การแยก slot พึ่งพาอยู่
//
// ทุก slot ใช้ TokenService ตัวเดียวกัน (ดูคอมเมนต์ของ Manager ใน manager.go)
// ดังนั้นสิ่งเดียวที่กันไม่ให้โทเคนของ slot 3 ใช้กับ slot 5 ได้ คือ user id
// ที่ไม่ซ้ำกัน ถ้าใครเปลี่ยน seedSlot ไปใช้ UUID คงที่เพื่อให้เทสต์
// deterministic การแยก slot จะพังเงียบ ๆ — เทสต์นี้จะจับได้ (DEMO-05)
func TestSeedProducesDistinctUserIDsAcrossSlots(t *testing.T) {
	ctx := context.Background()

	// Two throwaway databases stand in for two slots — each gets its own
	// seedSlot run, exactly as Bootstrap does per real slot.
	poolA := testutil.NewPool(t)
	poolB := testutil.NewPool(t)

	if err := seedSlot(ctx, &service.Container{Pool: poolA}); err != nil {
		t.Fatalf("seed slot A: %v", err)
	}
	if err := seedSlot(ctx, &service.Container{Pool: poolB}); err != nil {
		t.Fatalf("seed slot B: %v", err)
	}

	idsA := seededUserIDs(t, ctx, poolA)
	idsB := seededUserIDs(t, ctx, poolB)

	if len(idsA) == 0 || len(idsB) == 0 {
		t.Fatal("seedSlot inserted no users — test fixture is broken")
	}
	for id := range idsA {
		if idsB[id] {
			t.Fatalf("user id %s exists in BOTH slots — a demo token minted in one slot "+
				"would authenticate in the other, since every slot shares one TokenService", id)
		}
	}
}

func seededUserIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[uuid.UUID]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id FROM users`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
