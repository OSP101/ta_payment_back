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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/storage"
)

// AuditRow is one row of the trail: the stored fields, plus the reading of
// them that audit_view.go derives (label, category, outcome, changes …).
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
	// a person's name, a course and section, a document. Empty when the id
	// points at something with no readable name.
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

	// ── Derived at read time (see decorate) ────────────────────────────────
	// Ref is what a person quotes for this row: "AL-001366".
	Ref           string `json:"ref"`
	Label         string `json:"label"`
	Category      string `json:"category"`
	CategoryLabel string `json:"category_label"`
	Severity      string `json:"severity"`
	Outcome       string `json:"outcome"`
	// ActorKind separates the three things a missing actor can mean: "user",
	// "system" (a job or an automatic decision), "anonymous" (a request from
	// someone not signed in) and "unknown" (nobody recorded one).
	ActorKind string `json:"actor_kind"`
	// Automatic marks something the system decided or did by itself, even when
	// a person's request set it off ("the import auto-filled 336 makeups").
	// The actor, if any, started it; they did not do it.
	Automatic bool `json:"automatic,omitempty"`
	// Device is the user agent in words; Network places the address ("local",
	// "private", "public").
	Device  string        `json:"device,omitempty"`
	Network string        `json:"network,omitempty"`
	Details []string      `json:"details,omitempty"`
	Changes []AuditChange `json:"changes,omitempty"`
	// Count is how many rows this one stands for when the query folds a burst
	// (one request writing the same action hundreds of times) into its newest
	// row. 1 everywhere else.
	Count int `json:"count"`
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
	// Action is one action by name, or — with a trailing dot — a family:
	// "auth." reads as "everything about signing in", the way the action names
	// were already grouped by their dots. A comma-separated list is any of them.
	Action    string
	Entity    string
	EntityID  string
	IP        string
	RequestID *uuid.UUID
	SessionID *uuid.UUID
	// Category, Severity and Outcome are the catalog's terms (internal/audit).
	// Severity and Outcome take comma-separated lists. They are answered as a list of
	// actions, which is what lets them reach rows written before the catalog
	// existed and keeps them on the action index.
	Category string
	Severity string
	Outcome  string
	// Q is free text over what a person types from memory: a name, a course
	// code, the words on screen, an address, or a reference (AL-001366).
	Q string
	// Fold collapses a burst — every row one request wrote under one action —
	// into its newest row, with Count saying how many it stands for. One
	// timetable import files 336 "makeup auto-filled" rows; unfolded they are
	// seven pages that bury everything a person did that day.
	Fold   bool
	Limit  int
	Offset int
	// MaxLimit raises the page ceiling for the export; 0 means the screen's.
	MaxLimit int
}

// AuditDefaultWindow is how far back an unspecified query looks. Long enough to
// cover "what happened this week", short enough that the count behind the page
// numbers stays cheap. A longer look-back is a deliberate act: widen the dates.
const AuditDefaultWindow = 7 * 24 * time.Hour

