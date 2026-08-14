package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// DocumentProgressService tracks the off-system physical document journey for a
// term's payout documents: the signature round + routing to finance/treasury.
// The board only becomes actionable once every course in the ROUND is exported.
// See migration 0031 for the stage meanings.
//
// A term that crosses the 30 กันยายน budget-year boundary (see term_months.go's
// FiscalSplit) now has TWO independent journeys — fiscal_round 1 for the
// closing budget year's document, 2 for the new one (12/08/2026, migration
// 0082) — because the two are separate physical folders that get signed and
// routed on their own schedule; a term whose ปะหน้าจ่ายตรง never crosses that
// boundary has exactly one, and every existing row is that round automatically
// (DEFAULT 1). Round 2 is surfaced ONLY when it actually has payable content —
// see resolveFiscalRounds — so a term that crosses the boundary but has
// nothing left to claim in October never shows "รอบ" language at all.
type DocumentProgressService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
	// export resolves the term's certifier and its month calendar (TermMonths,
	// for the fiscal-round split) — the one signer on the checklist who is an
	// admin_officers row rather than a system user, plus the one place the
	// budget-year boundary is computed from.
	export *ExportService
}

// CourseRef names an un-exported course still blocking the round.
type CourseRef struct {
	Code   string `json:"code"`
	NameTH string `json:"name_th"`
}

// TermProgress is one ROUND's progress record + export readiness. A
// non-crossing term (or a crossing one whose round 2 has no content) has
// exactly one of these; a crossing term with round-2 content has two,
// advancing independently of each other.
type TermProgress struct {
	TermID uuid.UUID `json:"term_id"`
	// Round is 1 or 2 — see the package doc comment. RoundLabel is "" for a
	// single-round term (no fiscal split to name) and something like
	// "รอบ 1 · งบ 2569 (มิ.ย.–ก.ย.)" once a term has two.
	Round      int    `json:"round"`
	RoundLabel string `json:"round_label,omitempty"`

	// Export readiness — signing can only start once ALL courses that have
	// billable work THIS ROUND are exported. TotalCourses counts courses with
	// >=1 approved TA assignment (round 1) or with demonstrable round-2
	// billable content (round 2) — see exportReadiness.
	TotalCourses      int         `json:"total_courses"`
	ExportedCourses   int         `json:"exported_courses"`
	AllExported       bool        `json:"all_exported"`
	UnexportedCourses []CourseRef `json:"unexported_courses"`

	Stage             int     `json:"stage"` // 0..5
	TASignedAt        *string `json:"ta_signed_at,omitempty"`
	LecturerSignedAt  *string `json:"lecturer_signed_at,omitempty"`
	CertifierSignedAt *string `json:"certifier_signed_at,omitempty"`
	SentFinanceAt     *string `json:"sent_finance_at,omitempty"`
	SentTreasuryAt    *string `json:"sent_treasury_at,omitempty"`
	Note              *string `json:"note,omitempty"`
	UpdatedByName     *string `json:"updated_by_name,omitempty"`
	UpdatedAt         *string `json:"updated_at,omitempty"`

	// NextStage is the only stage the officer may advance to, and CanAdvance
	// says whether the signatures for it are in place. Server-derived on purpose:
	// the stepper reads these instead of working out for itself which circle to
	// enable, so the button can never offer a step SetStage would refuse.
	NextStage  int  `json:"next_stage"`
	CanAdvance bool `json:"can_advance"`
	// SignersMissing is who is still holding up the CURRENT stage, so the board
	// can say the names rather than just "ยังไม่ครบ".
	SignersMissing []string `json:"signers_missing,omitempty"`
	// CurrentRole is the role signing at the current stage ("" for 4-5, which
	// nobody signs). The detail list below the stepper shows only these people.
	CurrentRole string `json:"current_role,omitempty"`
}

// TermProgressOverview is what the board actually asks for: every round this
// term has. len(Rounds) is 1 for the ordinary case and 2 once a crossing
// term's second budget year has something to claim — the frontend renders
// tabs only when there is more than one, so a single-round term's screen is
// pixel-identical to before this feature existed.
type TermProgressOverview struct {
	TermID uuid.UUID      `json:"term_id"`
	Rounds []TermProgress `json:"rounds"`
}

// courseHasTA is the shared predicate for "this course has documents to sign"
// — round 1's course filter, unchanged since before the fiscal-round split.
const courseHasTA = `EXISTS (
	SELECT 1 FROM ta_request_assignments a
	JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
	JOIN sections s ON s.id = a.section_id
	WHERE s.teaching_course_id = tc.id)`

