package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// DocumentProgressService tracks the off-system physical document journey for a
// whole TERM (one batch): the signature round + routing to finance/treasury.
// The board only becomes actionable once every course in the term is exported.
// See migration 0031 for the stage meanings.
type DocumentProgressService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
	// export resolves the term's certifier — the one signer on the checklist who
	// is an admin_officers row rather than a system user.
	export *ExportService
}

// CourseRef names an un-exported course still blocking the term.
type CourseRef struct {
	Code   string `json:"code"`
	NameTH string `json:"name_th"`
}

// TermProgress is the single per-term progress record + export readiness.
type TermProgress struct {
	TermID uuid.UUID `json:"term_id"`
	// Export readiness — signing can only start once ALL courses that have TAs
	// are exported. TotalCourses counts courses with >=1 approved TA assignment.
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

// courseHasTA is the shared predicate for "this course has documents to sign".
const courseHasTA = `EXISTS (
	SELECT 1 FROM ta_request_assignments a
	JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
	JOIN sections s ON s.id = a.section_id
	WHERE s.teaching_course_id = tc.id)`

// exportReadiness returns (total courses-with-TAs, exported count, unexported list).
func (s *DocumentProgressService) exportReadiness(ctx context.Context, termID uuid.UUID) (total, exported int, unexported []CourseRef, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE has_ta),
		       COUNT(*) FILTER (WHERE has_ta AND tc.exported_at IS NOT NULL)
		FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1`, termID).Scan(&total, &exported)
	if err != nil {
		return
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tc.code, tc.name_th
		FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1 AND x.has_ta AND tc.exported_at IS NULL
		ORDER BY tc.code`, termID)
	if err != nil {
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

// GetByTerm returns the term's document progress + export readiness. Readable by
// any authenticated user (the shared "where are the documents" board).
func (s *DocumentProgressService) GetByTerm(ctx context.Context, termID uuid.UUID) (*TermProgress, error) {
	total, exported, unexported, err := s.exportReadiness(ctx, termID)
	if err != nil {
		return nil, err
	}
	p := &TermProgress{
		TermID:            termID,
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
		FROM document_progress WHERE term_id = $1`, termID).Scan(
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
	ok, missing, err := s.stageComplete(ctx, p.TermID, p.Stage+1)
	if err != nil {
		return err
	}
	p.CanAdvance = ok
	p.SignersMissing = missing
	return nil
}

// SetStage moves the term's document progress to `stage` (0..5). Staff/admin
// only, and only once EVERY course with TAs in the term has been exported
// (moving back to 0 is always allowed as a correction).
func (s *DocumentProgressService) SetStage(ctx context.Context, actor, termID uuid.UUID, stage int, note string) error {
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
	if stage > 0 {
		total, exported, _, rErr := s.exportReadiness(ctx, termID)
		if rErr != nil {
			return rErr
		}
		if total == 0 {
			return Invalid("ยังไม่มีรายวิชาที่มี TA ในเทอมนี้")
		}
		if exported < total {
			return Invalid("ยังส่งออกเอกสารไม่ครบทุกวิชา — ต้องส่งออกให้ครบก่อนจึงจะเริ่มติดตามการเซ็นได้")
		}
	}
	// Every stage BELOW the requested one must already be complete. The paper
	// moves in one direction — a document cannot be on the lecturer's desk while
	// a TA has not signed it — so the board refuses to record a stage that the
	// physical folder cannot have reached. Going backwards is always allowed:
	// that is how staff correct a mistake.
	var current int
	_ = s.pool.QueryRow(ctx, `SELECT stage FROM document_progress WHERE term_id = $1`, termID).Scan(&current)
	if stage > current {
		// Up to AND INCLUDING the stage being set: pressing "TA เซ็นครบ" is the
		// claim that they all signed, so it needs their signatures — not just
		// the ones before it. Stages 4-5 have nobody to sign and pass trivially.
		// One checklist read for the whole loop: stageComplete would re-run the
		// same query per stage, so setting stage 5 cost five identical scans.
		items, lErr := s.ListChecklist(ctx, termID)
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
			After: map[string]int{"stage": stage}},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
		INSERT INTO document_progress
		    (term_id, stage, note, updated_by, updated_by_name, updated_at,
		     ta_signed_at, lecturer_signed_at, certifier_signed_at, sent_finance_at, sent_treasury_at)
		VALUES ($1, $2, NULLIF($3,''), $4, $5, now(),
		     CASE WHEN $2>=1 THEN now() END,
		     CASE WHEN $2>=2 THEN now() END,
		     CASE WHEN $2>=3 THEN now() END,
		     CASE WHEN $2>=4 THEN now() END,
		     CASE WHEN $2>=5 THEN now() END)
		ON CONFLICT (term_id) DO UPDATE SET
		     stage           = $2,
		     note            = NULLIF($3,''),
		     updated_by      = $4,
		     updated_by_name = $5,
		     updated_at      = now(),
		     ta_signed_at        = CASE WHEN $2>=1 THEN COALESCE(document_progress.ta_signed_at, now())        ELSE NULL END,
		     lecturer_signed_at  = CASE WHEN $2>=2 THEN COALESCE(document_progress.lecturer_signed_at, now())  ELSE NULL END,
		     certifier_signed_at = CASE WHEN $2>=3 THEN COALESCE(document_progress.certifier_signed_at, now()) ELSE NULL END,
		     sent_finance_at     = CASE WHEN $2>=4 THEN COALESCE(document_progress.sent_finance_at, now())     ELSE NULL END,
		     sent_treasury_at    = CASE WHEN $2>=5 THEN COALESCE(document_progress.sent_treasury_at, now())    ELSE NULL END`,
				termID, stage, note, actor, name)
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
/* Per-course signature checklist (migration 0037)                            */
/* -------------------------------------------------------------------------- */

// SignatureItem is one (course × signing-role) checklist row: who is
// responsible and whether they have signed yet.
// SignatureItem is ONE PERSON's signature on one course's document.
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
// document, for every course in the term that has approved TAs.
//
// The people are derived live rather than read from signature_checklist, so a
// TA added after the first export still appears — the checklist table only ever
// holds the ticks. Readable by any authenticated user.
func (s *DocumentProgressService) ListChecklist(ctx context.Context, termID uuid.UUID) ([]SignatureItem, error) {
	rows, err := s.pool.Query(ctx, `
		WITH courses AS (
		    SELECT tc.id, tc.code, tc.name_th, (tc.exported_at IS NOT NULL) AS exported
		    FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		    WHERE tc.term_id = $1 AND x.has_ta
		),
		people AS (
		    -- Every TA who actually holds an assignment on the course. One row
		    -- each: the document is signed one hand at a time.
		    SELECT DISTINCT c.id AS tc_id, 'ta' AS role, u.id AS signer_id,
		           COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'') AS signer_name
		    FROM courses c
		    JOIN sections s               ON s.teaching_course_id = c.id
		    JOIN ta_request_assignments a ON a.section_id = s.id AND a.state <> 'dropped'
		    JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
		    JOIN users u                  ON u.id = a.ta_id
		  UNION ALL
		    -- The lecturer stage belongs to whoever SUBMITTED the request. A
		    -- co-teacher listed on teaching_lecturers who filed nothing has
		    -- nothing to sign, and naming them sent the officer after the wrong
		    -- person.
		    SELECT DISTINCT c.id, 'lecturer', u.id,
		           COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'')
		    FROM courses c
		    JOIN ta_requests r ON r.teaching_course_id = c.id AND r.status = 'approved'
		    JOIN users u       ON u.id = r.lecturer_id
		  UNION ALL
		    -- One certifier per course, and not a system user — signer_id stays
		    -- NULL and the name is resolved from the term's officer.
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
		ORDER BY c.code, CASE p.role WHEN 'ta' THEN 1 WHEN 'lecturer' THEN 2 ELSE 3 END,
		         COALESCE(p.signer_name, '')`, termID)
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
	return fmt.Sprintf("ยังข้ามขั้นไม่ได้ — \"%s\" ยังไม่ครบ (เหลือ %d รายการ): %s%s",
		label, len(missing), strings.Join(shown, " · "), extra)
}

// stageComplete reports whether every signature the stage needs is in place,
// and names who is still missing.
//
// This is what makes the board a sequence instead of five free buttons: the
// officer may not record "อาจารย์เซ็นครบ" while a TA has not signed, because the
// physical document cannot have reached the lecturer yet.
func (s *DocumentProgressService) stageComplete(ctx context.Context, termID uuid.UUID, stage int) (bool, []string, error) {
	items, err := s.ListChecklist(ctx, termID)
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

// ToggleSignature marks (or clears) ONE PERSON's signature on one course.
//
// signerID is the TA or lecturer who signed; nil for the certifier, who is a
// single admin_officers row rather than a user. Staff/admin only.
func (s *DocumentProgressService) ToggleSignature(
	ctx context.Context, actor, tcID uuid.UUID, role string, signerID *uuid.UUID, signed bool,
) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
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
	// person row keys on (course, role, signer), the certifier row on
	// (course, role) alone. ON CONFLICT has to name the matching one.
	conflict := "(teaching_course_id, role, signer_id) WHERE signer_id IS NOT NULL"
	if signerID == nil {
		conflict = "(teaching_course_id, role) WHERE signer_id IS NULL"
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "signature_checklist.toggle",
			Entity: "teaching_course", EntityID: tcID.String(),
			After: map[string]any{"role": role, "signer_id": signerID, "signed": signed}},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO signature_checklist
				    (term_id, teaching_course_id, role, signer_id, signer_name,
				     signed_at, updated_by, updated_by_name, updated_at)
				VALUES ($1, $2, $3, $4, NULLIF($5,''), `+signedExpr+`, $6, $7, now())
				ON CONFLICT `+conflict+` DO UPDATE SET
				  signed_at = `+signedExpr+`, signer_name = EXCLUDED.signer_name,
				  updated_by = $6, updated_by_name = $7, updated_at = now()`,
				termID, tcID, role, signerID, signerName, actor, name)
			return err
		})
}

// RemindUnsigned emails+notifies every lecturer whose course still lacks the
// lecturer signature, one message per lecturer listing their pending courses.
// Returns how many people were notified. Staff/admin only.
func (s *DocumentProgressService) RemindUnsigned(ctx context.Context, actor, termID uuid.UUID) (int, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return 0, err
	}
	if !priv {
		return 0, ErrForbidden
	}
	if s.notify == nil {
		return 0, nil
	}
	// The submitter, not every co-teacher. This used to join teaching_lecturers,
	// so a lecturer who merely appears on the course roster — and therefore has
	// nothing on the claim to sign — got chased for a signature that was never
	// theirs. It has to agree with ListChecklist, which shows exactly these
	// people, or the button nags somebody the board never asked for.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.lecturer_id, tc.code
		FROM teaching_courses tc
		JOIN ta_requests r ON r.teaching_course_id = tc.id AND r.status = 'approved'
		LEFT JOIN signature_checklist sc
		       ON sc.teaching_course_id = tc.id
		      AND sc.role = 'lecturer'
		      AND sc.signer_id = r.lecturer_id
		CROSS JOIN LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1 AND x.has_ta AND sc.signed_at IS NULL
		ORDER BY tc.code`, termID)
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
	for _, uid := range order {
		body := "มีเอกสารเบิกจ่าย TA ที่รอลายเซ็นของท่านในรายวิชา: " +
			strings.Join(byUser[uid], ", ") +
			"\nกรุณาลงนามเพื่อให้เอกสารเดินทางต่อได้"
		s.notify.Send(ctx, uid, "แจ้งเตือน: เอกสาร TA รอลายเซ็น", body, "/document-progress")
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "signature_checklist.remind",
		Entity: "academic_term", EntityID: termID.String(),
		After: map[string]int{"notified": len(order)}}); err != nil {
		return 0, err
	}
	return len(order), nil
}
