// audit_query.go is how an investigation actually reads the audit trail.
//
// Before this, the only way in was `SELECT … FROM audit_logs ORDER BY at DESC
// LIMIT 200` with every filter applied in the browser. That is not a query
// interface, it is a peek at the newest page: a question like "what did this
// account do on 3 September" could not be asked at all once 200 newer rows
// existed, and no amount of filtering in the browser could reach a row the
// server never sent.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/storage"
)

// AuditRow is one row as the investigation screen shows it.
type AuditRow struct {
	ID        int64      `json:"id"`
	At        time.Time  `json:"at"`
	ActorID   *uuid.UUID `json:"actor_id"`
	ActorName string     `json:"actor_name,omitempty"`
	ActorRole *string    `json:"actor_role"`
	Action    string     `json:"action"`
	Entity    string     `json:"entity"`
	EntityID  *string    `json:"entity_id"`
	// SubjectName is who/what EntityID refers to, when it can be resolved —
	// a person's name or a course code. Empty when the id points at something
	// with no readable name.
	SubjectName string          `json:"subject_name,omitempty"`
	IP          *string         `json:"ip"`
	UserAgent   *string         `json:"user_agent"`
	Method      *string         `json:"method"`
	Path        *string         `json:"path"`
	RequestID   *uuid.UUID      `json:"request_id"`
	SessionID   *uuid.UUID      `json:"session_id"`
	Note        *string         `json:"note"`
	Before      json.RawMessage `json:"before"`
	After       json.RawMessage `json:"after"`
}

// AuditQuery is one question put to the trail.
//
// From/To are not optional in practice: ListAudit defaults them, because an
// unbounded audit query is a full scan of a table that only ever grows, and
// both the page count and the offset need a bounded set to be answerable at
// all. An investigation always knows roughly when.
type AuditQuery struct {
	From, To time.Time
	ActorID  *uuid.UUID
	Role     string
	// Action matches by PREFIX, so "auth." reads as "everything about signing
	// in" — the way the action names were already grouped by their dots.
	Action    string
	Entity    string
	EntityID  string
	IP        string
	RequestID *uuid.UUID
	SessionID *uuid.UUID
	// Q is a free-text contains over the fields a person types from memory:
	// the action, what was acted on, the note, and the address.
	Q      string
	Limit  int
	Offset int
}

// AuditDefaultWindow is how far back an unspecified query looks. Long enough to
// cover "what happened this week", short enough that the count behind the page
// numbers stays cheap. A longer look-back is a deliberate act: widen the dates.
const AuditDefaultWindow = 7 * 24 * time.Hour

