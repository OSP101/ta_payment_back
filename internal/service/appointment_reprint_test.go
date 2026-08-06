package service

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Re-issuing a คำสั่ง must produce the document that was already signed, not a
// fresh one built from whatever the database says today. These tests pin that
// distinction, because the two are indistinguishable on the happy path and only
// diverge once the underlying data moves — which is exactly when it matters.

// firstOrderID returns the id of the single order issued by the fixture.
func (f *apptFixture) firstOrderID(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.svc.pool.QueryRow(f.ctx,
		`SELECT id FROM appointment_orders WHERE term_id = $1 ORDER BY round_no LIMIT 1`,
		f.term).Scan(&id); err != nil {
		t.Fatalf("no order recorded: %v", err)
	}
	return id
}

// docxText pulls word/document.xml out of the returned .docx — the only way to
// read back what the reprint actually says.
//
// A .docx IS a zip, so this opens exactly one level. Until 06/08/2026 the
// service handed back a zip WRAPPING the .docx (next to a PDF) and this helper
// opened two; the PDF was dropped and the wrapper with it.
func docxText(t *testing.T, docxBytes []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(docxBytes), int64(len(docxBytes)))
	if err != nil {
		t.Fatalf("open docx: %v", err)
	}
	for _, p := range zr.File {
		if p.Name != "word/document.xml" {
			continue
		}
		prc, err := p.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer prc.Close()
		var doc bytes.Buffer
		if _, err := doc.ReadFrom(prc); err != nil {
			t.Fatal(err)
		}
		return doc.String()
	}
	t.Fatal("no word/document.xml — the returned bytes are not a .docx")
	return ""
}

func TestReprint_ReturnsTheSameBundleAsTheOriginal(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")

	original, name, err := f.svc.Build(f.ctx, uuid.Nil, f.in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	again, name2, err := f.svc.Reprint(f.ctx, uuid.Nil, f.firstOrderID(t))
	if err != nil {
		t.Fatalf("Reprint: %v", err)
	}
	if name2 != name {
		t.Errorf("filename = %q, want the original %q — a copy that arrives under a "+
			"different name looks like a different document", name2, name)
	}
	if docxText(t, again) != docxText(t, original) {
		t.Error("the reprinted document differs from the original")
	}
}

// The point of storing a snapshot. Everything the page prints is re-read at
// render time, so without one a reprint silently picks up edits made since.
func TestReprint_IsUnaffectedByLaterEditsToTheUnderlyingData(t *testing.T) {
	f := newApptFixture(t)
	_, taID := f.addCourseWithTA("CP101", "หนึ่ง", "approved")

	original, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Everything a later correction could plausibly touch: the appointee's name,
	// the course's credit hours, and the signer's title.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE users SET first_name='เปลี่ยนชื่อแล้ว' WHERE id=$1`, []any{taID}},
		{`UPDATE teaching_courses SET credits=99, lecture_hrs=9 WHERE term_id=$1`, []any{f.term}},
		{`UPDATE admin_officers SET title='คณบดีคนใหม่', full_name='ผู้ลงนามคนใหม่'`, nil},
	} {
		if _, err := f.svc.pool.Exec(f.ctx, q.sql, q.args...); err != nil {
			t.Fatalf("mutate: %v", err)
		}
	}

	again, _, err := f.svc.Reprint(f.ctx, uuid.Nil, f.firstOrderID(t))
	if err != nil {
		t.Fatalf("Reprint: %v", err)
	}
	doc := docxText(t, again)
	if doc != docxText(t, original) {
		t.Error("the reprint changed after the source data was edited — it is being " +
			"rebuilt from live tables instead of from the stored snapshot")
	}
	for _, leaked := range []string{"เปลี่ยนชื่อแล้ว", "99 (9-", "คณบดีคนใหม่", "ผู้ลงนามคนใหม่"} {
		if strings.Contains(doc, leaked) {
			t.Errorf("the copy carries %q, which was written AFTER the order was signed", leaked)
		}
	}
	// ...and the original wording is still there, so the test above cannot pass
	// by the reprint being empty.
	if !strings.Contains(doc, "หนึ่ง") {
		t.Error("the appointee's name as printed is missing from the copy")
	}
}

// A reprint is a copy, not a new act: it must not consume a round number or
// re-appoint anyone, or the next real round would skip names.
func TestReprint_DoesNotIssueANewRound(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var beforeOrders, beforeItems int
	f.svc.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM appointment_orders`).Scan(&beforeOrders)
	f.svc.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM appointment_order_items`).Scan(&beforeItems)

	if _, _, err := f.svc.Reprint(f.ctx, uuid.Nil, f.firstOrderID(t)); err != nil {
		t.Fatalf("Reprint: %v", err)
	}

	var afterOrders, afterItems int
	f.svc.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM appointment_orders`).Scan(&afterOrders)
	f.svc.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM appointment_order_items`).Scan(&afterItems)
	if afterOrders != beforeOrders || afterItems != beforeItems {
		t.Errorf("reprint wrote to the ledger (orders %d→%d, items %d→%d) — "+
			"it must be a copy, not a new round",
			beforeOrders, afterOrders, beforeItems, afterItems)
	}

	next, err := f.svc.nextRoundNo(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Errorf("next round = %d, want 2 — a reprint must not consume a round number", next)
	}
}

// Reprints are audited: the ledger says who ISSUED an order, not who else has
// walked away with a copy of it.
func TestReprint_IsAudited(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, _, err := f.svc.Reprint(f.ctx, uuid.Nil, f.firstOrderID(t)); err != nil {
		t.Fatalf("Reprint: %v", err)
	}

	var n int
	if err := f.svc.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE action='appointment_order.reprint'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reprint audit rows = %d, want 1", n)
	}
}

// Orders printed before snapshots existed have nothing to copy. Refusing is the
// honest answer; rebuilding from today's tables would hand over a "copy" that
// may not match the paper it claims to reproduce.
func TestReprint_RefusesAnOrderWithNoStoredSnapshot(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	id := f.firstOrderID(t)
	if _, err := f.svc.pool.Exec(f.ctx,
		`UPDATE appointment_orders SET document=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	_, _, err := f.svc.Reprint(f.ctx, uuid.Nil, id)
	if err == nil {
		t.Fatal("an order with no stored snapshot must be refused, not rebuilt")
	}
	if !strings.Contains(err.Error(), "สำเนา") {
		t.Errorf("the refusal should explain that no copy was kept, got: %v", err)
	}
}

func TestReprint_RefusesAnUnknownOrder(t *testing.T) {
	f := newApptFixture(t)
	if _, _, err := f.svc.Reprint(f.ctx, uuid.Nil, uuid.New()); err == nil {
		t.Fatal("an order id that does not exist must be refused")
	}
}

// The history list drives the button, so it has to know which rows are
// reprintable — otherwise staff click and get an error.
func TestListRounds_FlagsWhichOrdersCanBeReprinted(t *testing.T) {
	f := newApptFixture(t)
	f.addCourseWithTA("CP101", "หนึ่ง", "approved")
	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	rounds, err := f.svc.ListRounds(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(rounds))
	}
	if !rounds[0].CanReprint {
		t.Error("an order issued today stores its document and must be reprintable")
	}

	if _, err := f.svc.pool.Exec(f.ctx, `UPDATE appointment_orders SET document=NULL`); err != nil {
		t.Fatal(err)
	}
	rounds, err = f.svc.ListRounds(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if rounds[0].CanReprint {
		t.Error("an order with no snapshot must not advertise a button that will fail")
	}
}