// roundBillableSQL is a round's course filter: courseHasTA alone would catch
// every course in the term (the roster is fixed at appointment, independent
// of any month), so a round additionally requires DEMONSTRABLE billable
// content in that round's own months — otherwise every course with an ordinary
// full appointment would appear to owe a signature it has no document for.
// param must be the round's Gregorian "YYYY-MM" months as a text[] arg.
//
// Written for round 2 and named for it; generalised (13/08/2026) when the
// payouts list needed the same question asked of round 1, to show both halves
// of a crossing term side by side. Nothing in the body was round-specific.
//
// grad-special (เหมาจ่าย) has no work_logs at all — its lump is apportioned by
// schedule share across whichever months are printed (gradLumpShare) — so any
// grad-special assignment counts as content for whichever round is asked about;
// there is no คาบ to check a month against.
func roundBillableSQL(param string) string {
	return `(
		EXISTS (
		    SELECT 1 FROM work_logs wl
		    JOIN ta_request_assignments a2 ON a2.id = wl.assignment_id
		    JOIN sections sec2 ON sec2.id = a2.section_id
		    WHERE sec2.teaching_course_id = tc.id AND wl.status = 'approved'
		      AND to_char(wl.work_date,'YYYY-MM') = ANY(` + param + `::text[])
		)
		OR EXISTS (
		    SELECT 1 FROM ta_request_assignments a3
		    JOIN ta_requests r3 ON r3.id = a3.request_id AND r3.status = 'approved'
		    JOIN sections sec3 ON sec3.id = a3.section_id AND sec3.track = 'special'
		    JOIN users u3 ON u3.id = a3.ta_id
		    WHERE sec3.teaching_course_id = tc.id AND u3.study_level::text IN ('master','phd')
		)
	)`
}

// roundExportedSQL is "has this course's document for these months actually
// been issued" — read from the export_batches ledger rather than from the
// one-shot teaching_courses.exported_at flag, which fires on the FIRST ZIP a
// course ever gets and so cannot tell one round from another.
// param must be the round's Gregorian "YYYY-MM" months as a text[] arg.
//
// `months && $1` is NULL, not false, for a row whose months is NULL — which
// used to be every whole-term export, because the ZIP handler stored the
// absent ?months= parameter verbatim. Migration 0083 backfilled those rows and
// the handler now resolves the selection first (ResolveCourseMonths), so a
// surviving NULL means only "this term has no submission periods to
// enumerate", for which false is the right answer.
func roundExportedSQL(param string) string {
	return `EXISTS (
		SELECT 1 FROM export_batches eb
		WHERE eb.teaching_course_id = tc.id AND eb.months && ` + param + `::text[]
	)`
}

// exportReadiness returns (total courses-with-round-content, exported count,
// unexported list) for ONE round. Round 1 is byte-for-byte the original
// whole-term query (courseHasTA + tc.exported_at) — a non-crossing term's
// only round behaves exactly as this board always has. Round 2 uses the
// round2* predicates above and needs gregMonths (round 2's Gregorian months).
func (s *DocumentProgressService) exportReadiness(
	ctx context.Context, termID uuid.UUID, round int, gregMonths []string,
) (total, exported int, unexported []CourseRef, err error) {
	if round == 1 {
		err = s.pool.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE has_ta),
			       COUNT(*) FILTER (WHERE has_ta AND tc.exported_at IS NOT NULL)
			FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
			WHERE tc.term_id = $1`, termID).Scan(&total, &exported)
		if err != nil {
			return
		}
		rows, rErr := s.pool.Query(ctx, `
			SELECT tc.code, tc.name_th
			FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
			WHERE tc.term_id = $1 AND x.has_ta AND tc.exported_at IS NULL
			ORDER BY tc.code`, termID)
		if rErr != nil {
			err = rErr
			return
		}
		defer rows.Close()
		unexported = []CourseRef{}
		for rows.Next() {
			var c CourseRef
			if err = rows.Scan(&c.Code, &c.NameTH); err != nil {
				return
			}
			unexported = append(unexported, c)
		}
		err = rows.Err()
		return
	}

	unexported = []CourseRef{}
	if len(gregMonths) == 0 {
		return 0, 0, unexported, nil
	}
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE has_ta AND billable),
		       COUNT(*) FILTER (WHERE has_ta AND billable AND exported)
		FROM teaching_courses tc,
		     LATERAL (SELECT `+courseHasTA+` AS has_ta) x,
		     LATERAL (SELECT `+roundBillableSQL("$2")+` AS billable) y,
		     LATERAL (SELECT `+roundExportedSQL("$2")+` AS exported) z
		WHERE tc.term_id = $1`, termID, gregMonths).Scan(&total, &exported)
	if err != nil {
		return
	}
	rows, rErr := s.pool.Query(ctx, `
		SELECT tc.code, tc.name_th
		FROM teaching_courses tc,
		     LATERAL (SELECT `+courseHasTA+` AS has_ta) x,
		     LATERAL (SELECT `+roundBillableSQL("$2")+` AS billable) y,
		     LATERAL (SELECT `+roundExportedSQL("$2")+` AS exported) z
		WHERE tc.term_id = $1 AND has_ta AND billable AND NOT exported
		ORDER BY tc.code`, termID, gregMonths)
	if rErr != nil {
		err = rErr
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c CourseRef
		if err = rows.Scan(&c.Code, &c.NameTH); err != nil {
			return
		}
		unexported = append(unexported, c)
	}
	err = rows.Err()
	return
}

