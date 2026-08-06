package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// The ผู้รับรอง block on the claim form is signed by the head of department, not
// by the course's lecturer — the exporter used to write the lecturer's name
// under the template's hardcoded "ตำแหน่ง หัวหน้าสาขาวิชา…" line, which had
// every form asserting that a lecturer had certified their own TA's hours in a
// seat they do not hold.

type certFixture struct {
	svc  *ExportService
	ctx  context.Context
	pool *pgxpool.Pool
	term uuid.UUID
	// actor is a real user row: audit_logs.actor_id is a FK, and SetCertifier
	// is audited.
	actor uuid.UUID
}

func newCertFixture(t *testing.T, officers ...[2]string) (*certFixture, []uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	f := &certFixture{
		svc:   &ExportService{pool: pool, aud: audit.New(pool)},
		ctx:   ctx,
		pool:  pool,
		term:  uuid.New(),
		actor: uuid.New(),
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id,email,first_name,last_name,is_active)
		 VALUES ($1,$2,'เจ้าหน้าที่','ทดสอบ',TRUE)`,
		f.actor, "staff-"+f.actor.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1,2569,1,TRUE)`,
		f.term); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 0, len(officers))
	for _, o := range officers {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO admin_officers (id, academic_prefix, full_name, title, is_active)
			 VALUES ($1,'ผศ. ดร.',$2,$3,TRUE)`, id, o[0], o[1]); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return f, ids
}

func TestIsHeadTitle(t *testing.T) {
	cases := []struct {
		title string
		want  bool
	}{
		{"หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", true},
		{"  หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์ ", true},
		// Contains "หัวหน้าสาขา" but is a deputy — a substring test would hand
		// them the seat outright.
		{"รองหัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", false},
		{"คณบดีวิทยาลัยการคอมพิวเตอร์", false},
		{"รองคณบดีฝ่ายบริหาร", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsHeadTitle(c.title); got != c.want {
			t.Errorf("IsHeadTitle(%q) = %v, want %v", c.title, got, c.want)
		}
	}
}

// With nothing chosen, the form should already be right: whoever holds the seat
// certifies. Staff should not have to configure anything on day one.
func TestResolveCertifier_DefaultsToTheHeadOfDepartment(t *testing.T) {
	f, _ := newCertFixture(t,
		[2]string{"วรัญญา วรรณศรี", "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"},
		[2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"},
	)

	c, err := f.svc.ResolveCertifier(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Resolved || c.Name != "ผศ. ดร.วรัญญา วรรณศรี" {
		t.Errorf("certifier = %+v, want the head of department", c)
	}
	if c.ActingFor != "" {
		t.Errorf("ActingFor = %q, want empty — the seat holder is not acting", c.ActingFor)
	}
	if c.PositionLine() != "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์" {
		t.Errorf("position line = %q", c.PositionLine())
	}
}

// Choosing anyone else prints the acting form, exactly as the appointment order
// does for the dean's seat.
func TestResolveCertifier_ChosenDeputyCertifiesAsActing(t *testing.T) {
	f, ids := newCertFixture(t,
		[2]string{"วรัญญา วรรณศรี", "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"},
		[2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"},
	)
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, &ids[1]); err != nil {
		t.Fatal(err)
	}

	c, err := f.svc.ResolveCertifier(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "ผศ. ดร.ณกร วัฒนกิจ" {
		t.Errorf("name = %q — the deputy certifies under their OWN name", c.Name)
	}
	if c.TitleLine != "รองคณบดีฝ่ายวิชาการ รักษาการแทน" {
		t.Errorf("title line = %q, want the acting form", c.TitleLine)
	}
	if c.ActingFor != "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์" {
		t.Errorf("acting for = %q, want the head-of-department seat — NOT the dean's, "+
			"which is the seat the other document uses", c.ActingFor)
	}
	if want := "รองคณบดีฝ่ายวิชาการ รักษาการแทน หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"; c.PositionLine() != want {
		t.Errorf("position line = %q, want %q", c.PositionLine(), want)
	}
}

// Choosing the head of department explicitly must read exactly like the default
// — no stray acting phrase for the person who holds the seat.
func TestResolveCertifier_ChosenHeadIsNotMarkedActing(t *testing.T) {
	f, ids := newCertFixture(t,
		[2]string{"วรัญญา วรรณศรี", "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"},
	)
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, &ids[0]); err != nil {
		t.Fatal(err)
	}

	c, err := f.svc.ResolveCertifier(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if c.ActingFor != "" || c.TitleLine != "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์" {
		t.Errorf("choosing the seat holder produced %+v, want the plain form", c)
	}
}

// Clearing the choice returns the term to following the seat, so a new head of
// department is picked up without anyone revisiting the screen.
func TestResolveCertifier_ClearingTheChoiceFollowsTheSeatAgain(t *testing.T) {
	f, ids := newCertFixture(t,
		[2]string{"วรัญญา วรรณศรี", "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"},
		[2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"},
	)
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, &ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, nil); err != nil {
		t.Fatal(err)
	}

	c, err := f.svc.ResolveCertifier(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if c.OfficerID != nil || c.Name != "ผศ. ดร.วรัญญา วรรณศรี" {
		t.Errorf("after clearing, certifier = %+v, want the seat holder with no explicit id", c)
	}
}

// No head on the roster and no choice: the block must stay blank for a wet
// signature rather than borrow a name from somewhere else.
func TestResolveCertifier_UnresolvedWhenNobodyHoldsTheSeat(t *testing.T) {
	f, _ := newCertFixture(t, [2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"})

	c, err := f.svc.ResolveCertifier(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if c.Resolved || c.Name != "" {
		t.Errorf("certifier = %+v, want unresolved — printing a guess above a "+
			"signature line is worse than leaving it blank", c)
	}
}

// An inactive officer must not be recorded as certifier: the form would print
// somebody the college has already retired from the roster.
func TestSetCertifier_RefusesAnInactiveOfficer(t *testing.T) {
	f, ids := newCertFixture(t, [2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"})
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE admin_officers SET is_active=FALSE WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, &ids[0]); err == nil {
		t.Fatal("an inactive officer must not be recorded as certifier")
	}
}

func TestSetCertifier_RefusesAnUnknownOfficer(t *testing.T) {
	f, _ := newCertFixture(t)
	stranger := uuid.New()
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, &stranger); err == nil {
		t.Fatal("an officer id that is not on the roster must be refused")
	}
}

// A certifier deleted after being chosen must not break every export in the
// term — the form falls back to the seat holder.
func TestResolveCertifier_SurvivesTheChosenOfficerBeingDeleted(t *testing.T) {
	f, ids := newCertFixture(t,
		[2]string{"วรัญญา วรรณศรี", "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"},
		[2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"},
	)
	if err := f.svc.SetCertifier(f.ctx, f.actor, f.term, &ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE academic_terms SET certifier_officer_id=NULL WHERE id=$1`, f.term); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM admin_officers WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}

	c, err := f.svc.ResolveCertifier(f.ctx, f.term)
	if err != nil {
		t.Fatalf("a deleted certifier must not break the export: %v", err)
	}
	if c.Name != "ผศ. ดร.วรัญญา วรรณศรี" {
		t.Errorf("certifier = %+v, want the seat holder", c)
	}
}

