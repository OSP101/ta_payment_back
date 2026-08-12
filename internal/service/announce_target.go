package service

// announce_target.go answers one question: WHO is this announcement for?
//
// There is exactly one implementation (resolveAudienceSQL) and four callers —
// the preview count staff see before publishing, the in-app fanout, the email
// delivery, and the feed's visibility check. Any second implementation would
// eventually disagree with the others, and the failure mode is the worst kind:
// somebody is told they were sent a notice they cannot find.
//
// Shape of a rule:
//
//	base    = roles ∪ courses ∪ named people      (a union: "any of these")
//	final   = base ∩ every narrowing filter        (an intersection: "and also")
//
// So "ผู้ช่วยสอน" + "ยังไม่ส่งเอกสาร" means TAs who are missing documents, not
// TAs plus everyone missing documents. Officers describe targets that way, and
// the preview lets them check it rather than trust it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// AudienceRule is the composer's targeting choice.
type AudienceRule struct {
	// Roles is the coarse group. Empty AND no courses AND no people means
	// "everyone with an account".
	Roles []string `json:"roles"`
	// CourseIDs pulls in everyone attached to a teaching course: its lecturers
	// and the TAs appointed to it.
	CourseIDs []uuid.UUID `json:"course_ids"`
	// UserIDs are named people, for an announcement aimed at one person.
	UserIDs []uuid.UUID `json:"user_ids"`
	// Filters narrow whatever the above selected. Closed set — see announceFilters.
	Filters []string `json:"filters"`
	// TermID scopes course and condition filters. nil = the active term.
	TermID *uuid.UUID `json:"term_id"`
}

// IsEveryone reports the "no base selected" case, which means all accounts.
func (r AudienceRule) IsEveryone() bool {
	return len(r.Roles) == 0 && len(r.CourseIDs) == 0 && len(r.UserIDs) == 0
}

// announceFilter is one narrowing condition: a label for the composer and the
// SQL that decides membership. `u` is the users row in the outer query.
type announceFilter struct {
	Label string
	// SQL is an EXISTS/NOT EXISTS predicate over `u.id`, with $1 bound to the
	// term id. Written as a predicate rather than a join so filters compose by
	// simple AND without multiplying rows.
	SQL string
}

// announceFilters is the closed set. A name outside it is refused at save time:
// an unknown filter that silently matched everyone would send an announcement
// meant for eight people to four hundred.
var announceFilters = map[string]announceFilter{
	"ta_missing_documents": {
		Label: "ยังส่งเอกสารไม่ครบ",
		// Same three documents the review queue counts (see ProfileDocsInSQL),
		// so "ยังไม่ครบ" here and on the review screen mean the same thing.
		SQL: `EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'ta')
		      AND ` + `(
		        SELECT COUNT(*) FROM ta_documents d
		         WHERE d.user_id = u.id
		           AND d.kind IN ('national_id','bank_book','creditor_form')
		           AND d.superseded_at IS NULL
		      ) < 3`,
	},
	"ta_missing_schedule": {
		Label: "ยังไม่กรอกตารางเรียนของตัวเอง",
		// The same emptiness that blocks work-log writes, so the notice reaches
		// exactly the people the system is currently refusing.
		SQL: `EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'ta')
		      AND NOT EXISTS (
		        SELECT 1 FROM ta_class_schedules cs
		         WHERE cs.user_id = u.id AND cs.term_id = $1
		      )`,
	},
	"ta_no_assignment": {
		Label: "ยังไม่ได้รับมอบหมายวิชา",
		SQL: `EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'ta')
		      AND NOT EXISTS (
		        SELECT 1 FROM ta_request_assignments a
		          JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		          JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		         WHERE a.ta_id = u.id AND a.state <> 'dropped' AND tc.term_id = $1
		      )`,
	},
	"lecturer_no_request": {
		Label: "อาจารย์ที่ยังไม่ได้ขอผู้ช่วยสอน",
		SQL: `EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id AND ur.role = 'lecturer')
		      AND EXISTS (
		        SELECT 1 FROM teaching_lecturers tl
		          JOIN teaching_courses tc ON tc.id = tl.teaching_course_id
		         WHERE tl.lecturer_id = u.id AND tc.term_id = $1
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM ta_requests r
		          JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		         WHERE r.lecturer_id = u.id AND tc.term_id = $1
		      )`,
	},
	"lecturer_pending_worklog": {
		Label: "อาจารย์ที่มีบันทึกเวลารออนุมัติ",
		SQL: `EXISTS (
		        SELECT 1
		          FROM work_logs wl
		          JOIN ta_request_assignments a ON a.id = wl.assignment_id
		          JOIN sections sec ON sec.id = a.section_id
		          JOIN ta_requests r  ON r.id = a.request_id AND r.status = 'approved'
		          JOIN teaching_courses tc ON tc.id = r.teaching_course_id
		         WHERE wl.status = 'submitted' AND tc.term_id = $1 AND r.lecturer_id = u.id
		           -- Grad-special no longer logs work_logs and never appears on
		           -- the lecturer's approval screen (ListPending excludes it) —
		           -- leftover 'submitted' rows must not tell a lecturer they have
		           -- something to approve when their own queue shows nothing.
		           AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		      )`,
	},
	"course_missing_schedule": {
		Label: "อาจารย์ของวิชาที่ยังไม่ระบุตารางสอน",
		SQL: `EXISTS (
		        SELECT 1
		          FROM teaching_lecturers tl
		          JOIN teaching_courses tc ON tc.id = tl.teaching_course_id
		          JOIN sections sec ON sec.teaching_course_id = tc.id
		         WHERE tl.lecturer_id = u.id AND tc.term_id = $1
		           AND NOT EXISTS (
		             SELECT 1 FROM section_schedules ss WHERE ss.section_id = sec.id
		           )
		      )`,
	},
}