/* -------------------------------------------------------------------------- */
/* Fiscal round resolution                                                    */
/* -------------------------------------------------------------------------- */

var fiscalRoundMonthAbbrev = [...]string{
	"ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.",
	"ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค.",
}

// fiscalRoundLabel renders "รอบ 1 · งบ 2569 (มิ.ย.–ก.ย.)" from a round number
// and its Gregorian "YYYY-MM" months (already in calendar order).
func fiscalRoundLabel(round int, months []string) (string, error) {
	if len(months) == 0 {
		return fmt.Sprintf("รอบ %d", round), nil
	}
	fy, err := fiscalYearOf(months[0])
	if err != nil {
		return "", err
	}
	monthNum := func(ym string) (int, error) {
		_, m, ok := strings.Cut(ym, "-")
		if !ok {
			return 0, fmt.Errorf("year_month %q: want <year>-<MM>", ym)
		}
		return strconv.Atoi(m)
	}
	first, err := monthNum(months[0])
	if err != nil {
		return "", err
	}
	last, err := monthNum(months[len(months)-1])
	if err != nil {
		return "", err
	}
	rangeLabel := fiscalRoundMonthAbbrev[first-1]
	if last != first {
		rangeLabel += "–" + fiscalRoundMonthAbbrev[last-1]
	}
	return fmt.Sprintf("รอบ %d · งบ %d (%s)", round, fy+543, rangeLabel), nil
}

// termFiscalSplit is the one place this service reads the term's month
// calendar — everything below builds on it.
func (s *DocumentProgressService) termFiscalSplit(ctx context.Context, termID uuid.UUID) (FiscalSplit, error) {
	if s.export == nil {
		return FiscalSplit{}, nil
	}
	all, err := s.export.TermMonths(ctx, termID)
	if err != nil {
		return FiscalSplit{}, err
	}
	return fiscalSplit(all)
}

// roundMonths resolves round 2's Gregorian months, and whether that round
// exists AT ALL for this term (the fiscal split crosses the budget year).
// Round 1 always returns (nil, true, nil) — nil is the sentinel exportReadiness
// and the round-1 checklist read as "unscoped", exactly the pre-round
// behaviour.
func (s *DocumentProgressService) roundMonths(ctx context.Context, termID uuid.UUID, round int) (greg []string, exists bool, err error) {
	if round == 1 {
		return nil, true, nil
	}
	split, err := s.termFiscalSplit(ctx, termID)
	if err != nil {
		return nil, false, err
	}
	if !split.Crosses {
		return nil, false, nil
	}
	return split.After, true, nil
}

// fiscalRounds is what GetOverview needs to build every round's progress in
// one pass, without re-deriving the term's month calendar per round.
type fiscalRounds struct {
	// twoRounds is true only once the term BOTH crosses the budget-year
	// boundary AND round 2 has something payable — a term that crosses but
	// whose October has no work at all stays a single, unlabeled round (see
	// the package doc comment).
	twoRounds                bool
	round1Label, round2Label string
	round2Greg               []string
}

func (s *DocumentProgressService) resolveFiscalRounds(ctx context.Context, termID uuid.UUID) (*fiscalRounds, error) {
	split, err := s.termFiscalSplit(ctx, termID)
	if err != nil {
		return nil, err
	}
	fr := &fiscalRounds{}
	if !split.Crosses {
		return fr, nil
	}
	total, _, _, err := s.exportReadiness(ctx, termID, 2, split.After)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		// Crosses the boundary, but nothing in October (or whichever months
		// fall after it) has any billable work yet — no round 2 to show.
		return fr, nil
	}
	r1Label, err := fiscalRoundLabel(1, split.Before)
	if err != nil {
		return nil, err
	}
	r2Label, err := fiscalRoundLabel(2, split.After)
	if err != nil {
		return nil, err
	}
	fr.twoRounds = true
	fr.round1Label, fr.round2Label, fr.round2Greg = r1Label, r2Label, split.After
	return fr, nil
}

/* -------------------------------------------------------------------------- */
/* Board reads                                                                */
/* -------------------------------------------------------------------------- */

// GetOverview returns every round of progress this term has — one for the
// ordinary case, two for a crossing term whose second budget year has
// payable content. Readable by any authenticated user (the shared "where are
// the documents" board).
func (s *DocumentProgressService) GetOverview(ctx context.Context, termID uuid.UUID) (*TermProgressOverview, error) {
	fr, err := s.resolveFiscalRounds(ctx, termID)
	if err != nil {
		return nil, err
	}
	r1, err := s.getRoundProgress(ctx, termID, 1, fr)
	if err != nil {
		return nil, err
	}
	out := &TermProgressOverview{TermID: termID, Rounds: []TermProgress{*r1}}
	if fr.twoRounds {
		r2, err := s.getRoundProgress(ctx, termID, 2, fr)
		if err != nil {
			return nil, err
		}
		out.Rounds = append(out.Rounds, *r2)
	}
	return out, nil
}

