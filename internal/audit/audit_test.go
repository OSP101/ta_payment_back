package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// The audit trail is the record of who did what to whose money. Until now this
// package had no test at all, and Log returned nothing: a failed INSERT went to
// a log line and the caller carried on believing the action was recorded.
//
// These tests pin the contract that replaced it.

func TestLog_ReturnsTheFailureInsteadOfSwallowingIt(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := context.Background()

	// An actor that is not a user violates audit_logs_actor_id_fkey. The point
	// is not the constraint — it is that the caller is TOLD.
	ghost := uuid.New()
	err := a.Log(ctx, Entry{ActorID: &ghost, Action: "test.ghost", Entity: "thing", EntityID: "x"})
	if err == nil {
		t.Fatal("a failed audit write reported success — this is the whole defect: the " +
			"row is gone, the screens built on audit_logs quietly lose an entry, and " +
			"nothing upstream can tell")
	}
	if !strings.Contains(err.Error(), "test.ghost") {
		t.Errorf("the error should name the action it failed to record, got: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='test.ghost'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d row(s) written for a write that failed", n)
	}
}

// A system-initiated action has no human actor, and callers express that as the
// zero uuid. Stored verbatim it is a foreign key to a user that cannot exist —
// so it must land as NULL, which is what the nullable column is for.
func TestLog_RecordsAnAbsentActorAsNull(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := context.Background()

	nilActor := uuid.Nil
	if err := a.Log(ctx, Entry{ActorID: &nilActor, Action: "test.system", Entity: "thing"}); err != nil {
		t.Fatalf("an action with no actor must still be recorded: %v", err)
	}

	var actor *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT actor_id FROM audit_logs WHERE action='test.system'`).Scan(&actor); err != nil {
		t.Fatalf("the row was not written: %v", err)
	}
	if actor != nil {
		t.Errorf("actor_id = %v, want NULL — the zero uuid is not a user", *actor)
	}
}

// The happy path, and the fields the reader depends on.
func TestLog_WritesWhatItWasGiven(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := context.Background()

	actor := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'Aud', 'Test', TRUE)`, actor, actor.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	if err := a.Log(ctx, Entry{
		ActorID: &actor, Action: "test.ok", Entity: "assignment", EntityID: "abc",
		Note: "2026-06", After: map[string]int{"count": 3},
	}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	var gotActor uuid.UUID
	var entity, entityID, note, after string
	if err := pool.QueryRow(ctx,
		`SELECT actor_id, entity, entity_id, note, after::text
		   FROM audit_logs WHERE action='test.ok'`).
		Scan(&gotActor, &entity, &entityID, &note, &after); err != nil {
		t.Fatalf("the row was not written: %v", err)
	}
	if gotActor != actor || entity != "assignment" || entityID != "abc" || note != "2026-06" {
		t.Errorf("row = %v/%s/%s/%s, want the values passed in", gotActor, entity, entityID, note)
	}
	if !strings.Contains(after, `"count"`) {
		t.Errorf("the After payload did not survive: %s", after)
	}
}

// LogTx exists so a change and the record of it commit together. If the caller
// rolls back, the audit row must go with it — otherwise the trail claims
// something happened that did not.
func TestLogTx_RollsBackWithTheCallersTransaction(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.LogTx(ctx, tx, Entry{Action: "test.rollback", Entity: "thing"}); err != nil {
		t.Fatalf("LogTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='test.rollback'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d audit row(s) survived a rolled-back transaction", n)
	}
}
