package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// ExportBatchService records the zip files staff generate via /exports/course/:id
// so the exports dashboard can list history and re-download without rebuilding.
type ExportBatchService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

type ExportBatch struct {
	ID                 uuid.UUID  `json:"id"`
	TeachingCourseID   uuid.UUID  `json:"teaching_course_id"`
	SubmissionPeriodID *uuid.UUID `json:"submission_period_id,omitempty"`
	FilePath           string     `json:"file_path"`
	FileName           string     `json:"file_name"`
	TACount            int        `json:"ta_count"`
	TotalBaht          float64    `json:"total_baht"`
	GeneratedAt        string     `json:"generated_at"`
	GeneratedBy        uuid.UUID  `json:"generated_by"`
	GeneratedByName    string     `json:"generated_by_name,omitempty"`
	// Months is the fiscal slice this ZIP covered, Gregorian "YYYY-MM". Empty
	// for batches recorded before the split existed — those were whole-term.
	Months []string `json:"months,omitempty"`
}

// Record persists a batch that has already been written to storage.
func (s *ExportBatchService) Record(ctx context.Context, actor uuid.UUID, in ExportBatch) (*ExportBatch, error) {
	in.ID = uuid.New()
	in.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	in.GeneratedBy = actor
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "export_batch.record",
			Entity: "teaching_course", EntityID: in.TeachingCourseID.String(), After: in},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO export_batches
				    (id, teaching_course_id, submission_period_id,
				     file_path, file_name, ta_count, total_baht,
				     generated_at, generated_by, months)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8::timestamptz,$9,$10)`,
				in.ID, in.TeachingCourseID, in.SubmissionPeriodID,
				in.FilePath, in.FileName, in.TACount, in.TotalBaht,
				in.GeneratedAt, in.GeneratedBy, in.Months)
			return err
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

// Get returns one batch by id, for BatchDownload to resolve which file to
// stream. FilePath is "" for a batch recorded before file retention existed,
// or one whose storage write failed at generation time (see CourseZip) —
// callers must handle that as "no file to re-download," not an error.
func (s *ExportBatchService) Get(ctx context.Context, id uuid.UUID) (*ExportBatch, error) {
	var b ExportBatch
	if err := s.pool.QueryRow(ctx, `
		SELECT id, teaching_course_id, submission_period_id, file_path, file_name
		FROM export_batches WHERE id = $1`, id).Scan(
		&b.ID, &b.TeachingCourseID, &b.SubmissionPeriodID, &b.FilePath, &b.FileName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &b, nil
}

// ListByCourse returns the batch history for a course, newest first.
func (s *ExportBatchService) ListByCourse(ctx context.Context, tcID uuid.UUID) ([]ExportBatch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.teaching_course_id, b.submission_period_id,
		       b.file_path, b.file_name, b.ta_count, b.total_baht,
		       TO_CHAR(b.generated_at,'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       b.generated_by, u.first_name||' '||u.last_name,
		       COALESCE(b.months, '{}')
		FROM export_batches b
		JOIN users u ON u.id = b.generated_by
		WHERE b.teaching_course_id = $1
		ORDER BY b.generated_at DESC`, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExportBatch{}
	for rows.Next() {
		var b ExportBatch
		if err := rows.Scan(&b.ID, &b.TeachingCourseID, &b.SubmissionPeriodID,
			&b.FilePath, &b.FileName, &b.TACount, &b.TotalBaht,
			&b.GeneratedAt, &b.GeneratedBy, &b.GeneratedByName, &b.Months); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// CourseSummary is one row on the exports dashboard: a course with its budget,
// used-baht, remaining, TA count, unsubmitted-months hint, and last export.
type CourseSummary struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	CourseCode       string    `json:"course_code"`
	CourseNameTH     string    `json:"course_name_th"`
	TermLabel        string    `json:"term_label"`
	PerCourseMaxBaht float64   `json:"per_course_max_baht"`
	UsedBaht         float64   `json:"used_baht"`
	RemainingBaht    float64   `json:"remaining_baht"`
	OverBudget       bool      `json:"over_budget"`
	TACount          int       `json:"ta_count"`
	PendingMonths    []string  `json:"pending_months,omitempty"` // labels of periods where no batch exists
	LastExportAt     *string   `json:"last_export_at,omitempty"`
	// UnreviewedMonths names periods that have lecturer-approved work but have
	// NOT passed staff review (step 3). Export refuses these, so the screen has
	// to name them — otherwise a download silently contains fewer TAs than the
	// course actually has, which is the failure mode staff would notice last.
	UnreviewedMonths []string `json:"unreviewed_months,omitempty"`

	// HasAppointmentOrder is whether a printed appointment order covers this
	// course (see AppointedSQL). Until it does, the TA's work is not official and
	// there is nothing to export against.
	HasAppointmentOrder bool `json:"has_appointment_order"`
	// ReviewComplete is whether step 3 is finished for this course: at least one
	// month signed off, and no month left waiting. "No months at all" is NOT
	// complete — a course nobody has logged work on has nothing to export.
	ReviewComplete bool `json:"review_complete"`
	// ExportEligible is the rule itself, decided here rather than in the UI so the
	// two screens cannot drift apart on what "ready to export" means.
	ExportEligible bool `json:"export_eligible"`

	// LecturerNames is who to talk to about this course. The payout list is a
	// list of COURSES, and the person an officer chases when a row is stuck is
	// the lecturer — not the TAs, whose names were taking the space and telling
	// nobody anything they could act on.
	LecturerNames string `json:"lecturer_names"`

	// Rounds is this course's standing in EACH half of a term that crosses the
	// 30 กันยายน budget year, in round order. Empty for a term that does not
	// cross — there is only one document, and LastExportAt already describes it.
	//
	// (13/08/2026) This replaced a single RoundTwoOutstanding boolean, which
	// collapsed three states the payouts screen has to tell apart: round 2
	// exported, round 2 still owed, and no round-2 work in this course at all.
	// The last two both read false, so a course that was genuinely finished and
	// one still owing a second document against next year's appropriation were
	// indistinguishable — which is exactly the confusion the flag was added to
	// resolve.
	Rounds []CourseRoundStatus `json:"rounds,omitempty"`
}