// The two cells the claim form's ผู้รับรอง block receives. This is the decision
// that was wrong before: the name above that signature, and the position line
// under it.
func TestCertifierClaimCells(t *testing.T) {
	head := CertifierChoice{
		Name: "ผศ. ดร.วรัญญา วรรณศรี", TitleLine: "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", Resolved: true,
	}
	name, pos, ok := head.ClaimCells()
	if !ok {
		t.Fatal("a resolved certifier must fill the block")
	}
	if name != "(ผศ. ดร.วรัญญา วรรณศรี)" {
		t.Errorf("name cell = %q — brackets match the template's other signature cells", name)
	}
	if pos != "ตำแหน่ง หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์" {
		t.Errorf("position cell = %q", pos)
	}

	acting := CertifierChoice{
		Name:      "ผศ. ดร.ณกร วัฒนกิจ",
		TitleLine: "รองคณบดีฝ่ายวิชาการ รักษาการแทน",
		ActingFor: "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์",
		Resolved:  true,
	}
	_, pos, ok = acting.ClaimCells()
	if !ok {
		t.Fatal("an acting certifier still signs")
	}
	if pos != "ตำแหน่ง รองคณบดีฝ่ายวิชาการ รักษาการแทน หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์" {
		t.Errorf("position cell = %q — a deputy must not be labelled with the seat "+
			"outright, which is what the template's hardcoded line does", pos)
	}

	// Unresolved must NOT write: the template already carries a blank signature
	// line and the seat's name, which is the correct thing to hand to a human.
	if _, _, ok := (CertifierChoice{}).ClaimCells(); ok {
		t.Error("an unresolved certifier must leave the template's cells alone")
	}
	if _, _, ok := (CertifierChoice{Resolved: true}).ClaimCells(); ok {
		t.Error("a resolved-but-nameless certifier must not print empty brackets")
	}
}