// ListAudit answers one AuditQuery, newest first, plus the total number of
// matching rows so the screen can page through them.
func (s *AuditService) ListAudit(ctx context.Context, q AuditQuery) ([]AuditRow, int, error) {
	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-AuditDefaultWindow)
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	where := []string{"a.at >= $1", "a.at <= $2"}
	args := []any{q.From, q.To}
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if q.ActorID != nil {
		add("a.actor_id = $%d", *q.ActorID)
	}
	if q.Role != "" {
		add("a.actor_role::text = $%d", q.Role)
	}
	if q.Action != "" {
		// Prefix, not contains: the index on (action, at DESC) can serve it,
		// and "auth." meaning "the auth family" is how these names are built.
		//
		// A COMMA-SEPARATED list is one question too: the screen's overview
		// cards each stand for a group of actions ("เข้าระบบไม่สำเร็จ" is a
		// failed password, an unknown account, and a failed second factor), and
		// clicking one has to reach all of them. Matching the whole list as a
		// single prefix — which is what this did at first — returns nothing at
		// all, so the card looked broken rather than empty.
		var ors []string
		for _, part := range strings.Split(q.Action, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			args = append(args, likePrefix(part))
			ors = append(ors, fmt.Sprintf("a.action LIKE $%d", len(args)))
		}
		if len(ors) > 0 {
			where = append(where, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if q.Entity != "" {
		add("a.entity = $%d", q.Entity)
	}
	if q.EntityID != "" {
		add("a.entity_id = $%d", q.EntityID)
	}
	if q.IP != "" {
		// host() so a typed "203.0.113.9" matches the stored inet, which would
		// otherwise only compare equal to the same value with its /32.
		add("host(a.ip) = $%d", q.IP)
	}
	if q.RequestID != nil {
		add("a.request_id = $%d", *q.RequestID)
	}
	if q.SessionID != nil {
		add("a.session_id = $%d", *q.SessionID)
	}
	if q.Q != "" {
		// One bind reused across six columns, so `add`'s single-placeholder
		// formatting does not apply here.
		args = append(args, likeContains(q.Q))
		n := len(args)
		where = append(where, fmt.Sprintf(
			`(a.action ILIKE $%[1]d OR a.entity ILIKE $%[1]d OR a.entity_id ILIKE $%[1]d
			  OR a.note ILIKE $%[1]d OR host(a.ip) ILIKE $%[1]d OR a.path ILIKE $%[1]d)`, n))
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs a WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, q.Limit, q.Offset)
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.at, a.actor_id,
		       COALESCE(NULLIF(TRIM(COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')),''), ''),
		       a.actor_role::text, a.action, a.entity, a.entity_id, host(a.ip), a.user_agent,
		       a.method, a.path, a.request_id, a.session_id, a.note, a.before, a.after,
		       -- Who or what was ACTED ON, by name. The screen showed a truncated
		       -- uuid here while displaying the actor's real name two columns to
		       -- the left, which made "who looked at whose record" — the question
		       -- these rows exist to answer — unreadable at a glance.
		       --
		       -- Joined on id::text rather than casting entity_id to uuid: that
		       -- column is free text (it holds "periodID/taID" pairs and storage
		       -- keys too) and a cast would raise on the first row that is not a
		       -- uuid, taking the whole query with it.
		       COALESCE(
		           NULLIF(TRIM(COALESCE(su.first_name,'')||' '||COALESCE(su.last_name,'')),''),
		           tc.code, '')
		FROM audit_logs a
		LEFT JOIN users u  ON u.id = a.actor_id
		LEFT JOIN users su ON a.entity = 'user' AND su.id::text = a.entity_id
		LEFT JOIN teaching_courses tc ON a.entity = 'teaching_course' AND tc.id::text = a.entity_id
		WHERE `+cond+`
		ORDER BY a.at DESC, a.id DESC
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		var before, after []byte
		if err := rows.Scan(&r.ID, &r.At, &r.ActorID, &r.ActorName, &r.ActorRole,
			&r.Action, &r.Entity, &r.EntityID, &r.IP, &r.UserAgent,
			&r.Method, &r.Path, &r.RequestID, &r.SessionID, &r.Note, &before, &after,
			&r.SubjectName); err != nil {
			return nil, 0, err
		}
		r.Before, r.After = json.RawMessage(before), json.RawMessage(after)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// ListAuditActions returns the distinct action names, for the screen's filter.
//
// A plain SELECT DISTINCT is a full scan, and this list is short (about 125
// names) against a table that grows forever. The recursive form is a loose
// index scan: it walks audit_action_idx one distinct value at a time, so the
// cost is the number of ACTIONS rather than the number of rows.
func (s *AuditService) ListAuditActions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE walk AS (
		    SELECT (SELECT MIN(action) FROM audit_logs) AS action
		    UNION ALL
		    SELECT (SELECT MIN(a.action) FROM audit_logs a WHERE a.action > w.action)
		    FROM walk w WHERE w.action IS NOT NULL
		)
		SELECT action FROM walk WHERE action IS NOT NULL ORDER BY action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func likePrefix(s string) string {
	return escapeLike(s) + "%"
}

func likeContains(s string) string {
	return "%" + escapeLike(s) + "%"
}

// escapeLike neutralises the wildcards so a search for "100%" looks for the
// text "100%" rather than for everything starting with 100.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// AuditService reads the trail. It deliberately has no writer: writing is
// audit.Auditor's job, and a service that could do both would be one refactor
// away from a handler that edits the record it is displaying.
type AuditService struct {
	pool *pgxpool.Pool
	// store is where expired rows are archived before they are deleted — the
	// same encrypted document store the claim files use. See audit_retention.go.
	store storage.Store
}

// AuditActionCount is one action's activity inside the window.
//
// DistinctIPs is what separates a person mistyping their own password eight
// times from eight failures spread across eight addresses — the second is the
// shape of an attack and the first is a Monday morning.
type AuditActionCount struct {
	Action         string `json:"action"`
	Count          int    `json:"count"`
	DistinctIPs    int    `json:"distinct_ips"`
	DistinctActors int    `json:"distinct_actors"`
}

// AuditActor is one person who did something in the window, for the screen's
// "look up this person" filter.
type AuditActor struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Role  string    `json:"role,omitempty"`
	Count int       `json:"count"`
}

// AuditSummary is the overview the screen shows above the table.
type AuditSummary struct {
	Actions []AuditActionCount `json:"actions"`
	Actors  []AuditActor       `json:"actors"`
}

// SummarizeAudit counts the window by action and lists who was active in it.
//
// Deliberately raw counts rather than a server-side verdict: what counts as
// alarming is a presentation decision, and keeping the vocabulary in one place
// (the screen) stops the labels and the thresholds drifting apart in two.
//
// The actor list is drawn from the WINDOW, not from the user table. It is
// shorter, it is ordered by who was actually busy, and it avoids the screen
// having to read the full account roster — which is itself an audited
// disclosure (users.list.view), so opening the audit page would otherwise file
// a row saying the reader had gone through everyone's details.
func (s *AuditService) SummarizeAudit(ctx context.Context, from, to time.Time) (*AuditSummary, error) {
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-AuditDefaultWindow)
	}
	out := &AuditSummary{Actions: []AuditActionCount{}, Actors: []AuditActor{}}

	rows, err := s.pool.Query(ctx, `
		SELECT action, COUNT(*), COUNT(DISTINCT ip), COUNT(DISTINCT actor_id)
		FROM audit_logs WHERE at >= $1 AND at <= $2
		GROUP BY action ORDER BY COUNT(*) DESC`, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a AuditActionCount
		if err := rows.Scan(&a.Action, &a.Count, &a.DistinctIPs, &a.DistinctActors); err != nil {
			rows.Close()
			return nil, err
		}
		out.Actions = append(out.Actions, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	arows, err := s.pool.Query(ctx, `
		SELECT a.actor_id,
		       COALESCE(NULLIF(TRIM(COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')),''), ''),
		       COALESCE(MAX(a.actor_role::text), ''), COUNT(*)
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE a.at >= $1 AND a.at <= $2 AND a.actor_id IS NOT NULL
		GROUP BY a.actor_id, u.first_name, u.last_name
		ORDER BY COUNT(*) DESC LIMIT 200`, from, to)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var a AuditActor
		if err := arows.Scan(&a.ID, &a.Name, &a.Role, &a.Count); err != nil {
			return nil, err
		}
		out.Actors = append(out.Actors, a)
	}
	return out, arows.Err()
}