// AnnounceFilterOption is what the composer lists.
type AnnounceFilterOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AnnounceFilterOptions returns the closed set in a stable order, so the
// composer never invents a filter the resolver cannot honour.
func AnnounceFilterOptions() []AnnounceFilterOption {
	order := []string{
		"ta_missing_documents", "ta_missing_schedule", "ta_no_assignment",
		"lecturer_no_request", "lecturer_pending_worklog", "course_missing_schedule",
	}
	out := make([]AnnounceFilterOption, 0, len(order))
	for _, k := range order {
		out = append(out, AnnounceFilterOption{Value: k, Label: announceFilters[k].Label})
	}
	return out
}

// validateRule checks everything that could make a rule mean something other
// than it says, before it is stored.
func validateRule(r AudienceRule) error {
	for _, role := range r.Roles {
		if !validAudienceRoles[role] {
			return fmt.Errorf("บทบาท %q ไม่ถูกต้อง", role)
		}
	}
	for _, f := range r.Filters {
		if _, ok := announceFilters[f]; !ok {
			return fmt.Errorf("เงื่อนไข %q ไม่ถูกต้อง", f)
		}
	}
	return nil
}

// resolveAudienceSQL builds the query that lists the target user ids.
//
// Returns SQL plus its args. $1 is always the term id, so every filter can use
// it without the caller tracking positions.
func resolveAudienceSQL(r AudienceRule, termID uuid.UUID) (string, []any) {
	// $1 is the term id, bound ONLY when a selected filter actually mentions
	// it — "ยังส่งเอกสารไม่ครบ" has no term in its query, for instance.
	// Passing an unused parameter makes Postgres refuse the query outright
	// ("could not determine data type of parameter $1"), because nothing in
	// the text tells it what type to expect.
	args := []any{}
	usesTerm := false
	for _, f := range r.Filters {
		if def, ok := announceFilters[f]; ok && strings.Contains(def.SQL, "$1") {
			usesTerm = true
			break
		}
	}
	if usesTerm {
		args = append(args, termID)
	}
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	var base []string
	if len(r.Roles) > 0 {
		base = append(base, `EXISTS (SELECT 1 FROM user_roles ur
		                              WHERE ur.user_id = u.id AND ur.role::text = ANY(`+add(r.Roles)+`))`)
	}
	if len(r.CourseIDs) > 0 {
		ids := add(r.CourseIDs)
		// Everyone attached to the course: the lecturers who teach it and the
		// TAs actually appointed to it (a dropped assignment is not on it).
		base = append(base, `(
			EXISTS (SELECT 1 FROM teaching_lecturers tl
			         WHERE tl.lecturer_id = u.id AND tl.teaching_course_id = ANY(`+ids+`))
			OR EXISTS (SELECT 1 FROM ta_request_assignments a
			             JOIN ta_requests r2 ON r2.id = a.request_id AND r2.status = 'approved'
			            WHERE a.ta_id = u.id AND a.state <> 'dropped'
			              AND r2.teaching_course_id = ANY(`+ids+`))
		)`)
	}
	if len(r.UserIDs) > 0 {
		base = append(base, `u.id = ANY(`+add(r.UserIDs)+`)`)
	}

	where := []string{"u.is_active", "u.deleted_at IS NULL"}
	if len(base) > 0 {
		where = append(where, "("+strings.Join(base, " OR ")+")")
	}
	for _, f := range r.Filters {
		// Validated at save time; a name that reached here without a query
		// would be a bug, and matching nobody is the safe direction.
		if def, ok := announceFilters[f]; ok {
			where = append(where, "("+def.SQL+")")
		}
	}

	return `SELECT u.id, u.email,
	               COALESCE(NULLIF(u.title,'') , '') || u.first_name || ' ' || u.last_name AS name
	          FROM users u
	         WHERE ` + strings.Join(where, "\n           AND ") + `
	         ORDER BY u.first_name, u.last_name`, args
}

