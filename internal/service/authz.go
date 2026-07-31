package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// authz.go centralises object-level access checks that must hold at the service
// layer. Route-level RBAC only proves "this user has role X"; it does not prove
// "this lecturer owns this course" or "this TA owns this assignment". Those
// object-level checks live here so every mutating path can enforce them
// consistently and IDOR bugs cannot creep back in per-endpoint.

// lecturerOwnsCourse reports whether actor teaches the given teaching course.
// LecturerOwnsCourse is the exported form for handlers that must gate a read
// before calling into a service method that has no actor of its own.
func (s *TeachingService) LecturerOwnsCourse(ctx context.Context, actor, tcID uuid.UUID) (bool, error) {
	return lecturerOwnsCourse(ctx, s.pool, actor, tcID)
}

func lecturerOwnsCourse(ctx context.Context, pool *pgxpool.Pool, actor, tcID uuid.UUID) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM teaching_lecturers WHERE teaching_course_id = $1 AND lecturer_id = $2)`,
		tcID, actor).Scan(&ok)
	return ok, err
}

// taHasApprovedAssignment reports whether the TA holds at least one approved
// assignment in the teaching course. The monthly submission workflow uses this
// to refuse status rows for arbitrary (TA × course) pairs.
func taHasApprovedAssignment(ctx context.Context, pool *pgxpool.Pool, taID, tcID uuid.UUID) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM ta_request_assignments a
			JOIN sections sec ON sec.id = a.section_id
			JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
			WHERE a.ta_id = $1 AND sec.teaching_course_id = $2)`,
		taID, tcID).Scan(&ok)
	return ok, err
}

