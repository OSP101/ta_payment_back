package service

// export_suppliers.go — the finance office's "Template-Suppliers" sheet
// (06/10/2026): one row per TA who is NEW in a term, which finance imports into
// the university ERP to open the TA as a supplier (POZ_SUPPLIERS_INT) with a
// bank account (IBY_TEMP_EXT_BANK_ACCTS) before the first payment.
//
// Built on the college's own file (data/suppliers_template.xlsx, a copy of
// docs/ตัวอย่างTemplate-Suppliers.xlsx) so the two header rows, fonts, borders
// and column widths are theirs. Two deliberate differences from the sample row:
//   - H/K/S hold values, not the sample's =F3 / =D3 / =E3 formulas. A file
//     generated here has no cached formula results, and an import tool that
//     reads the xlsx without recalculating would see those cells empty.
//   - Supplier Number and the bank account are TEXT, not numbers: an account
//     number can start with 0, and a 13-digit number is one Excel may show in
//     scientific notation.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
)

//go:embed data/suppliers_template.xlsx
var suppliersTemplate []byte

const (
	suppliersSheet = "Sheet1"
	// suppliersFirstRow is where the sample row sits in the template; data
	// replaces it.
	suppliersFirstRow = 3
	// Fixed per the finance office (06/10/2026): every TA is paid by this
	// college, and withheld as a student.
	suppliersProcurementBU = "วิทยาลัยการคอมพิวเตอร์"
	suppliersWithholding   = "student"
)