// GetRound returns ONE round's progress directly — used by callers (and
// tests) that already know which round they want without paying for the
// other round's readiness query.
func (s *DocumentProgressService) GetRound(ctx context.Context, termID uuid.UUID, round int) (*TermProgress, error) {
	if round != 1 && round != 2 {
		return nil, Invalid("รอบไม่ถูกต้อง")
	}
	fr, err := s.resolveFiscalRounds(ctx, termID)
	if err != nil {
		return nil, err
	}
	if round == 2 && !fr.twoRounds {
		return nil, Invalid("เทอมนี้ไม่มีรอบ 2")
	}
	return s.getRoundProgress(ctx, termID, round, fr)
}

func (s *DocumentProgressService) getRoundProgress(ctx context.Context, termID uuid.UUID, round int, fr *fiscalRounds) (*TermProgress, error) {
	var gregMonths []string
	if round == 2 {
		gregMonths = fr.round2Greg
	}
	total, exported, unexported, err := s.exportReadiness(ctx, termID, round, gregMonths)
	if err != nil {
		return nil, err
	}
	label := fr.round1Label
	if round == 2 {
		label = fr.round2Label
	}
	p := &TermProgress{
		TermID:            termID,
		Round:             round,
		RoundLabel:        label,
		TotalCourses:      total,
		ExportedCourses:   exported,
		AllExported:       total > 0 && exported == total,
		UnexportedCourses: unexported,
	}
	// LEFT-style load: no row yet → stage 0.
	err = s.pool.QueryRow(ctx, `
		SELECT stage,
		       TO_CHAR(ta_signed_at,        'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       TO_CHAR(lecturer_signed_at,  'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       TO_CHAR(certifier_signed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       TO_CHAR(sent_finance_at,     'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       TO_CHAR(sent_treasury_at,    'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'),
		       note, updated_by_name,
		       TO_CHAR(updated_at,          'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
		FROM document_progress WHERE term_id = $1 AND fiscal_round = $2`, termID, round).Scan(
		&p.Stage, &p.TASignedAt, &p.LecturerSignedAt, &p.CertifierSignedAt,
		&p.SentFinanceAt, &p.SentTreasuryAt, &p.Note, &p.UpdatedByName, &p.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if aErr := s.fillAdvance(ctx, p); aErr != nil {
		return nil, aErr
	}
	return p, nil
}

// fillAdvance works out the one step the officer may take next, and who is
// standing in the way of the step they are on.
//
// Derived here rather than in the browser: the stepper used to let staff click
// any circle, so a term could be marked "ส่งการเงินแล้ว" while half the TAs had
// not signed — a state the paper cannot be in.
func (s *DocumentProgressService) fillAdvance(ctx context.Context, p *TermProgress) error {
	p.NextStage = p.Stage + 1
	if p.NextStage > 5 {
		p.NextStage = 5
	}
	p.CurrentRole = roleForStage(p.Stage + 1)
	if !p.AllExported || p.Stage >= 5 {
		return nil
	}
	// Advancing to stage N needs stage N-1's signatures. Stage 1 needs none of
	// its own — it IS the TA signing — so what gates it is the export, already
	// checked above.
	ok, missing, err := s.stageComplete(ctx, p.TermID, p.Stage+1, p.Round)
	if err != nil {
		return err
	}
	p.CanAdvance = ok
	p.SignersMissing = missing
	return nil
}

// SetStage moves the term's document progress for ONE ROUND to `stage`
// (0..5). Staff/admin only, and only once every course with round-relevant
// content has been exported (moving back to 0 is always allowed as a
// correction).
func (s *DocumentProgressService) SetStage(ctx context.Context, actor, termID uuid.UUID, stage int, note string, round int) error {
	if round != 1 && round != 2 {
		return Invalid("รอบไม่ถูกต้อง")
	}
	if stage < 0 || stage > 5 {
		return Invalid("ขั้นความคืบหน้าไม่ถูกต้อง")
	}
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
	}
	gregMonths, exists, err := s.roundMonths(ctx, termID, round)
	if err != nil {
		return err
	}
	if round == 2 && !exists {
		return Invalid("เทอมนี้ไม่มีรอบ 2")
	}
	if stage > 0 {
		total, exported, _, rErr := s.exportReadiness(ctx, termID, round, gregMonths)
		if rErr != nil {
			return rErr
		}
		if total == 0 {
			return Invalid("ยังไม่มีรายวิชาที่มีเอกสารต้องเดินในรอบนี้")
		}
		if exported < total {
			return Invalid("ยังส่งออกเอกสารไม่ครบทุกวิชาของรอบนี้ ต้องส่งออกให้ครบก่อนจึงจะเริ่มติดตามการเซ็นได้")
		}
	}
	// Every stage BELOW the requested one must already be complete. The paper
	// moves in one direction — a document cannot be on the lecturer's desk while
	// a TA has not signed it — so the board refuses to record a stage that the
	// physical folder cannot have reached. Going backwards is always allowed:
	// that is how staff correct a mistake.
	var current int
	_ = s.pool.QueryRow(ctx, `SELECT stage FROM document_progress WHERE term_id = $1 AND fiscal_round = $2`, termID, round).Scan(&current)
	if stage > current {
		// Up to AND INCLUDING the stage being set: pressing "TA เซ็นครบ" is the
		// claim that they all signed, so it needs their signatures — not just
		// the ones before it. Stages 4-5 have nobody to sign and pass trivially.
		// One checklist read for the whole loop: stageComplete would re-run the
		// same query per stage, so setting stage 5 cost five identical scans.
		items, lErr := s.ListChecklist(ctx, termID, round)
		if lErr != nil {
			return lErr
		}
		for n := 1; n <= stage; n++ {
			if ok, missing := stageCompleteIn(items, n); !ok {
				return Invalid(stageBlockedMessage(n, missing))
			}
		}
	}
	name := userDisplayName(ctx, s.pool, actor)
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "document_progress.set_stage",
			Entity: "academic_term", EntityID: termID.String(),
			After: map[string]any{"stage": stage, "round": round}},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
		INSERT INTO document_progress
		    (term_id, fiscal_round, stage, note, updated_by, updated_by_name, updated_at,
		     ta_signed_at, lecturer_signed_at, certifier_signed_at, sent_finance_at, sent_treasury_at)
		VALUES ($1, $2, $3, NULLIF($4,''), $5, $6, now(),
		     CASE WHEN $3>=1 THEN now() END,
		     CASE WHEN $3>=2 THEN now() END,
		     CASE WHEN $3>=3 THEN now() END,
		     CASE WHEN $3>=4 THEN now() END,
		     CASE WHEN $3>=5 THEN now() END)
		ON CONFLICT (term_id, fiscal_round) DO UPDATE SET
		     stage           = $3,
		     note            = NULLIF($4,''),
		     updated_by      = $5,
		     updated_by_name = $6,
		     updated_at      = now(),
		     ta_signed_at        = CASE WHEN $3>=1 THEN COALESCE(document_progress.ta_signed_at, now())        ELSE NULL END,
		     lecturer_signed_at  = CASE WHEN $3>=2 THEN COALESCE(document_progress.lecturer_signed_at, now())  ELSE NULL END,
		     certifier_signed_at = CASE WHEN $3>=3 THEN COALESCE(document_progress.certifier_signed_at, now()) ELSE NULL END,
		     sent_finance_at     = CASE WHEN $3>=4 THEN COALESCE(document_progress.sent_finance_at, now())     ELSE NULL END,
		     sent_treasury_at    = CASE WHEN $3>=5 THEN COALESCE(document_progress.sent_treasury_at, now())    ELSE NULL END`,
				termID, round, stage, note, actor, name)
			return err
		})
}

// userDisplayName returns "first last" (falling back to email) for a user id —
// a package-level companion to SubmissionPeriodService.userDisplayName so other
// services can snapshot an actor's name.
func userDisplayName(ctx context.Context, pool *pgxpool.Pool, uid uuid.UUID) string {
	var first, last, email string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(first_name,''), COALESCE(last_name,''), COALESCE(email,'')
		 FROM users WHERE id=$1`, uid).Scan(&first, &last, &email); err != nil {
		return ""
	}
	name := first + " " + last
	if name == " " || name == "" {
		return email
	}
	return name
}

