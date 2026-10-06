package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// supplierTA gives taID an approved profile with a citizen ID and, when
// withPayee, the bank account + address — the state UpsertProfile followed by
// staff approval leaves behind.
func (f *tcFixture) supplierTA(taID uuid.UUID, status string, withPayee bool) {
	f.t.Helper()
	f.storeCitizenID(taID, "1234567890121")
	f.exec(`UPDATE ta_profiles SET prefix = 'นางสาว', status = $2::doc_status,
	               verified_at = CASE WHEN $2 = 'approved' THEN '2026-05-29 10:00+07'::timestamptz END
	         WHERE user_id = $1`, taID, status)
	f.exec(`UPDATE users SET phone = '0812345678' WHERE id = $1`, taID)
	if !withPayee {
		return
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := f.docs.storePayee(f.ctx, tx, taID, TAProfile{
		BankName: "ธนาคารไทยพาณิชย์", BankBranch: "มหาวิทยาลัยขอนแก่น",
		AccountNo: "012-3-45678-9", AccountName: "นางสาวใจดี ทดสอบ",
		Address: "123 ม.4 ต.ท่าพระ อ.เมืองขอนแก่น จ.ขอนแก่น", PostalCode: "40260",
	}); err != nil {
		f.t.Fatalf("storePayee: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func TestSuppliers_NewTAsOnlyAndGapsHighlighted(t *testing.T) {
	f := newTCFixture(t)

	// An earlier term, where "returning" was already appointed.
	thisTerm := f.termID
	f.termID = uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months)
	        VALUES ($1, 2568, 2, '2025-11-01', '2026-03-31', FALSE, 4)`, f.termID)
	oldCourse, oldSec, _ := f.insertCourse(tcCourseOpts{Code: "CP000001", Curriculum: "CY", LectureHrs: 10})
	returning := f.newTA("เก่า", "undergrad")
	f.assignTA(returning, oldCourse, oldSec, "undergrad", nil)
	f.termID = thisTerm

	course, sec, _ := f.insertCourse(tcCourseOpts{Code: "CP000002", Curriculum: "CY", LectureHrs: 10})
	ready := f.newTA("ใจดี", "undergrad")
	pending := f.newTA("รอตรวจ", "undergrad")
	noBank := f.newTA("ไม่มีบัญชี", "undergrad")
	for _, id := range []uuid.UUID{returning, ready, pending, noBank} {
		f.assignTA(id, course, sec, "undergrad", nil)
	}
	f.supplierTA(returning, "approved", true)
	f.supplierTA(ready, "approved", true)
	f.supplierTA(pending, "submitted", true)
	f.supplierTA(noBank, "approved", false)

	cands, err := f.docs.SupplierCandidates(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]SupplierCandidate{}
	for _, c := range cands {
		got[c.UserID] = c
	}
	if _, ok := got[returning]; ok {
		t.Error("a TA appointed in an earlier term must not be listed as new")
	}
	if c := got[ready]; !c.ready() {
		t.Errorf("complete new TA not ready: %+v", c)
	}
	if c, ok := got[pending]; !ok || c.ready() {
		t.Errorf("TA with unapproved documents must be listed but not ready: %+v", c)
	}
	// Approved before bank/address were kept: still in the file, flagged.
	if c, ok := got[noBank]; !ok || !c.ready() || c.Incomplete == "" {
		t.Errorf("approved TA with no bank data must be in the file and flagged incomplete: %+v", c)
	}

	actor := f.actor()
	body, n, err := f.docs.BuildSuppliersWorkbook(f.ctx, actor, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("file holds %d TAs, want 2", n)
	}
	wb := openWorkbookBytes(t, body)
	cell := func(ref string) string {
		v, err := wb.GetCellValue(suppliersSheet, ref)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Row order follows the database collation; find each TA by first name.
	readyRow, gapRow := "3", "4"
	if cell("B3") != "ใจดี" {
		readyRow, gapRow = "4", "3"
	}
	// Header rows are the template's own.
	if cell("E1") != "POZ_SUPPLIERS_INT" || cell("P2") != "*Payee Bank Account Identifier" {
		t.Errorf("template headers lost: E1=%q P2=%q", cell("E1"), cell("P2"))
	}
	want := map[string]string{
		"A": "นางสาว", "B": "ใจดี", "C": "", "D": "ทดสอบ", "E": "นางสาวใจดี ทดสอบ",
		"F": "1234567890121", "H": "1234567890121", "I": "student", "J": "วิทยาลัยการคอมพิวเตอร์",
		"K": "ทดสอบ", "M": "0812345678", "N": "123 ม.4 ต.ท่าพระ อ.เมืองขอนแก่น จ.ขอนแก่น",
		"O": "40260", "P": "0123456789", "Q": "ไทยพาณิชย์", "R": "มหาวิทยาลัยขอนแก่น",
		"S": "นางสาวใจดี ทดสอบ", "T": "29/5/2569",
	}
	for col, w := range want {
		if g := cell(col + readyRow); g != w {
			t.Errorf("%s%s = %q, want %q", col, readyRow, g, w)
		}
	}
	// The incomplete TA: identity filled, bank/address blank and yellow.
	if cell("F"+gapRow) != "1234567890121" || cell("E"+gapRow) != "นางสาวไม่มีบัญชี ทดสอบ" {
		t.Errorf("incomplete row identity: E=%q F=%q", cell("E"+gapRow), cell("F"+gapRow))
	}
	for _, col := range []string{"N", "O", "P", "Q", "R"} {
		ref := col + gapRow
		if cell(ref) != "" {
			t.Errorf("%s = %q, want blank", ref, cell(ref))
		}
		id, _ := wb.GetCellStyle(suppliersSheet, ref)
		st, _ := wb.GetStyle(id)
		if st == nil || len(st.Fill.Color) == 0 {
			t.Errorf("%s blank but not highlighted", ref)
		}
	}
	// The sample row is gone; nothing below the data.
	if cell("A5") != "" || cell("E5") != "" {
		t.Error("data below the TA rows")
	}
	// The template's sample row is red in I/N/O/T to flag them for a human;
	// generated rows must be plain black.
	for _, ref := range []string{"I" + readyRow, "N" + readyRow, "O" + readyRow, "T" + readyRow} {
		id, _ := wb.GetCellStyle(suppliersSheet, ref)
		st, err := wb.GetStyle(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Font != nil && st.Font.Color != "" && st.Font.Color != "000000" && st.Font.Color != "FF000000" {
			t.Errorf("%s font colour %q, want black", ref, st.Font.Color)
		}
	}
	if f, _ := wb.GetCellFormula(suppliersSheet, "H"+readyRow); f != "" {
		t.Errorf("H3 is a formula (%q); values only, see export_suppliers.go", f)
	}

	// One export entry; two citizen-ID reveals and one payee reveal (the
	// incomplete TA has no payee to reveal).
	var exports, reveals int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FILTER (WHERE action = 'export.suppliers'),
		       COUNT(*) FILTER (WHERE action IN ('ta_profile.citizen_id.reveal','ta_profile.payee.reveal'))
		  FROM audit_logs WHERE actor_id = $1`, actor).Scan(&exports, &reveals); err != nil {
		t.Fatal(err)
	}
	if exports != 1 || reveals != 3 {
		t.Errorf("audit: %d export, %d reveal rows; want 1 and 3", exports, reveals)
	}
}

func TestSuppliers_NothingReadyIsRefused(t *testing.T) {
	f := newTCFixture(t)
	course, sec, _ := f.insertCourse(tcCourseOpts{Code: "CP000003", Curriculum: "CY", LectureHrs: 10})
	ta := f.newTA("รอตรวจ", "undergrad")
	f.assignTA(ta, course, sec, "undergrad", nil)
	f.supplierTA(ta, "submitted", true)
	if _, _, err := f.docs.BuildSuppliersWorkbook(f.ctx, f.actor(), f.termID); err == nil {
		t.Fatal("expected a refusal when no TA is ready")
	}
}

func TestBEDate_UsesBangkokDay(t *testing.T) {
	// 23:30 UTC on 28 May is already 29 May in Bangkok.
	got := beDate(time.Date(2026, 5, 28, 23, 30, 0, 0, time.UTC))
	if got.Year() != 2569 || got.Month() != 5 || got.Day() != 29 {
		t.Errorf("beDate = %v, want 29/5/2569", got)
	}
}