// SupplierCandidate is one new TA of the term, as the preview shows it. It
// carries no PII beyond the name — only whether each piece is on file.
type SupplierCandidate struct {
	UserID       uuid.UUID  `json:"user_id"`
	Name         string     `json:"name"`
	StudentID    string     `json:"student_id"`
	ProfileState string     `json:"profile_status"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	HasCitizenID bool       `json:"has_citizen_id"`
	HasPayee     bool       `json:"has_payee"`
	// Missing is empty when the TA goes into the file; otherwise the Thai
	// reason they are left out.
	Missing string `json:"missing,omitempty"`
	// Incomplete names what the file leaves blank for staff to copy from the
	// creditor-form PDF — the TA is in the file, those cells are highlighted.
	Incomplete string `json:"incomplete,omitempty"`

	prefix, first, last, email, phone string
}

func (c SupplierCandidate) ready() bool { return c.Missing == "" }

// SupplierCandidates lists the TAs who are new in termID: at least one approved
// request (assignment not dropped) in this term and none in any earlier term.
// A staff "returning" override (users.ta_seniority_override) wins, for people
// whose TA history predates this system.
func (s *DocsService) SupplierCandidates(ctx context.Context, termID uuid.UUID) ([]SupplierCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		WITH t AS (SELECT academic_year, semester FROM academic_terms WHERE id = $1),
		appointed AS (
			SELECT a.ta_id, at.academic_year, at.semester, tc.term_id
			  FROM ta_request_assignments a
			  JOIN ta_requests r       ON r.id = a.request_id
			  JOIN teaching_courses tc ON tc.id = r.teaching_course_id
			  JOIN academic_terms at   ON at.id = tc.term_id
			 WHERE r.status = 'approved' AND a.state <> 'dropped')
		SELECT u.id, COALESCE(p.prefix, ''), u.first_name, u.last_name,
		       COALESCE(u.student_id, ''), COALESCE(u.email, ''), COALESCE(u.phone, ''),
		       COALESCE(p.status::text, 'pending'), p.verified_at,
		       p.citizen_id_enc IS NOT NULL, p.payee_enc IS NOT NULL
		  FROM users u
		  LEFT JOIN ta_profiles p ON p.user_id = u.id
		 WHERE u.id IN (SELECT ta_id FROM appointed WHERE term_id = $1)
		   AND NOT EXISTS (
		         SELECT 1 FROM appointed e, t
		          WHERE e.ta_id = u.id
		            AND (e.academic_year, e.semester) < (t.academic_year, t.semester))
		   AND u.ta_seniority_override IS DISTINCT FROM 'returning'
		   AND u.deleted_at IS NULL
		 ORDER BY u.first_name, u.last_name`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SupplierCandidate{}
	for rows.Next() {
		var c SupplierCandidate
		if err := rows.Scan(&c.UserID, &c.prefix, &c.first, &c.last, &c.StudentID, &c.email, &c.phone,
			&c.ProfileState, &c.ApprovedAt, &c.HasCitizenID, &c.HasPayee); err != nil {
			return nil, err
		}
		c.Name = strings.TrimSpace(c.prefix + c.first + " " + c.last)
		if c.ProfileState != "approved" {
			c.Missing = "เอกสารยังไม่ได้รับการอนุมัติ"
		} else {
			// Approved before 0076 (ID) or 0152 (bank/address) was in place:
			// the data exists only in their approved creditor-form PDF. Still
			// in the file, so finance gets every new TA; staff fill the gaps.
			var gaps []string
			if !c.HasCitizenID {
				gaps = append(gaps, "เลขบัตรประชาชน")
			}
			if !c.HasPayee {
				gaps = append(gaps, "บัญชีธนาคารและที่อยู่")
			}
			if len(gaps) > 0 {
				c.Incomplete = "ไม่มี" + strings.Join(gaps, " และ") + "ในระบบ"
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BuildSuppliersWorkbook fills the template with every ready candidate of
// termID (incomplete ones included, their gaps highlighted) and returns the
// file plus how many TAs it holds. Each TA's ID and
// bank/address are decrypted through the audited Reveal* functions, and the
// export itself is audited before the bytes leave — never best-effort, same
// rule as the transfer cover.
func (s *DocsService) BuildSuppliersWorkbook(ctx context.Context, actor, termID uuid.UUID) ([]byte, int, error) {
	cands, err := s.SupplierCandidates(ctx, termID)
	if err != nil {
		return nil, 0, err
	}
	var ready []SupplierCandidate
	for _, c := range cands {
		if c.ready() {
			ready = append(ready, c)
		}
	}
	if len(ready) == 0 {
		return nil, 0, Invalid("ยังไม่มีทีเอใหม่ที่เอกสารได้รับการอนุมัติในภาคการศึกษานี้")
	}

	rows := make([]supplierRow, 0, len(ready))
	for _, c := range ready {
		r := supplierRow{
			Prefix: c.prefix, First: c.first, Last: c.last,
			Email: c.email, Phone: c.phone, StartDate: c.ApprovedAt,
		}
		if c.HasCitizenID {
			if r.CitizenID, err = s.RevealCitizenID(ctx, actor, c.UserID, "ไฟล์ Suppliers"); err != nil {
				return nil, 0, fmt.Errorf("citizen id of %s: %w", c.UserID, err)
			}
		}
		if c.HasPayee {
			payee, err := s.RevealPayee(ctx, actor, c.UserID, "ไฟล์ Suppliers")
			if err != nil {
				return nil, 0, fmt.Errorf("payee of %s: %w", c.UserID, err)
			}
			r.Payee = *payee
		}
		rows = append(rows, r)
	}

	body, err := writeSuppliersWorkbook(rows)
	if err != nil {
		return nil, 0, err
	}
	skipped := len(cands) - len(ready)
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "export.suppliers", Entity: "academic_term", EntityID: termID.String(),
		After: map[string]any{"ta_count": len(rows), "skipped": skipped},
	}); err != nil {
		return nil, 0, err
	}
	return body, len(rows), nil
}

type supplierRow struct {
	Prefix, First, Last string
	CitizenID           string
	Email, Phone        string
	Payee               PayeeDetails
	StartDate           *time.Time
}

// bankShortName drops the leading "ธนาคาร" — the finance sample writes
// "ไทยพาณิชย์", while the form's bank list carries the full name.
func bankShortName(name string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "ธนาคาร"))
}