// auditRelatedIDs turns a search for a person or a course into the ids of
// everything that belongs to them — their assignments, documents, sections,
// period rows — so "CP363205" or a TA's name returns the whole file on that
// subject, not just the rows that happen to carry the name in a text column.
// audit_logs stores ids; this is the join a reader assumes exists.
func (s *AuditService) auditRelatedIDs(ctx context.Context, q string) (entityIDs []string, actorIDs []uuid.UUID, err error) {
	rows, err := s.pool.Query(ctx, `
		WITH u AS (
		    SELECT id FROM users
		     WHERE (COALESCE(first_name,'')||' '||COALESCE(last_name,'')) ILIKE $1
		        OR email::text ILIKE $1 OR COALESCE(student_id,'') ILIKE $1
		     LIMIT 50),
		c AS (
		    SELECT id FROM teaching_courses
		     WHERE code ILIKE $1 OR COALESCE(name_th,'') ILIKE $1
		     LIMIT 50),
		sec AS (SELECT id FROM sections WHERE teaching_course_id IN (SELECT id FROM c)),
		asg AS (SELECT id FROM ta_request_assignments
		         WHERE ta_id IN (SELECT id FROM u) OR section_id IN (SELECT id FROM sec))
		SELECT 'user', id::text FROM u
		UNION ALL SELECT 'x', id::text FROM c
		UNION ALL SELECT 'x', id::text FROM sec
		UNION ALL SELECT 'x', id::text FROM asg
		UNION ALL SELECT 'x', id::text FROM ta_documents WHERE user_id IN (SELECT id FROM u)
		UNION ALL SELECT 'x', id::text FROM ta_requests WHERE teaching_course_id IN (SELECT id FROM c)
		UNION ALL SELECT 'x', id::text FROM submission_period_status
		           WHERE ta_id IN (SELECT id FROM u) OR teaching_course_id IN (SELECT id FROM c)
		LIMIT 5000`, likeContains(q))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, nil, err
		}
		entityIDs = append(entityIDs, id)
		if kind == "user" {
			if uid, perr := uuid.Parse(id); perr == nil {
				actorIDs = append(actorIDs, uid)
			}
		}
	}
	return entityIDs, actorIDs, rows.Err()
}