// courseLecturerIDs returns every lecturer assigned to the teaching course —
// the notification fan-out target for submit/sign events.
func courseLecturerIDs(ctx context.Context, pool *pgxpool.Pool, tcID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx,
		`SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id = $1`, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// hasRole reports whether the actor holds any of the given role codes.
func hasRole(ctx context.Context, pool *pgxpool.Pool, actor uuid.UUID, roles ...string) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role::text = ANY($2))`,
		actor, roles).Scan(&ok)
	return ok, err
}

// isPrivileged reports whether the actor is an admin or staff member — the two
// roles allowed to manage any course regardless of ownership.
func isPrivileged(ctx context.Context, pool *pgxpool.Pool, actor uuid.UUID) (bool, error) {
	return hasRole(ctx, pool, actor, "admin", "staff")
}

// assertCourseManager allows the action when the actor is privileged
// (admin/staff) or teaches the course. Returns ErrForbidden otherwise.
func assertCourseManager(ctx context.Context, pool *pgxpool.Pool, actor, tcID uuid.UUID) error {
	_, err := courseAccess(ctx, pool, actor, tcID)
	return err
}

// courseAccess is assertCourseManager plus the answer to "which kind of
// manager". Both may act on the course, but not equally: admin/staff have full
// control, while an owning lecturer is restricted to the narrow set of edits
// described in teaching.go (they cannot add, rename or delete a section, and
// may fill in a missing timetable only once). Callers that enforce those limits
// need the distinction; the rest use assertCourseManager.
//
// Returns ErrForbidden for anyone who is neither.
func courseAccess(ctx context.Context, pool *pgxpool.Pool, actor, tcID uuid.UUID) (privileged bool, err error) {
	priv, err := isPrivileged(ctx, pool, actor)
	if err != nil {
		return false, err
	}
	if priv {
		return true, nil
	}
	owns, err := lecturerOwnsCourse(ctx, pool, actor, tcID)
	if err != nil {
		return false, err
	}
	if !owns {
		return false, ErrForbidden
	}
	return false, nil
}

// assignmentContext resolves the parent teaching course, section, request
// status and the owning TA for a work-log assignment. SectionID is required by
// the worklog validator to look up makeup schedules. ReimburseScope gates
// which activity types the worklog Generate/Upsert flow may create — the
// frontend already restricts the picker, but the server needs the same rule
// so a stale tab or scripted client can't POST an unauthorized activity.
//
// The Allow* flags are the finer-grained "what did the lecturer actually
// declare this TA doing" gate, derived from ta_workload_forms:
//   - undergrad: attendance_hrs → lecture, lab_hrs → lab,
//     check_work_hrs → review, ug_other_hrs → other
//   - grad (master/phd): help_teach_hrs → lecture+lab,
//     grade_hrs → review, other_hrs → other
//
// If no workload form exists yet (older data or partial setup) all flags
// default to true so existing flows don't break — the scope gate above is
// still enforced.
type assignmentContext struct {
	TeachingCourseID uuid.UUID
	SectionID        uuid.UUID
	TAID             uuid.UUID
	RequestStatus    string
	ReimburseScope   string
	Level            string
	AllowLecture     bool
	AllowLab         bool
	AllowReview      bool
	AllowOther       bool
	// Weekly hour caps per activity, derived from the workload form. Same
	// mapping as TAAssignment (undergrad: attendance/lab/check_work/ug_other;
	// grad: help_teach shared for lecture+lab, grade, other+prep). Zero when
	// HasWorkloadForm=false — callers must gate on HasWorkloadForm before
	// enforcing.
	WeeklyCapLecture       float64
	WeeklyCapLab           float64
	WeeklyCapReview        float64
	WeeklyCapOther         float64
	WeeklyLectureLabShared bool
	HasWorkloadForm        bool
	// WeeklyTotalHours is the sum of the declared weekly workload hours. Times
	// weeks-in-term it yields the TERM ceiling that bounds total logged hours.
	// Zero when HasWorkloadForm=false.
	WeeklyTotalHours float64
}

func loadAssignmentContext(ctx context.Context, pool *pgxpool.Pool, assignmentID uuid.UUID) (*assignmentContext, error) {
	var ac assignmentContext
	// LEFT JOIN ta_workload_forms so an assignment without a workload row
	// still returns; COALESCE ensures NULL numeric fields don't blow up the
	// comparison and get treated as "no hours declared" (which then falls
	// back to "allow all" below).
	var (
		help, prep, grade, other        float64
		attendance, check, ugOther, lab float64
		hasForm                         bool
	)
	err := pool.QueryRow(ctx, `
		SELECT sec.teaching_course_id, sec.id, a.ta_id, r.status::text,
		       r.reimburse_scope::text, a.level::text,
		       COALESCE(wf.help_teach_hrs, 0),
		       COALESCE(wf.prep_hrs, 0),
		       COALESCE(wf.grade_hrs, 0),
		       COALESCE(wf.other_hrs, 0),
		       COALESCE(wf.attendance_hrs, 0),
		       COALESCE(wf.check_work_hrs, 0),
		       COALESCE(wf.ug_other_hrs, 0),
		       COALESCE(wf.lab_hrs, 0),
		       (wf.id IS NOT NULL)
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN ta_requests r ON r.id = a.request_id
		LEFT JOIN ta_workload_forms wf ON wf.assignment_id = a.id
		WHERE a.id = $1`, assignmentID).Scan(
		&ac.TeachingCourseID, &ac.SectionID, &ac.TAID, &ac.RequestStatus,
		&ac.ReimburseScope, &ac.Level,
		&help, &prep, &grade, &other,
		&attendance, &check, &ugOther, &lab,
		&hasForm,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// No workload row → conservative default (don't lock the TA out of any
	// activity type). Once the lecturer fills in the form, the flags kick in.
	ac.HasWorkloadForm = hasForm
	if !hasForm {
		ac.AllowLecture, ac.AllowLab, ac.AllowReview, ac.AllowOther = true, true, true, true
		return &ac, nil
	}
	if ac.Level == "undergrad" {
		ac.AllowLecture = attendance > 0
		ac.AllowLab = lab > 0
		ac.AllowReview = check > 0
		// Undergrad "อื่นๆ" is a lecture-slot activity per the form's field
		// grouping; prep_hrs doesn't exist for undergrad so we only use
		// ug_other_hrs here.
		ac.AllowOther = ugOther > 0
		ac.WeeklyCapLecture = attendance
		ac.WeeklyCapLab = lab
		ac.WeeklyCapReview = check
		ac.WeeklyCapOther = ugOther
		ac.WeeklyLectureLabShared = false
		ac.WeeklyTotalHours = attendance + lab + check + ugOther
	} else { // master / phd → grad workload fields
		// help_teach = ช่วยสอน covers both lecture and lab sessions
		// depending on section schedule, so authorize both when > 0.
		ac.AllowLecture = help > 0
		ac.AllowLab = help > 0
		ac.AllowReview = grade > 0
		// Grad "อื่นๆ" and "เตรียมการสอน" both fall under activity=other
		// (prep happens off-site but is billable time); either being > 0
		// authorizes the TA to log "other".
		ac.AllowOther = other > 0 || prep > 0
		// help_teach is a combined cap: lecture + lab weekly total must stay
		// under it (not each independently). WeeklyLectureLabShared=true tells
		// enforcement to sum the two activities before comparing.
		ac.WeeklyCapLecture = help
		ac.WeeklyCapLab = help
		ac.WeeklyCapReview = grade
		ac.WeeklyCapOther = other + prep
		ac.WeeklyLectureLabShared = true
		// help_teach covers lecture+lab (one figure, not double-counted).
		ac.WeeklyTotalHours = help + grade + other + prep
	}
	return &ac, nil
}
