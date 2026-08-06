package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// certifier.go answers "who signs the ผู้รับรอง block on this term's claim
// forms", and is the reason that block stopped printing the wrong person.
//
// The claim template (assets/templates/ta_claim_form.xlsx) hardcodes the
// position line "ตำแหน่ง หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์" above that
// signature. The exporter used to fill the name cell above it with the COURSE
// LECTURER's name, so every claim form asserted that the lecturer had certified
// their own TA's hours in the capacity of head of department.
//
// One certifier per term, not per course: it is a college appointment, and the
// screen that picks it lists every course at once. Unset means "whoever holds
// the seat", which is what staff expect on day one and needs no setup.

// CertifierChoice is the resolved certifier for one term, worded exactly as the
// claim form will print it.
type CertifierChoice struct {
	// OfficerID is the explicit choice stored on the term; nil when the term
	// simply follows whoever holds the seat.
	OfficerID *uuid.UUID `json:"officer_id,omitempty"`
	Name      string     `json:"name"`
	// TitleLine carries the acting phrase when the certifier does not hold the
	// seat ("รองคณบดีฝ่ายบริหาร รักษาการแทน").
	TitleLine string `json:"title_line"`
	// ActingFor is the seat being exercised, empty when they hold it.
	ActingFor string `json:"acting_for,omitempty"`
	// Resolved is false when nobody could be found at all — no explicit choice
	// and no head of department on the roster. The claim form then leaves the
	// block blank for a wet signature rather than inventing a name.
	Resolved bool `json:"resolved"`
}

// PositionLine is what the form prints under the name: the signer's own
// position, plus the seat when they are acting in it.
func (c CertifierChoice) PositionLine() string {
	if c.ActingFor == "" {
		return c.TitleLine
	}
	return c.TitleLine + " " + c.ActingFor
}

// ClaimCells returns the two ผู้รับรอง cells of the claim form — the bracketed
// name and the position line beneath it — or ok=false to leave the template's
// blank line and printed seat alone.
//
// Extracted from the writer so the one decision that matters here can be tested
// without assembling a whole workbook: WHOSE name goes above that signature.
// It was the lecturer's, under a position line naming a seat they do not hold.
func (c CertifierChoice) ClaimCells() (name, position string, ok bool) {
	if !c.Resolved || c.Name == "" {
		return "", "", false
	}
	return "(" + c.Name + ")", "ตำแหน่ง " + c.PositionLine(), true
}

// ResolveCertifier returns the certifier for a term: the officer explicitly
// chosen for it, otherwise the active head of department.
func (s *ExportService) ResolveCertifier(ctx context.Context, termID uuid.UUID) (CertifierChoice, error) {
	var chosen *uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT certifier_officer_id FROM academic_terms WHERE id = $1`, termID).Scan(&chosen); err != nil {
		return CertifierChoice{}, err
	}

	if chosen != nil {
		var a signerAuthority
		err := s.pool.QueryRow(ctx, `
			SELECT COALESCE(academic_prefix,'') || full_name, title
			FROM admin_officers WHERE id = $1`, *chosen).Scan(&a.Name, &a.Title)
		if err == nil {
			// Worded against the HEAD-OF-DEPARTMENT seat — that is the authority
			// a claim form is certified under, not the dean's.
			a.applyActing(ctx, s.pool, headTitlePrefix, fallbackHeadTitle)
			return CertifierChoice{
				OfficerID: chosen, Name: a.Name,
				TitleLine: a.Title, ActingFor: a.ActingFor, Resolved: true,
			}, nil
		}
		// An officer deleted after being chosen must not break the export — fall
		// through to the seat holder, the same as never having chosen.
	}

	// No explicit choice: whoever currently holds the seat.
	var name, title string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(academic_prefix,'') || full_name, title
		FROM admin_officers
		WHERE is_active AND title LIKE $1 || '%'
		ORDER BY created_at
		LIMIT 1`, headTitlePrefix).Scan(&name, &title)
	if err != nil {
		return CertifierChoice{Resolved: false}, nil
	}
	return CertifierChoice{Name: name, TitleLine: title, Resolved: true}, nil
}

// SetCertifier records who signs this term's claim forms. A nil officer clears
// the choice and returns the term to following the seat.
func (s *ExportService) SetCertifier(ctx context.Context, actor, termID uuid.UUID, officerID *uuid.UUID) error {
	if officerID != nil {
		var active bool
		if err := s.pool.QueryRow(ctx,
			`SELECT is_active FROM admin_officers WHERE id = $1`, *officerID).Scan(&active); err != nil {
			return Invalid("ไม่พบรายชื่อผู้รับรองที่เลือก")
		}
		if !active {
			return Invalid("รายชื่อที่เลือกถูกปิดใช้งานแล้ว เลือกผู้รับรองที่ยังใช้งานอยู่")
		}
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "term.set_certifier",
			Entity: "academic_term", EntityID: termID.String(),
			After: map[string]any{"certifier_officer_id": officerID}},
		func(tx pgx.Tx) error {
			res, err := tx.Exec(ctx,
				`UPDATE academic_terms SET certifier_officer_id = $2 WHERE id = $1`,
				termID, officerID)
			if err != nil {
				return err
			}
			if res.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		})
}