// auditWhere builds the WHERE clause for one question. Shared by the list, the
// export and anything else that must agree with them about which rows match.
func (s *AuditService) auditWhere(ctx context.Context, q *AuditQuery) (string, []any, error) {
	// A reference names exactly one row, whenever it was written. Someone
	// quoting "AL-001366" from a memo should not also have to know the month.
	if id, isRef := ParseAuditRef(q.Q); isRef {
		return "a.id = $1", []any{id}, nil
	}

	if q.To.IsZero() {
		q.To = time.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-AuditDefaultWindow)
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
		// Exact or prefix, never contains: the index on (action, at DESC) can
		// serve both, and "auth." meaning "the auth family" is how these names
		// are built.
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
			// A trailing dot asks for the family; anything else is one action
			// by its whole name. Matching a whole name as a prefix made
			// "worklog.staff_edit" also return "worklog.staff_edit_batch", so
			// opening an item from the overview showed rows it had not counted.
			if strings.HasSuffix(part, ".") {
				args = append(args, likePrefix(part))
				ors = append(ors, fmt.Sprintf("a.action LIKE $%d", len(args)))
			} else {
				args = append(args, part)
				ors = append(ors, fmt.Sprintf("a.action = $%d", len(args)))
			}
		}
		if len(ors) > 0 {
			where = append(where, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if q.Category != "" || q.Severity != "" || q.Outcome != "" {
		sev := map[string]bool{}
		for _, p := range strings.Split(q.Severity, ",") {
			if p = strings.TrimSpace(p); p != "" {
				sev[p] = true
			}
		}
		outc := map[string]bool{}
		for _, p := range strings.Split(q.Outcome, ",") {
			if p = strings.TrimSpace(p); p != "" {
				outc[p] = true
			}
		}
		actions := audit.ActionsWhere(func(i audit.ActionInfo) bool {
			return (q.Category == "" || string(i.Category) == q.Category) &&
				(len(sev) == 0 || sev[string(i.Severity)]) &&
				(len(outc) == 0 || outc[string(i.Outcome)])
		})
		// An empty list must match nothing. `= ANY('{}')` already does, but it
		// is said out loud here because the alternative reading — "no filter" —
		// would turn a mistyped category into the whole table.
		add("a.action = ANY($%d)", actions)
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
		// The note of a search or an export of the trail IS somebody's earlier
		// query. Matching it made every search return the record of the last
		// time the same words were searched — "CP363205" found "Admin searched
		// for CP363205" — and a search for something that does not exist
		// stopped coming back empty the second time it was typed. Those rows
		// are still reachable by action, by person and by date.
		ors := []string{fmt.Sprintf(
			`a.action ILIKE $%[1]d OR a.entity ILIKE $%[1]d OR a.entity_id ILIKE $%[1]d
			  OR (a.note ILIKE $%[1]d AND a.action NOT IN ('audit_log.search', 'audit_log.export'))
			  OR host(a.ip) ILIKE $%[1]d OR a.path ILIKE $%[1]d`, n)}

		// The words on screen. Nobody types "worklog.approve"; they type
		// "อนุมัติ", which appears nowhere in the table.
		if labelled := audit.ActionsLabelled(q.Q); len(labelled) > 0 {
			args = append(args, labelled)
			ors = append(ors, fmt.Sprintf("a.action = ANY($%d)", len(args)))
		}
		// A person or a course, by name. One character matches half the
		// roster, so this waits for two.
		if utf8.RuneCountInString(q.Q) >= 2 {
			entityIDs, actorIDs, err := s.auditRelatedIDs(ctx, q.Q)
			if err != nil {
				return "", nil, err
			}
			if len(entityIDs) > 0 {
				args = append(args, entityIDs)
				ors = append(ors, fmt.Sprintf("a.entity_id = ANY($%d)", len(args)))
			}
			if len(actorIDs) > 0 {
				args = append(args, actorIDs)
				ors = append(ors, fmt.Sprintf("a.actor_id = ANY($%d)", len(args)))
			}
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	return strings.Join(where, " AND "), args, nil
}

// ListAudit answers one AuditQuery, newest first, plus the total number of
// matching rows (or, when folding, bursts) so the screen can page through them.
func (s *AuditService) ListAudit(ctx context.Context, q AuditQuery) ([]AuditRow, int, error) {
	max := 200
	if q.MaxLimit > 0 {
		max = q.MaxLimit
	}
	if q.Limit <= 0 || q.Limit > max {
		q.Limit = 50
		if q.MaxLimit > 0 {
			q.Limit = q.MaxLimit
		}
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	cond, args, err := s.auditWhere(ctx, &q)
	if err != nil {
		return nil, 0, err
	}

	// `picked` is the set of row ids the page is drawn from, with how many
	// rows each stands for. Unfolded that is every matching row, counting 1.
	// Folded, rows sharing a request and an action collapse to the newest of
	// them; a row with no request behind it (a scheduled job) is its own group.
	picked := `SELECT a.id, 1 AS n FROM audit_logs a WHERE ` + cond
	if q.Fold {
		picked = `SELECT MAX(a.id) AS id, COUNT(*) AS n FROM audit_logs a WHERE ` + cond + `
		          GROUP BY COALESCE(a.request_id::text || '|' || a.action, a.id::text)`
	}

	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM (`+picked+`) p`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, q.Limit, q.Offset)
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.at, a.actor_id,
		       COALESCE(NULLIF(TRIM(COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')),''), ''),
		       a.actor_role::text, a.action, a.entity, a.entity_id, host(a.ip), a.user_agent,
		       a.method, a.path, a.request_id, a.session_id, a.note, a.before, a.after,
		       -- Who or what was ACTED ON, by name (migration 0130). The screen
		       -- showed a truncated uuid here while displaying the actor's real
		       -- name two columns to the left, which made "who looked at whose
		       -- record" — the question these rows exist to answer — unreadable.
		       -- The function never raises: entity_id is free text (it holds
		       -- "periodID/taID" pairs and storage keys too) and one odd row
		       -- must not take the page with it.
		       COALESCE(audit_subject_label(a.entity, a.entity_id), ''),
		       p.n
		FROM (`+picked+`) p
		JOIN audit_logs a ON a.id = p.id
		LEFT JOIN users u ON u.id = a.actor_id
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
			&r.SubjectName, &r.Count); err != nil {
			return nil, 0, err
		}
		r.Before, r.After = json.RawMessage(before), json.RawMessage(after)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close()

	if err := s.resolveVirtualSubjects(ctx, out); err != nil {
		return nil, 0, err
	}
	for i := range out {
		decorate(&out[i])
	}
	return out, total, nil
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
//
// Events counts bursts rather than rows: 336 rows written by one import are
// one event. It is the number that matches what the folded timeline shows.
type AuditActionCount struct {
	Action         string `json:"action"`
	Count          int    `json:"count"`
	Events         int    `json:"events"`
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

// AuditCategoryCount is how much happened in one category of the catalog.
type AuditCategoryCount struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Events int    `json:"events"`
}

// AuditAttention is one thing in the window worth a second look: a refusal, a
// lock-out, a decision undone, a change to somebody's access. Rows of the same
// kind by the same person about the same thing are one item with a count —
// four wrong passwords are one fact, not four.
type AuditAttention struct {
	Action      string     `json:"action"`
	Label       string     `json:"label"`
	Severity    string     `json:"severity"`
	Outcome     string     `json:"outcome"`
	ActorID     *uuid.UUID `json:"actor_id"`
	ActorName   string     `json:"actor_name,omitempty"`
	Entity      string     `json:"entity"`
	EntityID    *string    `json:"entity_id"`
	SubjectName string     `json:"subject_name,omitempty"`
	// Subjects is how many different things the rows were about; the subject
	// is named only when it is 1.
	Subjects    int       `json:"subjects"`
	Count       int       `json:"count"`
	DistinctIPs int       `json:"distinct_ips"`
	LastAt      time.Time `json:"last_at"`
	// LastID is the newest row behind the item, so the screen can open it.
	LastID int64  `json:"last_id"`
	Ref    string `json:"ref"`
}

// AuditSummary is the overview the screen shows above the timeline.
type AuditSummary struct {
	Actions    []AuditActionCount   `json:"actions"`
	Actors     []AuditActor         `json:"actors"`
	Categories []AuditCategoryCount `json:"categories"`
	Attention  []AuditAttention     `json:"attention"`
	// Events is the number of (folded) events in the window; Rows the number
	// of stored rows behind them.
	Events int `json:"events"`
	Rows   int `json:"rows"`
}

// auditRoutineFailureThreshold is how many times a sign-in has to fail before
// it is worth anyone's attention. One or two wrong passwords is a person
// typing; the list of things to check would be nothing else otherwise.
const auditRoutineFailureThreshold = 3

// SummarizeAudit counts the window by action and category, lists who was active
// in it, and picks out what deserves a second look.
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
	out := &AuditSummary{
		Actions: []AuditActionCount{}, Actors: []AuditActor{},
		Categories: []AuditCategoryCount{}, Attention: []AuditAttention{},
	}

	rows, err := s.pool.Query(ctx, `
		SELECT action, COUNT(*),
		       COUNT(DISTINCT COALESCE(request_id::text, id::text)),
		       COUNT(DISTINCT ip), COUNT(DISTINCT actor_id)
		FROM audit_logs WHERE at >= $1 AND at <= $2
		GROUP BY action ORDER BY COUNT(*) DESC`, from, to)
	if err != nil {
		return nil, err
	}
	byCat := map[audit.Category]int{}
	for rows.Next() {
		var a AuditActionCount
		if err := rows.Scan(&a.Action, &a.Count, &a.Events, &a.DistinctIPs, &a.DistinctActors); err != nil {
			rows.Close()
			return nil, err
		}
		out.Actions = append(out.Actions, a)
		out.Rows += a.Count
		out.Events += a.Events
		byCat[audit.Describe(a.Action).Category] += a.Events
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range audit.Categories() {
		out.Categories = append(out.Categories,
			AuditCategoryCount{ID: string(c.ID), Label: c.Label, Events: byCat[c.ID]})
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
	for arows.Next() {
		var a AuditActor
		if err := arows.Scan(&a.ID, &a.Name, &a.Role, &a.Count); err != nil {
			arows.Close()
			return nil, err
		}
		out.Actors = append(out.Actors, a)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, err
	}

	loud := audit.ActionsWhere(func(i audit.ActionInfo) bool {
		return i.Severity == audit.SevWarn || i.Severity == audit.SevDanger
	})
	// One item per (what, who). Per subject as well would list twenty deleted
	// rows as twenty things to check; the subject is named only when there was
	// exactly one, and Subjects says how many there were otherwise.
	trows, err := s.pool.Query(ctx, `
		SELECT a.action, a.actor_id,
		       COALESCE(NULLIF(TRIM(COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')),''), ''),
		       MIN(a.entity), MIN(a.entity_id),
		       COUNT(DISTINCT a.entity_id),
		       COUNT(*), COUNT(DISTINCT a.ip), MAX(a.at), MAX(a.id)
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE a.at >= $1 AND a.at <= $2 AND a.action = ANY($3)
		GROUP BY a.action, a.actor_id, u.first_name, u.last_name
		ORDER BY MAX(a.at) DESC, MAX(a.id) DESC
		LIMIT 200`, from, to, loud)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var t AuditAttention
		if err := trows.Scan(&t.Action, &t.ActorID, &t.ActorName, &t.Entity, &t.EntityID,
			&t.Subjects, &t.Count, &t.DistinctIPs, &t.LastAt, &t.LastID); err != nil {
			trows.Close()
			return nil, err
		}
		if t.Subjects != 1 {
			t.EntityID = nil
		}
		info := audit.Describe(t.Action)
		// A wrong password or two is somebody typing. A lock-out is never
		// routine, and neither is a refusal for lack of permission.
		if info.Outcome == audit.OutcomeFailed && info.Category == audit.CatAccess &&
			!strings.HasSuffix(t.Action, "_locked") && t.Count < auditRoutineFailureThreshold {
			continue
		}
		t.Label, t.Severity, t.Outcome = info.Label, string(info.Severity), string(info.Outcome)
		t.Ref = AuditRef(t.LastID)
		out.Attention = append(out.Attention, t)
		if len(out.Attention) == 30 {
			break
		}
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Attention {
		t := &out.Attention[i]
		if t.EntityID == nil {
			continue
		}
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(audit_subject_label($1, $2), '')`,
			t.Entity, *t.EntityID).Scan(&t.SubjectName); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AuditSession is one signed-in session as the overview lists it.
type AuditSession struct {
	ID             uuid.UUID `json:"id"`
	UserID         uuid.UUID `json:"user_id"`
	Name           string    `json:"name"`
	Roles          []string  `json:"roles"`
	Device         string    `json:"device,omitempty"`
	IP             string    `json:"ip,omitempty"`
	Network        string    `json:"network,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
}

// ActiveSessions lists the sessions that could act on the system right now:
// not revoked, not expired, and touched within the idle timeout. This is the
// "where you're logged in" view — who is in, on what, from where — which the
// trail itself can only answer by reading every login backwards.
func (s *AuditService) ActiveSessions(ctx context.Context) ([]AuditSession, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT se.id, se.user_id,
		       COALESCE(NULLIF(TRIM(COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')),''), u.email::text),
		       COALESCE((SELECT array_agg(r.role::text ORDER BY r.role::text)
		                   FROM user_roles r WHERE r.user_id = se.user_id), '{}'),
		       COALESCE(se.user_agent, ''), COALESCE(se.ip, ''),
		       se.created_at, se.last_activity_at
		FROM sessions se
		JOIN users u ON u.id = se.user_id
		WHERE se.revoked_at IS NULL AND se.expires_at > NOW()
		  AND se.last_activity_at > NOW() - make_interval(secs => $1)
		ORDER BY se.last_activity_at DESC
		LIMIT 200`, IdleTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditSession{}
	for rows.Next() {
		var a AuditSession
		var ua string
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.Roles, &ua, &a.IP,
			&a.CreatedAt, &a.LastActivityAt); err != nil {
			return nil, err
		}
		a.Device = audit.Device(ua)
		a.Network = audit.Network(a.IP)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditExportMaxRows caps one export. An unbounded export of a table that only
// grows is a request that eventually cannot be served; a person who needs more
// narrows the dates and exports twice.
const AuditExportMaxRows = 10000