// AudienceMember is one resolved recipient.
type AudienceMember struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
}

// activeTermID is the term a rule is evaluated against when none was frozen.
func (s *AnnounceService) activeTermID(ctx context.Context, given *uuid.UUID) (uuid.UUID, error) {
	if given != nil && *given != uuid.Nil {
		return *given, nil
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM academic_terms
		 ORDER BY is_active DESC, academic_year DESC, semester DESC LIMIT 1`).Scan(&id)
	if err != nil {
		// A fresh install has no terms. Course/condition filters then match
		// nobody, which is correct: there is nothing to be behind on yet.
		return uuid.Nil, nil
	}
	return id, nil
}

// ResolveAudience lists the people a rule selects, right now.
func (s *AnnounceService) ResolveAudience(ctx context.Context, r AudienceRule) ([]AudienceMember, error) {
	if err := validateRule(r); err != nil {
		return nil, Invalid(err.Error())
	}
	termID, err := s.activeTermID(ctx, r.TermID)
	if err != nil {
		return nil, err
	}
	q, args := resolveAudienceSQL(r, termID)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AudienceMember{}
	for rows.Next() {
		var m AudienceMember
		if err := rows.Scan(&m.ID, &m.Email, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AudiencePreview is what the composer shows before anything is sent.
type AudiencePreview struct {
	Total int              `json:"total"`
	Names []AudienceMember `json:"names"`
	// Everyone reports that the rule selected no base group, so the answer is
	// "every account". Shown as a warning rather than a number, because 400
	// people is not a target an officer should reach by leaving fields blank.
	Everyone bool `json:"everyone"`
}

// PreviewAudience answers "who will get this" with a count and the first names,
// so the officer confirms the target instead of trusting it.
func (s *AnnounceService) PreviewAudience(ctx context.Context, r AudienceRule) (*AudiencePreview, error) {
	members, err := s.ResolveAudience(ctx, r)
	if err != nil {
		return nil, err
	}
	const sample = 25
	out := &AudiencePreview{Total: len(members), Everyone: r.IsEveryone() && len(r.Filters) == 0}
	if len(members) > sample {
		out.Names = members[:sample]
	} else {
		out.Names = members
	}
	return out, nil
}

// materializeAudience writes the resolved audience into the recipient ledger.
//
// Called once, when the announcement goes live. Freezing it is deliberate: the
// feed and the email then agree forever, and a TA who uploads their documents
// tomorrow does not lose an announcement they were already told about.
func (s *AnnounceService) materializeAudience(ctx context.Context, id uuid.UUID) error {
	var (
		roles     []string
		courseIDs []uuid.UUID
		userIDs   []uuid.UUID
		filters   []string
		termID    *uuid.UUID
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT audience, target_course_ids, target_user_ids, target_filters, target_term_id
		  FROM announcements WHERE id = $1`, id).Scan(
		&roles, &courseIDs, &userIDs, &filters, &termID); err != nil {
		return err
	}
	rule := AudienceRule{Roles: roles, CourseIDs: courseIDs, UserIDs: userIDs, Filters: filters, TermID: termID}

	term, err := s.activeTermID(ctx, termID)
	if err != nil {
		return err
	}
	// Freeze the term the rule was evaluated against, so a re-send later in the
	// year cannot quietly retarget a different cohort.
	if termID == nil && term != uuid.Nil {
		_, _ = s.pool.Exec(ctx, `UPDATE announcements SET target_term_id=$2 WHERE id=$1 AND target_term_id IS NULL`, id, term)
	}

	members, err := s.ResolveAudience(ctx, rule)
	if err != nil {
		return err
	}
	for _, m := range members {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO announcement_recipients (announcement_id, email, user_id)
			VALUES ($1, lower($2), $3)
			ON CONFLICT (announcement_id, lower(email)) DO NOTHING`,
			id, m.Email, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// ErrNoAudience is returned when a rule selects nobody. Publishing to an empty
// audience is nearly always a mistake in the rule, and silence is exactly what
// it looks like from the officer's side.
var ErrNoAudience = errors.New("ยังไม่มีผู้รับตามเงื่อนไขที่เลือก กรุณาตรวจสอบกลุ่มเป้าหมายอีกครั้ง")

// derefSlice reads an optional slice field: nil (absent from the payload)
// yields nil, which every caller treats as "not specified".
func derefSlice[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

// nonNil turns a nil slice into an empty one. The targeting columns are NOT
// NULL with a '{}' default, and pgx sends a nil Go slice as SQL NULL — which
// the column rejects rather than defaulting.
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