// beDate returns the approval date as the finance sample stores it: a real
// date cell whose YEAR is the Buddhist year (29/5/2569 typed into Excel), so it
// reads the same as the hand-made file under the template's d/m/yyyy format.
func beDate(t time.Time) time.Time {
	t = t.In(time.FixedZone("ICT", 7*60*60)) // Thailand has no DST
	return time.Date(t.Year()+543, t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func writeSuppliersWorkbook(rows []supplierRow) ([]byte, error) {
	f, err := excelize.OpenReader(bytes.NewReader(suppliersTemplate))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// The sample paints I3/N3/O3/T3 red to point them out to whoever fills it
	// in; data rows are plain black. A3 is the template's own plain cell.
	plain, err := f.GetCellStyle(suppliersSheet, "A3")
	if err != nil {
		return nil, err
	}
	font := &excelize.Font{Family: "Sarabun", Size: 11, Color: "000000"}
	dateFmt := "d/m/yyyy"
	dateStyle, err := f.NewStyle(&excelize.Style{Font: font, Border: thinBorders(), CustomNumFmt: &dateFmt})
	if err != nil {
		return nil, err
	}
	// A text style for the ID and account number, so Excel keeps every digit.
	textStyle, err := f.NewStyle(&excelize.Style{Font: font, Border: thinBorders(), NumFmt: 49}) // "@"
	if err != nil {
		return nil, err
	}
	// Yellow marks a cell the system had no data for (see
	// SupplierCandidate.Incomplete), so staff can find what to fill in.
	fill := excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFF2CC"}}
	gapText, err := f.NewStyle(&excelize.Style{Font: font, Border: thinBorders(), NumFmt: 49, Fill: fill})
	if err != nil {
		return nil, err
	}
	gapPlain, err := f.NewStyle(&excelize.Style{Font: font, Border: thinBorders(), Fill: fill})
	if err != nil {
		return nil, err
	}
	for col := 1; col <= 20; col++ {
		cell, _ := excelize.CoordinatesToCellName(col, suppliersFirstRow)
		if err := f.SetCellValue(suppliersSheet, cell, nil); err != nil {
			return nil, err
		}
	}

	for i, r := range rows {
		n := suppliersFirstRow + i
		name := r.Prefix + r.First + " " + r.Last
		var postal any = r.Payee.PostalCode
		if v, err := parsePostal(r.Payee.PostalCode); err == nil {
			postal = v // numeric, as in the sample (40260)
		}
		values := []any{
			r.Prefix,                        // A First Name New (the sample puts คำนำหน้า here)
			r.First,                         // B First Name
			nil,                             // C Middle Name
			r.Last,                          // D Last Name
			name,                            // E Supplier Name*
			r.CitizenID,                     // F Supplier Number
			nil,                             // G Supplier Type
			r.CitizenID,                     // H Supplier Number (=F)
			suppliersWithholding,            // I Withholding Tax Group
			suppliersProcurementBU,          // J Procurement BU*
			r.Last,                          // K Last Name (=D)
			r.Email,                         // L E-Mail
			r.Phone,                         // M Phone
			r.Payee.Address,                 // N Address Name *
			postal,                          // O Postal code
			r.Payee.AccountNo,               // P *Payee Bank Account Identifier
			bankShortName(r.Payee.BankName), // Q **Bank Name
			r.Payee.BankBranch,              // R **Branch Name
			name,                            // S Account Name (=E)
			nil,                             // T Account Start Date
		}
		if r.StartDate != nil {
			values[19] = beDate(*r.StartDate)
		}
		for col, v := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, n)
			style := plain
			if gapColumns[col] && (v == nil || v == "") {
				style = gapPlain
				if col == 5 || col == 7 || col == 15 {
					style = gapText
				}
				if err := f.SetCellStyle(suppliersSheet, cell, cell, style); err != nil {
					return nil, err
				}
				continue
			}
			switch col {
			case 5, 7, 15: // F, H, P
				style = textStyle
				if s, ok := v.(string); ok {
					if err := f.SetCellStr(suppliersSheet, cell, s); err != nil {
						return nil, err
					}
					v = nil
				}
			case 19:
				style = dateStyle
			}
			if v != nil {
				if err := f.SetCellValue(suppliersSheet, cell, v); err != nil {
					return nil, err
				}
			}
			if err := f.SetCellStyle(suppliersSheet, cell, cell, style); err != nil {
				return nil, err
			}
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gapColumns are the 0-based columns that must be filled for the ERP import
// and that come from encrypted storage, so they can be missing for a TA
// approved before that storage existed: F/H ID, N/O address, P/Q/R bank.
var gapColumns = map[int]bool{5: true, 7: true, 13: true, 14: true, 15: true, 16: true, 17: true}

func parsePostal(s string) (int, error) {
	if len(s) != 5 {
		return 0, errors.New("not a postal code")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a postal code")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func thinBorders() []excelize.Border {
	b := make([]excelize.Border, 0, 4)
	for _, side := range []string{"left", "right", "top", "bottom"} {
		b = append(b, excelize.Border{Type: side, Color: "000000", Style: 1})
	}
	return b
}
