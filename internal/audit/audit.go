package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	ActorID   *uuid.UUID
	ActorRole string
	Action    string
	Entity    string
	EntityID  string
	IP        string
	UserAgent string
	Before    any
	After     any
	Note      string
}

type Auditor struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Auditor { return &Auditor{pool: pool} }

// execer is what both a pool and a transaction satisfy, so an audit row can be
// written either on its own connection or inside a caller's transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Log records one action. It RETURNS the failure rather than absorbing it.
//
// It used to end with `log.Printf("audit: %v", err)` and return nothing, which
// made a lost audit row invisible to everything except a terminal nobody was
// reading: the caller could not tell, the request still answered 200, and the
// screens built on audit_logs — the lecturer's "ประวัติการอนุมัติ", the PII
// access trail on exports — would simply be missing entries with no sign that
// anything had gone wrong. An audit trail that fails quietly is worse than one
// that fails loudly, because it still looks complete.
//
// Callers with a transaction in hand should prefer LogTx: an audit written on
// a separate connection AFTER the change has committed can still fail, and then
// the only honest thing left to report is that the change happened but the
// record of it did not.
func (a *Auditor) Log(ctx context.Context, e Entry) error {
	return write(ctx, a.pool, e)
}

// LogTx writes the audit row inside the caller's transaction, so the change and
// the record of it commit together or not at all. This is the only way to make
// the two agree: post-commit auditing has a window where the first has landed
// and the second cannot.
func (a *Auditor) LogTx(ctx context.Context, tx pgx.Tx, e Entry) error {
	return write(ctx, tx, e)
}

func write(ctx context.Context, q execer, e Entry) error {
	var before, after []byte
	if e.Before != nil {
		var err error
		if before, err = json.Marshal(e.Before); err != nil {
			// Marshalling used to be `_`-ignored, which turned an unencodable
			// value into a NULL `before` column — an audit row that looks
			// complete and has quietly lost the thing it was recording.
			return fmt.Errorf("audit %s: encode before: %w", e.Action, err)
		}
	}
	if e.After != nil {
		var err error
		if after, err = json.Marshal(e.After); err != nil {
			return fmt.Errorf("audit %s: encode after: %w", e.Action, err)
		}
	}
	var ip *string
	if e.IP != "" {
		ip = &e.IP
	}
	var role *string
	if e.ActorRole != "" {
		role = &e.ActorRole
	}
	// actor_id is a FK to users, and the zero uuid is not a user — it is what a
	// caller passes when there is no human behind the action (a scheduled job,
	// a system-initiated round). Storing it verbatim can only ever raise a
	// foreign-key violation, which is precisely the failure this function used
	// to eat. The column is nullable for exactly this case, so say "no actor".
	actor := e.ActorID
	if actor != nil && *actor == uuid.Nil {
		actor = nil
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO audit_logs (actor_id, actor_role, action, entity, entity_id, ip, user_agent, before, after, note)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		actor, role, e.Action, e.Entity, nilIfEmpty(e.EntityID), ip, nilIfEmpty(e.UserAgent), before, after, nilIfEmpty(e.Note)); err != nil {
		return fmt.Errorf("audit %s on %s %s: %w", e.Action, e.Entity, e.EntityID, err)
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