/* -------------------------------------------------------------------------- */
/* Per-course signature checklist (migration 0037, per-round since 0082)      */
/* -------------------------------------------------------------------------- */

// SignatureItem is one (course × signing-role) checklist row: who is
// responsible and whether they have signed yet.
// SignatureItem is ONE PERSON's signature on one course's document, for one
// fiscal round.
//
// It used to be one row per course-role, with the "ta" row standing for every TA
// at once, so the board could only say "all of them signed" and the officer had
// no way to see which two of four were still missing. SignerID identifies the
// person for ta/lecturer; it is nil for certifier, who is a single
// admin_officers row rather than a system user.
type SignatureItem struct {
	TeachingCourseID uuid.UUID  `json:"teaching_course_id"`
	Code             string     `json:"code"`
	NameTH           string     `json:"name_th"`
	Exported         bool       `json:"exported"`
	Role             string     `json:"role"`
	RoleLabel        string     `json:"role_label"`
	SignerID         *uuid.UUID `json:"signer_id,omitempty"`
	// Responsible is the person's name — one name now, not a joined list.
	Responsible string  `json:"responsible"`
	SignedAt    *string `json:"signed_at,omitempty"`
}

// signatureRoles maps a stage number to the role signing at it. Stages 4 and 5
// (ส่งการเงิน / คณบดีลงนาม) carry no per-person signatures — they are actions
// the officer takes, not sheets somebody signs.
var signatureRoles = []struct {
	key, label string
	stage      int
}{
	{"ta", "TA เซ็น", 1},
	{"lecturer", "อาจารย์เซ็น", 2},
	{"certifier", "ผู้รับรองเซ็น", 3},
}

// roleForStage is the inverse — "" when the stage has nobody to chase.
func roleForStage(stage int) string {
	for _, r := range signatureRoles {
		if r.stage == stage {
			return r.key
		}
	}
	return ""
}

