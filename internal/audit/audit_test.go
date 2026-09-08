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

// Every audit call site takes IP, role and session from the request context
// instead of asking each of the 122 of them to pass it. Before this, the live
// table had 0 rows with a role and 0 with an IP out of 407.
func TestLog_TakesTheRequestIdentityFromTheContext(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)

	req := RequestInfo{
		RequestID: uuid.New(),
		SessionID: uuid.New(),
		ActorRole: "staff",
		IP:        "203.0.113.9",
		UserAgent: "probe/1.0",
		Method:    "POST",
		Path:      "/api/v1/things/:id",
	}
	ctx := WithRequest(context.Background(), req)

	// The call site passes NOTHING but the action — exactly like the 110 sites
	// that never populated any of this.
	if err := a.Log(ctx, Entry{Action: "test.ctx", Entity: "thing", EntityID: "x"}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	var role, ip, ua, method, path string
	var reqID, sessID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT actor_role::text, host(ip), user_agent, method, path, request_id, session_id
		FROM audit_logs WHERE action='test.ctx'`).
		Scan(&role, &ip, &ua, &method, &path, &reqID, &sessID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, c := range []struct{ got, want, field string }{
		{role, "staff", "actor_role"},
		{ip, "203.0.113.9", "ip"},
		{ua, "probe/1.0", "user_agent"},
		{method, "POST", "method"},
		{path, "/api/v1/things/:id", "path"},
		{reqID.String(), req.RequestID.String(), "request_id"},
		{sessID.String(), req.SessionID.String(), "session_id"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q — the row cannot be traced without it", c.field, c.got, c.want)
		}
	}
}

// A caller that DID pass a value keeps it. The context fills gaps; it does not
// overwrite a deliberate choice (an admin acting on behalf of someone, a job
// recording the address a webhook came from).
func TestLog_ExplicitFieldsBeatTheContext(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := WithRequest(context.Background(), RequestInfo{ActorRole: "ta", IP: "10.0.0.1"})

	if err := a.Log(ctx, Entry{Action: "test.explicit", Entity: "thing", EntityID: "x",
		ActorRole: "admin", IP: "198.51.100.4"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	var role, ip string
	if err := pool.QueryRow(ctx,
		`SELECT actor_role::text, host(ip) FROM audit_logs WHERE action='test.explicit'`).
		Scan(&role, &ip); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || ip != "198.51.100.4" {
		t.Errorf("got role=%q ip=%q, want the values the caller passed", role, ip)
	}
}

// No request behind the action (the scheduler, a migration) records no request:
// a zero uuid stored verbatim would look like an id someone could look up.
func TestLog_NoRequestLeavesTheTraceColumnsNull(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := context.Background()

	if err := a.Log(ctx, Entry{Action: "test.noreq", Entity: "thing", EntityID: "x"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	var reqID, sessID, ip *string
	if err := pool.QueryRow(ctx,
		`SELECT request_id::text, session_id::text, host(ip) FROM audit_logs WHERE action='test.noreq'`).
		Scan(&reqID, &sessID, &ip); err != nil {
		t.Fatal(err)
	}
	if reqID != nil || sessID != nil || ip != nil {
		t.Errorf("got request_id=%v session_id=%v ip=%v, want all NULL", reqID, sessID, ip)
	}
}

// The trail is evidence, so the database refuses to let the application edit or
// erase it. Nothing in the product updates or deletes an audit row; the trigger
// is there so that no future handler — and no injected statement — can.
func TestAuditLogs_AreAppendOnly(t *testing.T) {
	pool := testutil.NewPool(t)
	a := New(pool)
	ctx := context.Background()
	if err := a.Log(ctx, Entry{Action: "test.immutable", Entity: "thing", EntityID: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE audit_logs SET action='tampered' WHERE action='test.immutable'`,
		`DELETE FROM audit_logs WHERE action='test.immutable'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%q succeeded — the audit trail can be rewritten by anything "+
				"that can reach the database", stmt)
		} else if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q failed with %v, want the append-only guard", stmt, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='test.immutable'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row count = %d after the blocked statements, want 1", n)
	}
}