// CourseRoundStatus is one course's standing in one fiscal round: whether the
// round has anything to claim for this course, and whether that claim has been
// issued. Both are needed — "not exported" means nothing without "has work",
// since every course in a crossing term exists in both rounds' month ranges
// whether or not anyone taught in them.
type CourseRoundStatus struct {
	Round int `json:"round"` // 1 = closes 30 ก.ย., 2 = opens 1 ต.ค.
	// Billable is whether this course has demonstrable work in the round's
	// months (roundBillableSQL). False means there is no document to owe.
	Billable bool `json:"billable"`
	// Exported is whether an export_batches row covers any of the round's
	// months (roundExportedSQL).
	Exported bool `json:"exported"`
}

// PayoutDashboard is the exports dashboard's whole payload: the course rows
// plus where the budget year cuts this term.
//
// The split used to be computed inside DashboardSummary and thrown away, and
// the screen received only per-course booleans. That left it unable to tell a
// term that never crosses the boundary from one that crosses but has no
// round-2 work — both produced identical rows — so it could not decide whether
// to speak about "รอบ" at all. Sending the split says which of the two it is.
type PayoutDashboard struct {
	Courses []CourseSummary `json:"courses"`
	Split   FiscalSplit     `json:"fiscal_split"`
}

// DashboardSummary aggregates budget + submission status per teaching_course
// filtered by term. Heavy query — used by the staff dashboard page only.
func (s *ExportBatchService) DashboardSummary(ctx context.Context, budget *BudgetService, export *ExportService, termID uuid.UUID) (*PayoutDashboard, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th,
		       COALESCE((SELECT string_agg(u.first_name || ' ' || u.last_name, ', '
		                                   ORDER BY tl.is_primary DESC, u.first_name)
		                 FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		                 WHERE tl.teaching_course_id = tc.id), ''),
		       t.academic_year || ' ' ||
		           CASE t.semester WHEN 1 THEN 'ภาคต้น' WHEN 2 THEN 'ภาคปลาย' ELSE 'ภาคฤดูร้อน' END,
		       (SELECT COUNT(DISTINCT a.ta_id)
		          FROM ta_request_assignments a
		          JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
		          WHERE r.teaching_course_id = tc.id) AS ta_count,
		       (SELECT TO_CHAR(MAX(generated_at),'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
		          FROM export_batches WHERE teaching_course_id = tc.id),
		       `+CourseAppointedSQL("tc.id")+`,
		       -- At least one month of this course has been signed off in step 3.
		       -- Paired with UnreviewedMonths below to mean "review finished":
		       -- this half rules out a course with no work at all, that half rules
		       -- out one with work still waiting.
		       EXISTS (
		           SELECT 1 FROM submission_period_status st
		            WHERE st.teaching_course_id = tc.id
		              AND st.status IN ('staff_reviewed','exported','finance_sent')
		       )
		FROM teaching_courses tc
		JOIN academic_terms t ON t.id = tc.term_id
		WHERE ($1::uuid IS NULL OR tc.term_id = $1)
		ORDER BY tc.code`, uuid.NullUUID{UUID: termID, Valid: termID != uuid.Nil})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CourseSummary{}
	// anySignedOff is scanned separately from ReviewComplete: completeness also
	// needs UnreviewedMonths, which is only known after the query below.
	anySignedOff := map[uuid.UUID]bool{}
	for rows.Next() {
		var s CourseSummary
		var signedOff bool
		if err := rows.Scan(&s.TeachingCourseID, &s.CourseCode, &s.CourseNameTH,
			&s.LecturerNames,
			&s.TermLabel, &s.TACount, &s.LastExportAt,
			&s.HasAppointmentOrder, &signedOff); err != nil {
			return nil, err
		}
		anySignedOff[s.TeachingCourseID] = signedOff
		out = append(out, s)
	}
	// Compute budget for each (heavy — done sequentially to avoid pool
	// contention). Callers can cache the result at HTTP layer if needed.
	for i := range out {
		snap, err := budget.Compute(ctx, out[i].TeachingCourseID)
		if err != nil {
			continue
		}
		out[i].PerCourseMaxBaht = snap.PerCourseMaxBaht
		out[i].UsedBaht = snap.UsedBaht
		out[i].RemainingBaht = snap.RemainingBaht
		out[i].OverBudget = snap.OverBudget
		// The spend figure comes from the settlement, not from BudgetSnapshot's
		// own sum: that one prices raw work_logs at face value, with no merged
		// sittings, no B2 deduction and no คาบ cutoff, so the list quoted a
		// course at 24,360 while its own preview said 14,860.
		if export != nil {
			if st, serr := export.SettleCourse(ctx, out[i].TeachingCourseID); serr == nil {
				out[i].UsedBaht = st.EarnedBaht()
				out[i].RemainingBaht = round2(snap.PerCourseMaxBaht - out[i].UsedBaht)
				out[i].OverBudget = snap.PerCourseMaxBaht > 0 && out[i].UsedBaht > snap.PerCourseMaxBaht+0.01
			}
		}
	}
	// Pending months: for each course, list periods (of its term) with no batch yet.
	pendingRows, err := s.pool.Query(ctx, `
		SELECT tc.id, sp.label
		FROM teaching_courses tc
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		LEFT JOIN export_batches b
		    ON b.teaching_course_id = tc.id AND b.submission_period_id = sp.id
		WHERE ($1::uuid IS NULL OR tc.term_id = $1) AND b.id IS NULL AND sp.is_closed = FALSE
		ORDER BY sp.due_date`,
		uuid.NullUUID{UUID: termID, Valid: termID != uuid.Nil})
	if err == nil {
		pending := map[uuid.UUID][]string{}
		for pendingRows.Next() {
			var tc uuid.UUID
			var label string
			if err := pendingRows.Scan(&tc, &label); err == nil {
				pending[tc] = append(pending[tc], label)
			}
		}
		pendingRows.Close()
		for i := range out {
			out[i].PendingMonths = pending[out[i].TeachingCourseID]
		}
	}

	// Months blocked by step 3. One query for the whole term rather than per
	// course — this runs on the exports dashboard, which already pays for a
	// budget computation per row.
	unreviewedRows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tc.id, sp.label, sp.due_date
		FROM teaching_courses tc
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		JOIN sections sec          ON sec.teaching_course_id = tc.id
		JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		JOIN ta_requests r         ON r.id = a.request_id AND r.status = 'approved'
		JOIN work_logs wl          ON wl.assignment_id = a.id
		                          AND to_char(wl.work_date, 'MM') = RIGHT(sp.year_month, 2)
		                          AND wl.status = 'approved'
		LEFT JOIN submission_period_status st
		       ON st.submission_period_id = sp.id
		      AND st.ta_id = a.ta_id
		      AND st.teaching_course_id = tc.id
		WHERE ($1::uuid IS NULL OR tc.term_id = $1)
		  AND COALESCE(st.status, 'pending') = 'pending'
		  -- Same appointment gate as the review queue, and it MUST be here too.
		  -- A TA without a printed order cannot be reviewed (they are not in the
		  -- queue), so counting their month as "unreviewed" would hold the course
		  -- short of ReviewComplete forever — a deadlock where the screen asks for
		  -- a sign-off that no screen offers.
		  AND `+AppointedSQL("tc.id", "a.ta_id")+`
		  -- Grad-special no longer logs work_logs at all (see export_gate.go) and
		  -- is excluded from ListReviewQueue, so staff have no screen to clear
		  -- this status on. Leftover approved rows from before that change must
		  -- not count as "unreviewed", or the course would never reach
		  -- ReviewComplete.
		  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		ORDER BY sp.due_date`,
		uuid.NullUUID{UUID: termID, Valid: termID != uuid.Nil})
	if err == nil {
		unreviewed := map[uuid.UUID][]string{}
		for unreviewedRows.Next() {
			var tc uuid.UUID
			var label string
			var due time.Time
			if err := unreviewedRows.Scan(&tc, &label, &due); err == nil {
				unreviewed[tc] = append(unreviewed[tc], label)
			}
		}
		unreviewedRows.Close()
		for i := range out {
			out[i].UnreviewedMonths = unreviewed[out[i].TeachingCourseID]
		}
	}

	// Decide eligibility last, once both inputs exist. Both conditions, not
	// either: the order makes the work official, the review makes the amounts
	// final, and an export missing either produces a package the finance office
	// sends back.
	for i := range out {
		out[i].ReviewComplete = anySignedOff[out[i].TeachingCourseID] &&
			len(out[i].UnreviewedMonths) == 0
		out[i].ExportEligible = out[i].HasAppointmentOrder && out[i].ReviewComplete
	}

	// Per-round standing: only worth querying when the TERM actually crosses the
	// budget-year boundary — the ordinary case has one document and pays nothing
	// extra here. termID can be uuid.Nil ("every term"), which has no single
	// fiscal split to compute against, so it is skipped there too.
	var split FiscalSplit
	if export != nil && termID != uuid.Nil {
		if all, merr := export.TermMonths(ctx, termID); merr == nil {
			if sp, serr := fiscalSplit(all); serr == nil {
				split = sp
			}
		}
	}
	if split.Crosses && len(split.After) > 0 {
		for _, r := range []struct {
			num    int
			months []string
		}{{1, split.Before}, {2, split.After}} {
			if len(r.months) == 0 {
				continue
			}
			st, err := s.roundStanding(ctx, termID, r.num, r.months)
			if err != nil {
				return nil, err
			}
			for i := range out {
				// Default rather than the map's zero value: a course the round
				// query did not return would otherwise get Round: 0, which the
				// screen would render as an unlabelled slot.
				got, ok := st[out[i].TeachingCourseID]
				if !ok {
					got = CourseRoundStatus{Round: r.num}
				}
				out[i].Rounds = append(out[i].Rounds, got)
			}
		}
	}
	return &PayoutDashboard{Courses: out, Split: split}, nil
}

// roundStanding answers billable/exported for every course in the term for ONE
// round, keyed by course. Asked per round rather than per course so a term of
// 127 courses costs two queries rather than 254.
func (s *ExportBatchService) roundStanding(
	ctx context.Context, termID uuid.UUID, round int, months []string,
) (map[uuid.UUID]CourseRoundStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, y.billable, z.exported
		FROM teaching_courses tc,
		     LATERAL (SELECT `+roundBillableSQL("$2")+` AS billable) y,
		     LATERAL (SELECT `+roundExportedSQL("$2")+` AS exported) z
		WHERE tc.term_id = $1`, termID, months)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]CourseRoundStatus{}
	for rows.Next() {
		var id uuid.UUID
		st := CourseRoundStatus{Round: round}
		if err := rows.Scan(&id, &st.Billable, &st.Exported); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}