// ListChecklist returns one row per PERSON who has to sign each course's
// document IN THIS ROUND. Round 1 is the original query, unchanged (every
// course with an approved TA assignment, independent of any month — the
// roster is fixed at appointment) plus a fiscal_round=1 filter on the ticks so
// a round-2 signature can never read back as round 1's. Round 2 additionally
// requires roundBillableSQL — only courses that actually have round-2
// content get a round-2 document to sign at all.
//
// Names carry the คำนำหน้า the same way the export files do (ta_profiles.prefix
// first, users.title as the fallback) — the screen this feeds asks a TA to
// recognise the lecturer who is holding their paperwork, and a bare
// "วรัญญา วรรณศรี" is not how anybody here refers to them.
//
// The people are derived live rather than read from signature_checklist, so a
// TA added after the first export still appears — the checklist table only ever
// holds the ticks. Readable by any authenticated user.
func (s *DocumentProgressService) ListChecklist(ctx context.Context, termID uuid.UUID, round int) ([]SignatureItem, error) {
	var rows pgx.Rows
	var err error
	switch round {
	case 1:
		rows, err = s.pool.Query(ctx, `
			WITH courses AS (
			    SELECT tc.id, tc.code, tc.name_th, (tc.exported_at IS NOT NULL) AS exported
			    FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
			    WHERE tc.term_id = $1 AND x.has_ta
			),
			people AS (
			    SELECT DISTINCT c.id AS tc_id, 'ta' AS role, u.id AS signer_id,
			           COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '') ||
			           COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'') AS signer_name
			    FROM courses c
			    JOIN sections s               ON s.teaching_course_id = c.id
			    JOIN ta_request_assignments a ON a.section_id = s.id AND a.state <> 'dropped'
			    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
			    JOIN users u                  ON u.id = a.ta_id
			    LEFT JOIN ta_profiles tp      ON tp.user_id = u.id
			  UNION ALL
			    SELECT DISTINCT c.id, 'lecturer', u.id,
			           COALESCE(NULLIF(u.title,''), '') ||
			           COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'')
			    FROM courses c
			    JOIN ta_requests r ON r.teaching_course_id = c.id AND r.status = 'approved'
			    JOIN users u       ON u.id = r.lecturer_id
			  UNION ALL
			    SELECT c.id, 'certifier', NULL::uuid, NULL::text FROM courses c
			)
			SELECT c.id, c.code, c.name_th, c.exported,
			       p.role, p.signer_id, COALESCE(p.signer_name, ''),
			       TO_CHAR(sc.signed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
			FROM courses c
			JOIN people p ON p.tc_id = c.id
			LEFT JOIN signature_checklist sc
			       ON sc.teaching_course_id = c.id
			      AND sc.role = p.role
			      AND sc.signer_id IS NOT DISTINCT FROM p.signer_id
			      AND sc.fiscal_round = 1
			ORDER BY c.code, CASE p.role WHEN 'ta' THEN 1 WHEN 'lecturer' THEN 2 ELSE 3 END,
			         COALESCE(p.signer_name, '')`, termID)
	case 2:
		gregMonths, exists, rErr := s.roundMonths(ctx, termID, 2)
		if rErr != nil {
			return nil, rErr
		}
		if !exists {
			return []SignatureItem{}, nil
		}
		rows, err = s.pool.Query(ctx, `
			WITH courses AS (
			    SELECT tc.id, tc.code, tc.name_th,
			           `+roundExportedSQL("$2")+` AS exported
			    FROM teaching_courses tc,
			         LATERAL (SELECT `+courseHasTA+` AS has_ta) x,
			         LATERAL (SELECT `+roundBillableSQL("$2")+` AS billable) y
			    WHERE tc.term_id = $1 AND x.has_ta AND y.billable
			),
			people AS (
			    SELECT DISTINCT c.id AS tc_id, 'ta' AS role, u.id AS signer_id,
			           COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '') ||
			           COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'') AS signer_name
			    FROM courses c
			    JOIN sections s               ON s.teaching_course_id = c.id
			    JOIN ta_request_assignments a ON a.section_id = s.id AND a.state <> 'dropped'
			    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
			    JOIN users u                  ON u.id = a.ta_id
			    LEFT JOIN ta_profiles tp      ON tp.user_id = u.id
			  UNION ALL
			    SELECT DISTINCT c.id, 'lecturer', u.id,
			           COALESCE(NULLIF(u.title,''), '') ||
			           COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'')
			    FROM courses c
			    JOIN ta_requests r ON r.teaching_course_id = c.id AND r.status = 'approved'
			    JOIN users u       ON u.id = r.lecturer_id
			  UNION ALL
			    SELECT c.id, 'certifier', NULL::uuid, NULL::text FROM courses c
			)
			SELECT c.id, c.code, c.name_th, c.exported,
			       p.role, p.signer_id, COALESCE(p.signer_name, ''),
			       TO_CHAR(sc.signed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
			FROM courses c
			JOIN people p ON p.tc_id = c.id
			LEFT JOIN signature_checklist sc
			       ON sc.teaching_course_id = c.id
			      AND sc.role = p.role
			      AND sc.signer_id IS NOT DISTINCT FROM p.signer_id
			      AND sc.fiscal_round = 2
			ORDER BY c.code, CASE p.role WHEN 'ta' THEN 1 WHEN 'lecturer' THEN 2 ELSE 3 END,
			         COALESCE(p.signer_name, '')`, termID, gregMonths)
	default:
		return nil, Invalid("รอบไม่ถูกต้อง")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	labels := map[string]string{}
	for _, r := range signatureRoles {
		labels[r.key] = r.label
	}
	out := []SignatureItem{}
	for rows.Next() {
		var it SignatureItem
		if err := rows.Scan(&it.TeachingCourseID, &it.Code, &it.NameTH, &it.Exported,
			&it.Role, &it.SignerID, &it.Responsible, &it.SignedAt); err != nil {
			return nil, err
		}
		it.RoleLabel = labels[it.Role]
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The certifier's name comes from the term, not from a join — resolved once
	// and stamped onto every certifier row.
	if s.export != nil {
		if cert, cerr := s.export.ResolveCertifier(ctx, termID); cerr == nil && cert.Name != "" {
			for i := range out {
				if out[i].Role == "certifier" {
					out[i].Responsible = cert.Name
				}
			}
		}
	}
	for i := range out {
		if out[i].Responsible == "" {
			out[i].Responsible = "ผู้รับรอง"
		}
	}
	return out, nil
}

// stageBlockedMessage names the stage and up to a few of the people holding it
// up — a bare "ยังเซ็นไม่ครบ" leaves the officer with nobody to call.
func stageBlockedMessage(stage int, missing []string) string {
	label := "ขั้นก่อนหน้า"
	for _, r := range signatureRoles {
		if r.stage == stage {
			label = r.label
		}
	}
	shown := missing
	extra := ""
	if len(shown) > 5 {
		extra = fmt.Sprintf(" และอีก %d รายการ", len(shown)-5)
		shown = shown[:5]
	}
	return fmt.Sprintf("ยังข้ามขั้นไม่ได้ \"%s\" ยังไม่ครบ (เหลือ %d รายการ): %s%s",
		label, len(missing), strings.Join(shown, " · "), extra)
}

// stageComplete reports whether every signature the stage needs is in place
// for the given round, and names who is still missing.
//
// This is what makes the board a sequence instead of five free buttons: the
// officer may not record "อาจารย์เซ็นครบ" while a TA has not signed, because the
// physical document cannot have reached the lecturer yet.
func (s *DocumentProgressService) stageComplete(ctx context.Context, termID uuid.UUID, stage int, round int) (bool, []string, error) {
	items, err := s.ListChecklist(ctx, termID, round)
	if err != nil {
		return false, nil, err
	}
	ok, missing := stageCompleteIn(items, stage)
	return ok, missing, nil
}

// stageCompleteIn is the same answer against a checklist already in hand, so a
// caller checking several stages reads the list once.
func stageCompleteIn(items []SignatureItem, stage int) (bool, []string) {
	role := roleForStage(stage)
	if role == "" {
		return true, nil // stages 4-5 are officer actions, nothing to sign
	}
	var missing []string
	for _, it := range items {
		if it.Role == role && it.SignedAt == nil {
			missing = append(missing, it.Code+" — "+it.Responsible)
		}
	}
	return len(missing) == 0, missing
}

// ToggleSignature marks (or clears) ONE PERSON's signature on one course, for
// ONE fiscal round.
//
// signerID is the TA or lecturer who signed; nil for the certifier, who is a
// single admin_officers row rather than a user. Staff/admin only.
func (s *DocumentProgressService) ToggleSignature(
	ctx context.Context, actor, tcID uuid.UUID, role string, signerID *uuid.UUID, signed bool, round int,
) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
	}
	if round != 1 && round != 2 {
		return Invalid("รอบไม่ถูกต้อง")
	}
	if role != "ta" && role != "lecturer" && role != "certifier" {
		return Invalid("บทบาทไม่ถูกต้อง")
	}
	// A person is required for the roles that have people, and forbidden for the
	// one that does not — otherwise a stray nil would quietly write a lumped row
	// again and the stage gate would read it as everybody signing at once.
	if role == "certifier" && signerID != nil {
		return Invalid("ผู้รับรองมีผู้ลงนามคนเดียว ไม่ต้องระบุรายบุคคล")
	}
	if role != "certifier" && signerID == nil {
		return Invalid("ต้องระบุผู้ลงนาม")
	}
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, tcID).Scan(&termID); err != nil {
		return Invalid("ไม่พบรายวิชา")
	}
	var signerName string
	if signerID != nil {
		_ = s.pool.QueryRow(ctx, `
			SELECT COALESCE(first_name,'') || ' ' || COALESCE(last_name,'')
			FROM users WHERE id = $1`, *signerID).Scan(&signerName)
	}

	name := userDisplayName(ctx, s.pool, actor)
	signedExpr := "NULL"
	if signed {
		signedExpr = "now()"
	}
	// Two upserts because the uniqueness is split in two partial indexes: a
	// person row keys on (course, role, signer, round), the certifier row on
	// (course, role, round) alone. ON CONFLICT has to name the matching one.
	conflict := "(teaching_course_id, role, signer_id, fiscal_round) WHERE signer_id IS NOT NULL"
	if signerID == nil {
		conflict = "(teaching_course_id, role, fiscal_round) WHERE signer_id IS NULL"
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "signature_checklist.toggle",
			Entity: "teaching_course", EntityID: tcID.String(),
			After: map[string]any{"role": role, "signer_id": signerID, "signed": signed, "round": round}},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO signature_checklist
				    (term_id, teaching_course_id, role, signer_id, signer_name, fiscal_round,
				     signed_at, updated_by, updated_by_name, updated_at)
				VALUES ($1, $2, $3, $4, NULLIF($5,''), $8, `+signedExpr+`, $6, $7, now())
				ON CONFLICT `+conflict+` DO UPDATE SET
				  signed_at = `+signedExpr+`, signer_name = EXCLUDED.signer_name,
				  updated_by = $6, updated_by_name = $7, updated_at = now()`,
				termID, tcID, role, signerID, signerName, actor, name, round)
			return err
		})
}

// RemindUnsigned emails+notifies every lecturer whose course still lacks the
// lecturer signature FOR THIS ROUND, one message per lecturer listing their
// pending courses. Returns how many people were notified. Staff/admin only.
func (s *DocumentProgressService) RemindUnsigned(ctx context.Context, actor, termID uuid.UUID, round int) (int, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return 0, err
	}
	if !priv {
		return 0, ErrForbidden
	}
	if round != 1 && round != 2 {
		return 0, Invalid("รอบไม่ถูกต้อง")
	}
	if s.notify == nil {
		return 0, nil
	}
	var rows pgx.Rows
	switch round {
	case 1:
		// The submitter, not every co-teacher — see ListChecklist's own note.
		rows, err = s.pool.Query(ctx, `
			SELECT DISTINCT r.lecturer_id, tc.code
			FROM teaching_courses tc
			JOIN ta_requests r ON r.teaching_course_id = tc.id AND r.status = 'approved'
			LEFT JOIN signature_checklist sc
			       ON sc.teaching_course_id = tc.id
			      AND sc.role = 'lecturer'
			      AND sc.signer_id = r.lecturer_id
			      AND sc.fiscal_round = 1
			CROSS JOIN LATERAL (SELECT `+courseHasTA+` AS has_ta) x
			WHERE tc.term_id = $1 AND x.has_ta AND sc.signed_at IS NULL
			ORDER BY tc.code`, termID)
	case 2:
		var gregMonths []string
		var exists bool
		gregMonths, exists, err = s.roundMonths(ctx, termID, 2)
		if err != nil {
			return 0, err
		}
		if !exists {
			return 0, nil
		}
		rows, err = s.pool.Query(ctx, `
			SELECT DISTINCT r.lecturer_id, tc.code
			FROM teaching_courses tc
			JOIN ta_requests r ON r.teaching_course_id = tc.id AND r.status = 'approved'
			LEFT JOIN signature_checklist sc
			       ON sc.teaching_course_id = tc.id
			      AND sc.role = 'lecturer'
			      AND sc.signer_id = r.lecturer_id
			      AND sc.fiscal_round = 2
			CROSS JOIN LATERAL (SELECT `+courseHasTA+` AS has_ta) x
			CROSS JOIN LATERAL (SELECT `+roundBillableSQL("$2")+` AS billable) y
			WHERE tc.term_id = $1 AND x.has_ta AND y.billable AND sc.signed_at IS NULL
			ORDER BY tc.code`, termID, gregMonths)
	}
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	byUser := map[uuid.UUID][]string{}
	order := []uuid.UUID{}
	for rows.Next() {
		var uid uuid.UUID
		var code string
		if err := rows.Scan(&uid, &code); err != nil {
			return 0, err
		}
		if _, ok := byUser[uid]; !ok {
			order = append(order, uid)
		}
		byUser[uid] = append(byUser[uid], code)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	roundNote := ""
	if round == 2 {
		roundNote = " (รอบ 2 — งบประมาณปีใหม่)"
	}
	for _, uid := range order {
		body := "มีเอกสารเบิกจ่าย TA ที่รอลายเซ็นของท่านในรายวิชา: " +
			strings.Join(byUser[uid], ", ") + roundNote +
			"\nกรุณาลงนามเพื่อให้เอกสารเดินทางต่อได้"
		s.notify.Send(ctx, uid, "แจ้งเตือน: เอกสาร TA รอลายเซ็น", body, "/document-progress")
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "signature_checklist.remind",
		Entity: "academic_term", EntityID: termID.String(),
		After: map[string]any{"notified": len(order), "round": round}}); err != nil {
		return 0, err
	}
	return len(order), nil
}