// The cell ADDRESSES. Unit-testing the wording proves nothing if the values go
// into the wrong squares, and the template is the only thing that knows which
// squares belong to ผู้รับรอง — H33 labels the block, H35 is its signature rule,
// H36 the name, H37 the position line.
func TestClaimTemplate_CertifierCellsAreWhereWeWriteThem(t *testing.T) {
	path := filepath.Join(repoRoot(t), "assets", "templates", "ta_claim_form.xlsx")
	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Skipf("claim template not available: %v", err)
	}
	defer f.Close()

	const sheet = "ภาคปกติ"
	label, err := f.GetCellValue(sheet, "H33")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(label, "ผู้รับรอง") {
		t.Fatalf("H33 = %q, want the ผู้รับรอง block label the cells below it are "+
			"the ones the exporter writes, so if this moved the export is now "+
			"writing into somebody else's signature", label)
	}
	// D33/E33 is the lecturer's block. If these ever collide, the export would
	// be back to printing one person in both places.
	lect, _ := f.GetCellValue(sheet, "E33")
	if !strings.Contains(lect, "อาจารย์ผู้สอน") {
		t.Errorf("E33 = %q, want the lecturer block label", lect)
	}
	// The hardcoded seat the exporter overwrites when the certifier is acting.
	pos, _ := f.GetCellValue(sheet, "H37")
	if !strings.Contains(pos, "หัวหน้าสาขา") {
		t.Errorf("H37 = %q, want the head-of-department position line", pos)
	}

	// Round-trip the two writes the exporter makes, and read them back.
	c := CertifierChoice{
		Name: "ผศ. ดร.ณกร วัฒนกิจ", TitleLine: "รองคณบดีฝ่ายวิชาการ รักษาการแทน",
		ActingFor: "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", Resolved: true,
	}
	name, position, ok := c.ClaimCells()
	if !ok {
		t.Fatal("resolved certifier must produce cells")
	}
	if err := f.SetCellValue(sheet, "H36", name); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue(sheet, "H37", position); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.GetCellValue(sheet, "H36"); got != name {
		t.Errorf("H36 read back as %q, want %q", got, name)
	}
	if got, _ := f.GetCellValue(sheet, "H37"); got != position {
		t.Errorf("H37 read back as %q, want %q", got, position)
	}
	// The lecturer's own cell must be untouched by any of this.
	if got, _ := f.GetCellValue(sheet, "D36"); strings.Contains(got, "ณกร") {
		t.Error("writing the certifier leaked into the lecturer's signature cell")
	}
}
