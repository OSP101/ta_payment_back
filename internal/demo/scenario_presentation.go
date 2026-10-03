package demo

// scenario_presentation.go — "ชุดข้อมูลนำเสนอผู้บริหาร" (26/09/2026).
//
// The happy path (scenario_steps.go) walks three courses through one month:
// right for teaching the system, too thin to present the executive
// dashboard, where most panels need variety to say anything. This button
// loads a SEPARATE term of about two dozen fictional courses across all six
// curricula, with every case the dashboard is built to answer:
//
//   money     a course over its cap, a course near it (≥ NearCapRatio)
//   staffing  over the ceiling, above the guide, as recommended, under it,
//             no student count, and big courses that never asked
//   work      a request awaiting a TA timetable, a rejected request, TAs
//             approved after the appointment order, months at every step
//             (with the TA, with the lecturer, staff review, ready to
//             export, exported, sent back), returned TA profiles, and a
//             holiday some lecturers have not rescheduled
//   time      several months of history plus the month in progress
//
// Where the data comes from — the approved rules (26/09/2026):
//   - Everything a real user can do today goes through the real service:
//     term, courses, TA timetables, TA requests (the auto-decider approves,
//     rejects or holds them exactly as in production), the appointment
//     order, lecturer approve/reject, staff review, export, send-back,
//     holidays and makeups. Those rows are the service's own rows.
//   - Three things cannot be made that way and are written as SQL in the
//     shape the service writes them: the fictional users (seedSlot's own
//     pattern), TA profile STATUS rows (no personal data, no documents —
//     the TA's ID card, bank and signature columns stay NULL), and the work
//     logs of past months, inserted as the TA would have submitted them
//     (source 'auto', status 'submitted', submitted_at on the day after the
//     class). A closed month cannot be logged into by design, so history
//     has no other way in. Approval, review and export then run through
//     the services on top of them.
//   - No money is written anywhere. Every baht on the dashboard is priced
//     at read time by the settlement, from these hours.
//
// Isolation (fail closed): every entry point checks assertDemoSlot — the
// container must be a demo slot's (Cfg.IsDemoSlot), the schema a demo_slot_N
// name, and the pool's current schema that same slot. Raw SQL additionally
// runs in a transaction whose search_path is pinned to the slot schema ALONE
// (no public fallback), re-checked before each write, so a table missing
// from the slot can never silently resolve to the real one.
//
// Names are visibly fictional: course codes start with DM (no real faculty
// uses it), every course and person carries "(สมมติ)", and accounts live
// under @presentation.demo.local with no password (nobody can log in as them).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/service"
	"ta-payment-back/internal/timeutil"
)

// PresentationSemester is the semester number the dataset's term uses, so
// it never collides with the happy path's semester 1.
const PresentationSemester = 2

// presentationEmailDomain marks every account this dataset creates.
const presentationEmailDomain = "presentation.demo.local"

var errNotDemoSlot = errors.New("demo: ชุดข้อมูลนำเสนอเขียนได้เฉพาะในห้องทดลอง (demo slot) เท่านั้น ระบบปฏิเสธเพื่อไม่ให้แตะข้อมูลจริง")

var demoSlotSchema = regexp.MustCompile(`^demo_slot_[0-9]+$`)

// assertDemoSlot refuses anything that is not provably a demo slot. Called
// before every phase that writes, and inside every raw-SQL transaction.
func assertDemoSlot(ctx context.Context, slot *Slot) error {
	if slot == nil || slot.Pool == nil || slot.Container == nil || slot.Container.Pool == nil {
		return errNotDemoSlot
	}
	if !slot.Container.Cfg.IsDemoSlot || !demoSlotSchema.MatchString(slot.SchemaName) {
		return errNotDemoSlot
	}
	if slot.Container.Pool != slot.Pool {
		return errNotDemoSlot
	}
	var cur string
	if err := slot.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&cur); err != nil {
		return err
	}
	if cur != slot.SchemaName {
		return errNotDemoSlot
	}
	return nil
}

// slotExec runs one raw write inside a transaction pinned to the slot
// schema alone. See the file comment.
func slotExec(ctx context.Context, slot *Slot, fn func(tx pgx.Tx) error) error {
	if err := assertDemoSlot(ctx, slot); err != nil {
		return err
	}
	tx, err := slot.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+pgx.Identifier{slot.SchemaName}.Sanitize()); err != nil {
		return err
	}
	var cur string
	if err := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&cur); err != nil {
		return err
	}
	if cur != slot.SchemaName {
		return errNotDemoSlot
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- the dataset ----------------------------------------------------------

type presSection struct {
	students   int
	track      string // regular | special
	day        int    // 1 = Monday … 6 = Saturday
	start, end string
}

type presCourse struct {
	code, name, curriculum string
	// lecHrs is the course's lecture-hours figure, which the budget formula
	// reads as credits (cap = rate × lecHrs × students ÷ baseline × months).
	// Tuned per course so the money cases land where the comment says.
	lecHrs   int
	sections []presSection
	// tas is how many TAs the lecturer asks for; 0 = no request.
	tas int
	// grads adds this many master's TAs on the special-track section
	// (graduate-special lump sum).
	grads int
	// phase: 1 = approved and on the appointment order; 2 = approved after
	// the order (waiting for the next one); "pending" = submitted, waiting for
	// a TA timetable; "rejected" = the auto-decider refuses it.
	phase    string
	lecturer int
	case_    string // what this course demonstrates (documentation only)
}

func sec(n, day int, start, end string) presSection {
	return presSection{students: n, track: "regular", day: day, start: start, end: end}
}

// Budget cap with the sandbox's pay rates (seed.go): 3 h/credit × students ÷
// 60 × 300 ฿ × the term's 6 months = 90 × lecHrs × students. A TA's lecture
// hour pays 40 ฿, a 3-hour class 120 ฿ a week. The term is anchored on
// today's week, so each TA has taught the same number of classes on any day
// the sandbox is opened, give or take this week's class.
var presentationCourses = []presCourse{
	// ---- money cases ----
	// Robust on any weekday: 3 TAs × 11–12 Tuesday classes × 120 ฿ =
	// 3,960–4,320 ฿ against a cap of 90 × 2 × 20 = 3,600 ฿ — always over.
	{code: "DM120201", name: "ปฏิบัติการเครือข่ายคอมพิวเตอร์ (สมมติ)", curriculum: "IT", lecHrs: 2,
		sections: []presSection{sec(20, 2, "13:00", "16:00")}, tas: 3, phase: "1", lecturer: 1,
		case_: "เกินเพดานต่อนักศึกษา และชนเพดานงบ (งานที่ยังจ่ายไม่ได้)"},
	// 3 TAs × 10–11 Wednesday classes (the holiday and midterm weeks off) ×
	// 120 ฿ = 3,600–3,960 ฿ against a cap of 90 × 1 × 44 = 3,960 ฿ — 91–100%,
	// near the cap but never over it.
	{code: "DM140401", name: "การเรียนรู้ของเครื่อง (สมมติ)", curriculum: "AI", lecHrs: 1,
		sections: []presSection{sec(44, 3, "09:00", "12:00")}, tas: 3, phase: "1", lecturer: 2,
		case_: "ขอมากกว่าที่แนะนำ และใกล้เพดานงบ"},
	{code: "DM130302", name: "การสำรวจข้อมูลระยะไกล (สมมติ)", curriculum: "GIS", lecHrs: 1,
		sections: []presSection{sec(50, 4, "13:00", "16:00")}, tas: 2, phase: "1", lecturer: 3,
		case_: "ตามที่แนะนำ แต่ใกล้เพดานงบ"},

	// ---- staffing cases ----
	{code: "DM130301", name: "ระบบสารสนเทศภูมิศาสตร์ (สมมติ)", curriculum: "GIS", lecHrs: 3,
		sections: []presSection{sec(45, 1, "13:00", "16:00")}, tas: 4, phase: "1", lecturer: 3,
		case_: "เกินเพดานต่อนักศึกษา"},
	{code: "DM150501", name: "ความปลอดภัยเครือข่าย (สมมติ)", curriculum: "CY", lecHrs: 3,
		sections: []presSection{sec(55, 2, "09:00", "12:00")}, tas: 4, phase: "1", lecturer: 4,
		case_: "ขอมากกว่าที่แนะนำ"},
	{code: "DM110102", name: "โครงสร้างข้อมูลและอัลกอริทึม (สมมติ)", curriculum: "CS", lecHrs: 3,
		sections: []presSection{sec(70, 3, "13:00", "16:00")}, tas: 4, phase: "1", lecturer: 5,
		case_: "ขอมากกว่าที่แนะนำ"},
	{code: "DM110103", name: "ระบบฐานข้อมูล (สมมติ)", curriculum: "CS", lecHrs: 3,
		sections: []presSection{sec(50, 4, "09:00", "12:00")}, tas: 2, phase: "1", lecturer: 6,
		case_: "ตามที่แนะนำ"},
	{code: "DM120202", name: "การพัฒนาเว็บแอปพลิเคชัน (สมมติ)", curriculum: "IT", lecHrs: 3,
		sections: []presSection{sec(75, 5, "09:00", "12:00")}, tas: 3, phase: "1", lecturer: 7,
		case_: "ตามที่แนะนำ"},
	{code: "DM120203", name: "ระบบปฏิบัติการ (สมมติ)", curriculum: "IT", lecHrs: 3,
		sections: []presSection{sec(25, 1, "09:00", "12:00")}, tas: 1, phase: "1", lecturer: 1,
		case_: "ตามที่แนะนำ"},
	{code: "DM140402", name: "ปัญญาประดิษฐ์เบื้องต้น (สมมติ)", curriculum: "AI", lecHrs: 3,
		sections: []presSection{sec(48, 5, "13:00", "16:00")}, tas: 2, phase: "1", lecturer: 8,
		case_: "ตามที่แนะนำ"},
	{code: "DM150502", name: "นิติวิทยาการคอมพิวเตอร์ (สมมติ)", curriculum: "CY", lecHrs: 3,
		sections: []presSection{sec(30, 3, "09:00", "12:00")}, tas: 2, phase: "1", lecturer: 4,
		case_: "ตามที่แนะนำ (มีคาบวันหยุดยังไม่ชดเชย)"},
	{code: "DM110104", name: "คณิตศาสตร์ดิสครีต (สมมติ)", curriculum: "CS", lecHrs: 3,
		sections: []presSection{sec(40, 3, "13:00", "16:00"), sec(40, 4, "13:00", "16:00")}, tas: 4, phase: "1", lecturer: 9,
		case_: "สองกลุ่มเรียนคนละเวลา แนะนำกลุ่มละ 2 คน"},
	{code: "DM190102", name: "สถิติสำหรับนักวิทยาการข้อมูล (สมมติ)", curriculum: "OTHER", lecHrs: 3,
		sections: []presSection{sec(45, 2, "09:00", "12:00")}, tas: 2, phase: "1", lecturer: 10,
		case_: "ตามที่แนะนำ"},
	{code: "DM110105", name: "วิศวกรรมซอฟต์แวร์ (สมมติ)", curriculum: "CS", lecHrs: 3,
		sections: []presSection{sec(90, 1, "13:00", "16:00")}, tas: 1, phase: "1", lecturer: 11,
		case_: "ขอน้อยกว่าที่แนะนำ"},
	{code: "DM140403", name: "การประมวลผลภาษาธรรมชาติ (สมมติ)", curriculum: "AI", lecHrs: 3,
		sections: []presSection{sec(60, 5, "09:00", "12:00")}, tas: 1, phase: "1", lecturer: 2,
		case_: "ขอน้อยกว่าที่แนะนำ"},
	{code: "DM150503", name: "การบริหารความเสี่ยงไซเบอร์ (สมมติ)", curriculum: "CY", lecHrs: 3,
		sections: []presSection{sec(35, 4, "09:00", "12:00"), {students: 40, track: "special", day: 6, start: "09:00", end: "12:00"}},
		tas:      1, grads: 1, phase: "1", lecturer: 12,
		case_: "มีภาคพิเศษ บัณฑิตรับเหมาจ่าย"},

	// ---- work-flow cases ----
	{code: "DM120204", name: "การวิเคราะห์ข้อมูลขนาดใหญ่ (สมมติ)", curriculum: "IT", lecHrs: 3,
		sections: []presSection{sec(80, 2, "13:00", "16:00")}, tas: 2, phase: "2", lecturer: 13,
		case_: "อนุมัติหลังออกคำสั่งแต่งตั้ง รอคำสั่งรอบถัดไป"},
	{code: "DM140404", name: "หัวข้อพิเศษทางปัญญาประดิษฐ์ (สมมติ)", curriculum: "AI", lecHrs: 3,
		sections: []presSection{sec(0, 1, "09:00", "12:00")}, tas: 1, phase: "2", lecturer: 8,
		case_: "ยังไม่มีจำนวนนักศึกษา"},
	{code: "DM120205", name: "เทคโนโลยีคลาวด์ (สมมติ)", curriculum: "IT", lecHrs: 3,
		sections: []presSection{sec(40, 5, "13:00", "16:00")}, tas: 1, phase: "pending", lecturer: 14,
		case_: "คำขอรอตารางเรียนของ TA"},
	{code: "DM130303", name: "แผนที่ดิจิทัล (สมมติ)", curriculum: "GIS", lecHrs: 3,
		sections: []presSection{sec(35, 3, "09:00", "12:00")}, tas: 1, phase: "rejected", lecturer: 3,
		case_: "คำขอถูกปฏิเสธ (TA รับครบ 3 วิชาแล้ว)"},

	// ---- no request ----
	{code: "DM110106", name: "หลักการคอมพิวเตอร์ (สมมติ)", curriculum: "CS", lecHrs: 3,
		sections: []presSection{sec(90, 1, "13:00", "16:00"), sec(90, 1, "13:00", "16:00")}, lecturer: 15,
		case_: "วิชาใหญ่ที่ยังไม่ขอ TA"},
	{code: "DM190101", name: "การรู้ดิจิทัล (สมมติ)", curriculum: "OTHER", lecHrs: 2,
		sections: []presSection{sec(150, 4, "13:00", "15:00")}, lecturer: 16,
		case_: "วิชาใหญ่ที่ยังไม่ขอ TA"},
	{code: "DM120206", name: "สัมมนาเทคโนโลยีสารสนเทศ (สมมติ)", curriculum: "IT", lecHrs: 1,
		sections: []presSection{sec(15, 5, "15:00", "16:00")}, lecturer: 7,
		case_: "วิชาเล็กที่ไม่ขอ TA"},
	{code: "DM150504", name: "จริยธรรมและกฎหมายไซเบอร์ (สมมติ)", curriculum: "CY", lecHrs: 2,
		sections: []presSection{sec(40, 2, "13:00", "15:00")}, lecturer: 12,
		case_: "ไม่ขอ TA"},
}

const (
	presLecturers  = 16
	presUndergrads = 34
	presGrads      = 1
)

var thaiFirst = []string{"กมล", "ขวัญใจ", "จิรา", "ชนะ", "ณัฐ", "ดารา", "ธนา", "นภา", "บุญมี", "ปรีชา", "พิมพ์", "มานะ", "ยุพา", "รัตนา", "ลำดวน", "วิชัย", "ศรีสุข", "สมพร", "อรุณ", "เอกชัย"}

func presName(i int) string { return thaiFirst[i%len(thaiFirst)] }

// ---- entry points -----------------------------------------------------------

// PresentationStatus reports whether this slot already holds the dataset.
func PresentationStatus(ctx context.Context, slot *Slot) (loaded bool, label string, err error) {
	if err := assertDemoSlot(ctx, slot); err != nil {
		return false, "", err
	}
	var year int
	err = slot.Pool.QueryRow(ctx, `
		SELECT t.academic_year FROM academic_terms t
		WHERE t.semester = $1 AND EXISTS (
		    SELECT 1 FROM teaching_courses tc WHERE tc.term_id = t.id AND tc.code LIKE 'DM%')`,
		PresentationSemester).Scan(&year)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("%d/%d", year, PresentationSemester), nil
}

// SeedPresentation loads the dataset into this slot. Refuses outside a demo
// slot and when it is already there; never touches the happy-path term.
func SeedPresentation(ctx context.Context, slot *Slot) (string, error) {
	return seedPresentationAt(ctx, slot, timeutil.Now())
}

// seedPresentationAt anchors the term on `now` — the test runs it on several
// weekdays, since how many classes each TA has taught shifts with the day.
func seedPresentationAt(ctx context.Context, slot *Slot, now time.Time) (string, error) {
	if err := assertDemoSlot(ctx, slot); err != nil {
		return "", err
	}
	if loaded, label, err := PresentationStatus(ctx, slot); err != nil {
		return "", err
	} else if loaded {
		return fmt.Sprintf("มีชุดข้อมูลนำเสนออยู่แล้วในภาคเรียน %s เลือกภาคเรียนนี้ที่มุมขวาบนเพื่อดู หากต้องการโหลดใหม่ ให้กดรีเซ็ตห้องทดลองก่อน", label), nil
	}
	p := &presBuilder{slot: slot, svc: slot.Container, now: now}
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"ภาคเรียน", p.term},
		{"บัญชีสมมติ", p.users},
		{"ตารางเรียนของ TA", p.taTimetables},
		{"รายวิชา", p.courses},
		{"คำขอ TA รอบแรก", func(ctx context.Context) error { return p.requests(ctx, "1") }},
		{"คำสั่งแต่งตั้ง", p.appointment},
		{"คำขอ TA หลังออกคำสั่ง", func(ctx context.Context) error { return p.requests(ctx, "2", "pending", "rejected") }},
		{"โปรไฟล์ TA", p.profiles},
		{"วันหยุดและการชดเชย", p.holiday},
		{"รอบส่งรายเดือน", p.periods},
		{"บันทึกเวลาย้อนหลัง", p.worklogs},
		{"อนุมัติ ตรวจ และส่งออก", p.workflow},
		{"เอกสาร TA ที่ถูกตีกลับ", p.returnProfiles},
		{"ปิดรอบที่เลยกำหนด", p.closePeriods},
	}
	for _, st := range steps {
		if err := assertDemoSlot(ctx, slot); err != nil {
			return "", err
		}
		if err := st.fn(ctx); err != nil {
			return "", fmt.Errorf("ชุดข้อมูลนำเสนอ (%s): %w", st.name, err)
		}
	}
	return fmt.Sprintf(
		"โหลดชุดข้อมูลนำเสนอแล้ว: ภาคเรียน %d/%d มี %d วิชา %d หลักสูตร TA %d คน ข้อมูล %d เดือน "+
			"เลือกภาคเรียนนี้ที่มุมขวาบน แล้วเปิดหน้าแดชบอร์ดหรือมุมมองผู้บริหาร ทุกรายการเป็นข้อมูลสมมติ",
		p.year, PresentationSemester, len(presentationCourses), 6, presUndergrads+presGrads, len(p.months)), nil
}

// ---- builder ----------------------------------------------------------------

type presBuilder struct {
	slot *Slot
	// returned are the TAs whose documents the dashboard shows as sent back.
	// They are seeded approved and returned only after the workflow step,
	// because staff-review refuses a month for a TA whose documents are not
	// approved — the case is "paid months, then a document bounced".
	returned []returnedProfile
	svc  *service.Container
	now  time.Time

	adminID, staffID uuid.UUID
	termID           uuid.UUID
	year             int
	start, end       time.Time
	midStart, midEnd time.Time
	holidayDate      time.Time

	lecturers []uuid.UUID
	tas       []uuid.UUID // undergrads
	grads     []uuid.UUID
	pendingTA uuid.UUID // no timetable: holds a request at 'submitted'
	capTA     uuid.UUID // three courses already: the fourth request is rejected

	courseIDs map[string]uuid.UUID
	sections  map[string][]uuid.UUID
	// taOf lists, per course code, the TA of each assignment in section order.
	taOf       map[string][]presAssign
	months     []presMonth
	periodByYM map[string]*service.SubmissionPeriod // by Gregorian YYYY-MM
}

type presAssign struct {
	ta, section uuid.UUID
	sec         presSection
	grad        bool
	assignment  uuid.UUID
}

type presMonth struct {
	ym      string // Gregorian "2026-07"
	first   time.Time
	current bool
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, timeutil.Bangkok)
}

// term: 18 teaching weeks, the 13th of which is this week — about three
// quarters through, so the pace line, the history and the month in progress
// all have something to show on whatever day the sandbox is opened.
func (p *presBuilder) term(ctx context.Context) error {
	var err error
	if p.adminID, err = userIDByEmail(ctx, p.svc, "admin@demo.local"); err != nil {
		return err
	}
	if p.staffID, err = userIDByEmail(ctx, p.svc, "staff@demo.local"); err != nil {
		return err
	}
	today := dateOnly(p.now)
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	p.start = monday.AddDate(0, 0, -12*7)
	p.end = p.start.AddDate(0, 0, 18*7-1)
	p.midStart = p.start.AddDate(0, 0, 7*7)
	p.midEnd = p.midStart.AddDate(0, 0, 4)
	finalStart := p.end.AddDate(0, 0, -12)
	p.year = p.start.Year() + 543
	// Fixed, not counted off the calendar: 18 weeks touch five or six calendar
	// months depending on where the anchor week falls, and every budget cap in
	// presentationCourses is tuned for six.
	const months = 6
	// UpsertTerm bounds months by the calendar months the dates touch (a
	// real term's months multiplies every budget), so on a five-month anchor
	// the term is created with five and then set to the six the caps are tuned
	// for, by SQL like the rest of this dataset's history.
	created := months
	if span := service.TermMonthSpan(p.start, p.end); span < created {
		created = span
	}
	in := service.Term{
		AcademicYear:    p.year,
		Semester:        PresentationSemester,
		StartsOn:        strPtr(isoDate(p.start)),
		EndsOn:          strPtr(isoDate(p.end)),
		MidtermStartsOn: strPtr(isoDate(p.midStart)),
		MidtermEndsOn:   strPtr(isoDate(p.midEnd)),
		FinalStartsOn:   strPtr(isoDate(finalStart)),
		FinalEndsOn:     strPtr(isoDate(finalStart.AddDate(0, 0, 4))),
		Months:          created,
		IsActive:        false, // never takes over the happy path's term
	}
	// Overlap allowed: this term and the walkthrough's are both anchored on
	// today by design (see service.AllowTermOverlap).
	t, err := p.svc.Teaching.UpsertTerm(service.AllowTermOverlap(ctx), p.adminID, in)
	if err != nil {
		if errors.Is(err, service.ErrConflict) {
			return fmt.Errorf("มีภาคเรียน %d/%d อยู่แล้ว (ไม่ใช่ชุดข้อมูลนำเสนอ) กรุณารีเซ็ตห้องทดลองก่อน", p.year, PresentationSemester)
		}
		return err
	}
	p.termID = t.ID
	// Wide enough that every request the dataset files, however far back it
	// is dated, counts as on time.
	if err := openRequestWindow(ctx, p.svc, p.adminID, t.ID, timeutil.Now().AddDate(-1, 0, 0), timeutil.Now().AddDate(0, 1, 0)); err != nil {
		return err
	}
	if created != months {
		if _, err := p.slot.Pool.Exec(ctx, `UPDATE academic_terms SET months = $2 WHERE id = $1`, t.ID, months); err != nil {
			return err
		}
	}
	return nil
}

// users: fictional lecturers and TAs in seedSlot's own shape, minus the
// password — nobody logs in as them.
func (p *presBuilder) users(ctx context.Context) error {
	type acct struct {
		email, first, last, role string
		level                    *string
	}
	var accts []acct
	ug, ms := "undergrad", "master"
	for i := 1; i <= presLecturers; i++ {
		accts = append(accts, acct{fmt.Sprintf("lecturer%02d@%s", i, presentationEmailDomain),
			"ผศ." + presName(i), fmt.Sprintf("อาจารย์ที่ %02d (สมมติ)", i), "lecturer", nil})
	}
	for i := 1; i <= presUndergrads; i++ {
		accts = append(accts, acct{fmt.Sprintf("ta%02d@%s", i, presentationEmailDomain),
			presName(i + 7), fmt.Sprintf("ผู้ช่วยสอนที่ %02d (สมมติ)", i), "ta", &ug})
	}
	for i := 1; i <= presGrads; i++ {
		accts = append(accts, acct{fmt.Sprintf("grad%02d@%s", i, presentationEmailDomain),
			presName(i + 13), fmt.Sprintf("บัณฑิตที่ %02d (สมมติ)", i), "ta", &ms})
	}
	ids := make([]uuid.UUID, len(accts))
	err := slotExec(ctx, p.slot, func(tx pgx.Tx) error {
		for i, a := range accts {
			ids[i] = uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO users (id, email, first_name, last_name, is_active, profile_completed, must_change_password, study_level)
				 VALUES ($1,$2,$3,$4,TRUE,TRUE,FALSE,$5::study_level)`,
				ids[i], a.email, a.first, a.last, a.level); err != nil {
				return fmt.Errorf("บัญชี %s: %w", a.email, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO user_roles (user_id, role) VALUES ($1,$2::role_code)`, ids[i], a.role); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.lecturers = ids[:presLecturers]
	p.tas = ids[presLecturers : presLecturers+presUndergrads]
	p.grads = ids[presLecturers+presUndergrads:]
	return nil
}

// taTimetables: every TA declares one class of their own (Saturday
// afternoon, clear of every course here) — except the one whose missing
// timetable holds a request at 'submitted'.
func (p *presBuilder) taTimetables(ctx context.Context) error {
	p.pendingTA = p.tas[len(p.tas)-1]
	for _, ta := range append(append([]uuid.UUID{}, p.tas...), p.grads...) {
		if ta == p.pendingTA {
			continue
		}
		block := service.ClassBlock{
			ID: "own", TermID: p.termID, CourseCode: "GE100099", CourseName: "วิชาศึกษาทั่วไป (สมมติ)",
			Kind: "lecture", SecNo: "1", DayOfWeek: 6, StartTime: "14:00", EndTime: "16:00",
		}
		if err := p.svc.Workload.ReplaceClasses(ctx, ta, p.termID, []service.ClassBlock{block}); err != nil {
			return err
		}
	}
	return nil
}

func (p *presBuilder) courses(ctx context.Context) error {
	p.courseIDs = map[string]uuid.UUID{}
	p.sections = map[string][]uuid.UUID{}
	for _, c := range presentationCourses {
		total := 0
		var secs []map[string]any
		for i, s := range c.sections {
			total += s.students
			cur := c.curriculum
			secs = append(secs, map[string]any{
				"sec_no": fmt.Sprintf("%d", i+1), "track": s.track, "num_students": s.students, "curriculum": cur,
				"schedules": []map[string]any{{"kind": "lecture", "day_of_week": s.day, "start_time": s.start, "end_time": s.end}},
			})
		}
		raw, err := json.Marshal(map[string]any{
			"term_id": p.termID, "code": c.code, "name_th": c.name, "level": "undergrad",
			"credits": 3, "lecture_hrs": c.lecHrs, "lab_hrs": 0, "self_hrs": 6,
			"num_students": total, "lecturer_ids": []uuid.UUID{p.lecturers[c.lecturer-1]},
			"sections": secs,
		})
		if err != nil {
			return err
		}
		var in service.CreateTeachingCourseInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return err
		}
		tcID, err := p.svc.Teaching.Create(ctx, p.adminID, in)
		if err != nil {
			return fmt.Errorf("%s: %w", c.code, err)
		}
		p.courseIDs[c.code] = tcID
		rows, err := p.slot.Pool.Query(ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 ORDER BY sec_no`, tcID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			p.sections[c.code] = append(p.sections[c.code], id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// allocate hands out undergrad TAs so no TA has two classes on one day and
// nobody but capTA takes more than two courses. Deterministic: same input,
// same people, every time.
func (p *presBuilder) allocate() {
	p.taOf = map[string][]presAssign{}
	days := map[uuid.UUID]map[int]bool{}
	load := map[uuid.UUID]int{}
	pool := p.tas[:len(p.tas)-1] // the last one is pendingTA
	p.capTA = pool[0]
	next := 1
	pick := func(day int, limit int, avoid map[uuid.UUID]bool) uuid.UUID {
		for tries := 0; tries < len(pool)*2; tries++ {
			ta := pool[next%len(pool)]
			next++
			if ta == p.capTA || avoid[ta] || load[ta] >= limit || days[ta][day] {
				continue
			}
			return ta
		}
		return uuid.Nil
	}
	mark := func(ta uuid.UUID, day int) {
		if days[ta] == nil {
			days[ta] = map[int]bool{}
		}
		days[ta][day] = true
		load[ta]++
	}
	// capTA: three courses on three different days, so the rejected request
	// below is refused for the cap alone.
	capCourses := map[string]bool{"DM120203": true, "DM150501": true, "DM110103": true}
	for _, c := range presentationCourses {
		if c.tas == 0 && c.grads == 0 {
			continue
		}
		var regular []int
		for i, s := range c.sections {
			if s.track == "regular" {
				regular = append(regular, i)
			}
		}
		used := map[uuid.UUID]bool{}
		for k := 0; k < c.tas; k++ {
			si := regular[k%len(regular)]
			s := c.sections[si]
			var ta uuid.UUID
			switch {
			case c.phase == "pending":
				ta = p.pendingTA
			case c.phase == "rejected":
				ta = p.capTA
			case capCourses[c.code] && k == 0:
				ta = p.capTA
			default:
				ta = pick(s.day, 2, used)
			}
			used[ta] = true
			if c.phase != "pending" && c.phase != "rejected" {
				mark(ta, s.day)
			}
			p.taOf[c.code] = append(p.taOf[c.code], presAssign{ta: ta, section: p.sections[c.code][si], sec: s})
		}
		for g := 0; g < c.grads; g++ {
			for si, s := range c.sections {
				if s.track == "special" {
					p.taOf[c.code] = append(p.taOf[c.code], presAssign{ta: p.grads[g], section: p.sections[c.code][si], sec: s, grad: true})
				}
			}
		}
	}
}

// requests files the TA requests of the given phases through the real
// service, as each course's lecturer. The auto-decider's verdict must match
// what the dataset intends, or the dataset is not what its comments claim.
func (p *presBuilder) requests(ctx context.Context, phases ...string) error {
	if p.taOf == nil {
		p.allocate()
	}
	want := map[string]bool{}
	for _, ph := range phases {
		want[ph] = true
	}
	for _, c := range presentationCourses {
		if !want[c.phase] || len(p.taOf[c.code]) == 0 {
			continue
		}
		byTA := map[uuid.UUID]*service.AssignmentInput{}
		var order []uuid.UUID
		for _, a := range p.taOf[c.code] {
			in, ok := byTA[a.ta]
			if !ok {
				level, wl := "undergrad", service.WorkloadInput{CheckWorkHrs: 2, AttendanceHrs: 3}
				if a.grad {
					level, wl = "master", service.WorkloadInput{HelpTeachHrs: 5, PrepHrs: 3, GradeHrs: 2}
				}
				in = &service.AssignmentInput{TAID: a.ta, Level: level, Workload: wl}
				byTA[a.ta] = in
				order = append(order, a.ta)
			}
			in.SectionIDs = append(in.SectionIDs, a.section)
		}
		req := service.CreateTARequestInput{TeachingCourseID: p.courseIDs[c.code], ReimburseScope: "lecture"}
		for _, ta := range order {
			req.Assignments = append(req.Assignments, *byTA[ta])
		}
		res, err := p.svc.TARequest.Create(ctx, p.lecturers[c.lecturer-1], req)
		if err != nil {
			return fmt.Errorf("%s: %w", c.code, err)
		}
		expect := map[string]string{"1": "approved", "2": "approved", "pending": "submitted", "rejected": "rejected"}[c.phase]
		if res.Status != expect {
			return fmt.Errorf("%s: ระบบตัดสินคำขอเป็น %s แต่ชุดข้อมูลตั้งใจให้เป็น %s (%s)", c.code, res.Status, expect, res.RejectReason)
		}
	}
	// Assignment ids, for the work logs.
	for code, list := range p.taOf {
		for i := range list {
			_ = p.slot.Pool.QueryRow(ctx, `
				SELECT a.id FROM ta_request_assignments a
				JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
				WHERE a.section_id = $1 AND a.ta_id = $2 AND a.state <> 'dropped'`,
				list[i].section, list[i].ta).Scan(&list[i].assignment)
		}
		p.taOf[code] = list
	}
	return nil
}

func (p *presBuilder) appointment(ctx context.Context) error {
	var signer uuid.UUID
	if err := p.slot.Pool.QueryRow(ctx, `SELECT id FROM admin_officers WHERE is_active LIMIT 1`).Scan(&signer); err != nil {
		return errors.New("ยังไม่มีผู้ลงนามในระบบ รีเซ็ตห้องทดลองแล้วลองใหม่")
	}
	issued := p.start.AddDate(0, 0, 3)
	_, _, err := p.svc.Appointment.Build(ctx, p.adminID, service.AppointmentOrderInput{
		TermID:          p.termID,
		OrderNo:         fmt.Sprintf("สาธิต-%d/%d", p.year, PresentationSemester),
		OrderDate:       thaiLongDate(issued),
		EffectiveDate:   thaiLongDate(p.start),
		SignerOfficerID: signer,
	})
	return err
}

type returnedProfile struct {
	ta     uuid.UUID
	status string
	reason string
}

// profiles: status rows only — the shape DocsService.ReviewProfile leaves
// behind, without any of the personal data or documents behind a real one.
// Most approved; two sent back for fixes, one rejected (the dashboard's
// "เอกสาร TA ถูกตีกลับ"); the late TAs have not submitted yet.
func (p *presBuilder) profiles(ctx context.Context) error {
	late := map[uuid.UUID]bool{p.pendingTA: true}
	for _, c := range presentationCourses {
		if c.phase == "2" {
			for _, a := range p.taOf[c.code] {
				late[a.ta] = true
			}
		}
	}
	all := append(append([]uuid.UUID{}, p.tas...), p.grads...)
	return slotExec(ctx, p.slot, func(tx pgx.Tx) error {
		returned := 0
		for i, ta := range all {
			if late[ta] {
				continue
			}
			status, reason := "approved", (*string)(nil)
			if returned < 3 && i%11 == 5 {
				returned++
				status = "needs_fix"
				r := "ภาพสำเนาบัญชีธนาคารไม่ชัด กรุณาอัปโหลดใหม่ (สมมติ)"
				if returned == 3 {
					status = "rejected"
					r = "ชื่อบัญชีไม่ตรงกับชื่อผู้ช่วยสอน (สมมติ)"
				}
				reason = &r
			}
			if status != "approved" {
				p.returned = append(p.returned, returnedProfile{ta: ta, status: status, reason: *reason})
				status, reason = "approved", nil
			}
			submitted := p.start.AddDate(0, 0, -10+i%5)
			reviewed := submitted.AddDate(0, 0, 2)
			prefix := "นาย"
			if i%2 == 1 {
				prefix = "นางสาว"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ta_profiles (user_id, prefix, status, completed_at, current_round, reject_reason, verified_at, verified_by)
				VALUES ($1,$2,$3::doc_status,$4,1,$5,$6,$7)`,
				ta, prefix, status, submitted, reason, reviewed, p.staffID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ta_profile_submissions (id, user_id, round, submitted_at, status, reviewed_at, reviewed_by, reject_reason, prefix)
				VALUES (gen_random_uuid(),$1,1,$2,$3::doc_status,$4,$5,$6,$7)`,
				ta, submitted, status, reviewed, p.staffID, reason, prefix); err != nil {
				return err
			}
			// Staff-review now certifies the TA's documents too (payoutIssue
			// wants an approved creditor form), so an approved profile carries
			// its approved form. Status row only — no file behind it, like
			// the profile itself.
			if status == "approved" {
				if _, err := tx.Exec(ctx, `
					INSERT INTO ta_documents (user_id, kind, filename, mime, size_bytes, storage_key, status, uploaded_at)
					VALUES ($1, 'creditor_form', 'creditor_form.pdf', 'application/pdf', 0,
					        $3, 'approved', $2)`,
					ta, submitted, "presentation/creditor_form/"+ta.String()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// holiday: one Wednesday closure in week 5. Three sections teach on
// Wednesdays; one lecturer files a makeup, the others have not — the
// dashboard's "คาบวันหยุดยังไม่ชดเชย". Wednesday keeps it clear of the happy
// path's Monday classes.
func (p *presBuilder) holiday(ctx context.Context) error {
	p.holidayDate = p.start.AddDate(0, 0, 4*7+2)
	if _, err := p.svc.Holiday.Create(ctx, p.adminID, service.HolidayInput{
		HolidayDate: isoDate(p.holidayDate), NameTH: "วันหยุดพิเศษของมหาวิทยาลัย (สมมติ)", Source: "custom",
	}); err != nil {
		return err
	}
	makeup := p.holidayDate.AddDate(0, 0, 3) // the Saturday after
	return p.svc.Teaching.AddMakeup(ctx, p.adminID, p.sections["DM140401"][0], service.MakeupSchedule{
		OriginalDate: isoDate(p.holidayDate), MakeupDate: isoDate(makeup), Kind: "lecture",
		StartTime: strPtr("09:00"), EndTime: strPtr("12:00"),
	})
}

// periods: one claim month per calendar month of the term so far, due on
// the 7th of the next month, the reminder three days ahead.
func (p *presBuilder) periods(ctx context.Context) error {
	p.periodByYM = map[string]*service.SubmissionPeriod{}
	today := dateOnly(p.now)
	for m := time.Date(p.start.Year(), p.start.Month(), 1, 0, 0, 0, 0, timeutil.Bangkok); !m.After(today); m = m.AddDate(0, 1, 0) {
		ym := m.Format("2006-01")
		pm := presMonth{ym: ym, first: m, current: m.Year() == today.Year() && m.Month() == today.Month()}
		p.months = append(p.months, pm)
		starts := m
		if starts.Before(p.start) {
			starts = p.start
		}
		sp, err := p.svc.SubmissionPeriods.Upsert(ctx, p.adminID, service.SubmissionPeriod{
			TermID:           p.termID,
			YearMonth:        fmt.Sprintf("%d-%02d", p.year, int(m.Month())),
			StartsOn:         isoDate(starts),
			DueDate:          isoDate(m.AddDate(0, 1, 6)),
			Label:            fmt.Sprintf("รอบส่งเดือน%s (สมมติ)", thaiMonthName(m.Month())),
			RemindDaysBefore: 3,
		})
		if err != nil {
			return err
		}
		p.periodByYM[ym] = sp
	}
	return nil
}

// worklogs: one row per class taught, from the start of term to yesterday
// (midterm week, the holiday and the grad-special section excluded), in the
// shape the work-log screen's auto-fill writes and the TA then submits.
// Rows of the last six days are still drafts on the TA's screen.
func (p *presBuilder) worklogs(ctx context.Context) error {
	today := dateOnly(p.now)
	draftFrom := today.AddDate(0, 0, -6)
	return slotExec(ctx, p.slot, func(tx pgx.Tx) error {
		for _, c := range presentationCourses {
			if c.phase != "1" && c.phase != "2" {
				continue
			}
			for _, a := range p.taOf[c.code] {
				if a.grad || a.assignment == uuid.Nil || a.sec.students == 0 {
					continue
				}
				st, _ := time.Parse("15:04", a.sec.start)
				en, _ := time.Parse("15:04", a.sec.end)
				hours := en.Sub(st).Hours()
				for d := p.start; d.Before(today); d = d.AddDate(0, 0, 1) {
					if int(d.Weekday()) != a.sec.day%7 {
						continue
					}
					if (!d.Before(p.midStart) && !d.After(p.midEnd)) || d.Equal(p.holidayDate) {
						continue
					}
					status := "submitted"
					var submittedAt any = d.Add(18*time.Hour).AddDate(0, 0, 1)
					if !d.Before(draftFrom) {
						status, submittedAt = "draft", nil
					}
					if _, err := tx.Exec(ctx, `
						INSERT INTO work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, status, submitted_at, source)
						VALUES (gen_random_uuid(),$1,$2::date,$3::time,$4::time,$5,'lecture',$6::worklog_status,$7,'auto')`,
						a.assignment, isoDate(d), a.sec.start, a.sec.end, hours, status, submittedAt); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// workflow moves each month along through the real services, so every month
// sits at a different step:
//
//	older months       approved, reviewed, exported
//	two months back    exported, except two courses left ready to export and
//	                   one TA's month sent back by staff
//	last month         approved; half the courses reviewed; one TA still with
//	                   the lecturer; one TA's month rejected back to the TA
//	this month         the first half approved, the rest still with the
//	                   lecturer or on the TA's screen
func (p *presBuilder) workflow(ctx context.Context) error {
	var past []presMonth
	var cur *presMonth
	for i := range p.months {
		if p.months[i].current {
			cur = &p.months[i]
		} else {
			past = append(past, p.months[i])
		}
	}
	k := len(past)
	readyExport := map[string]bool{"DM120202": true, "DM140402": true}
	lastReviewed := map[string]bool{}
	for i, c := range presentationCourses {
		if i%2 == 0 {
			lastReviewed[c.code] = true
		}
	}
	type pair struct {
		code string
		a    presAssign
	}
	var pairs []pair
	for _, c := range presentationCourses {
		if c.phase != "1" && c.phase != "2" {
			continue
		}
		for _, a := range p.taOf[c.code] {
			if !a.grad && a.assignment != uuid.Nil && a.sec.students > 0 {
				pairs = append(pairs, pair{c.code, a})
			}
		}
	}
	// The cases that hold a month back are picked by course, not by position,
	// so they never land on the money cases above and move their numbers.
	nth := func(code string, n int) presAssign {
		for _, pr := range pairs {
			if pr.code == code {
				if n == 0 {
					return pr.a
				}
				n--
			}
		}
		return presAssign{}
	}
	withLecturer := nth("DM110103", 0) // last month still awaiting the lecturer
	rejectedLast := nth("DM190102", 0) // last month rejected back to the TA
	rejectedNow := nth("DM190102", 1)  // this month rejected back to the TA
	held := func(a presAssign) bool {
		return a.assignment == withLecturer.assignment || a.assignment == rejectedLast.assignment
	}
	phaseOf := map[string]string{}
	lecturerOf := map[string]uuid.UUID{}
	for _, c := range presentationCourses {
		phaseOf[c.code] = c.phase
		lecturerOf[c.code] = p.lecturers[c.lecturer-1]
	}

	// Approve/Reject act on the month's submitted rows; a month in which a
	// pair has none (the first days of term, or all drafts) is skipped.
	countRows := func(a presAssign, ym, status string) int {
		var n int
		_ = p.slot.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM work_logs
			WHERE assignment_id=$1 AND status=$3::worklog_status AND to_char(work_date,'YYYY-MM')=$2`, a.assignment, ym, status).Scan(&n)
		return n
	}
	hasSubmitted := func(a presAssign, ym string) bool { return countRows(a, ym, "submitted") > 0 }

	// 1. Lecturer decisions, month by month.
	for i, m := range past {
		for _, pr := range pairs {
			lect := lecturerOf[pr.code]
			if !hasSubmitted(pr.a, m.ym) {
				continue
			}
			switch {
			case i == k-1 && pr.a.assignment == withLecturer.assignment:
				continue // still with the lecturer
			case i == k-1 && pr.a.assignment == rejectedLast.assignment:
				if err := p.svc.WorkLog.Reject(ctx, lect, pr.a.assignment,
					"เวลาเริ่มไม่ตรงกับตารางสอน กรุณาแก้ไข (สมมติ)", m.ym, false); err != nil {
					return fmt.Errorf("ตีกลับ %s %s: %w", pr.code, m.ym, err)
				}
				continue
			}
			if err := p.svc.WorkLog.Approve(ctx, lect, pr.a.assignment, m.ym, false); err != nil {
				return fmt.Errorf("อนุมัติ %s %s: %w", pr.code, m.ym, err)
			}
		}
	}
	if cur != nil {
		for j, pr := range pairs {
			if j%2 == 0 && pr.a.assignment != rejectedNow.assignment && hasSubmitted(pr.a, cur.ym) {
				if err := p.svc.WorkLog.Approve(ctx, lecturerOf[pr.code], pr.a.assignment, cur.ym, false); err != nil {
					return fmt.Errorf("อนุมัติ %s %s: %w", pr.code, cur.ym, err)
				}
			}
		}
	}
	if cur != nil && rejectedNow.assignment != uuid.Nil && hasSubmitted(rejectedNow, cur.ym) {
		if err := p.svc.WorkLog.Reject(ctx, lecturerOf["DM190102"], rejectedNow.assignment,
			"ขาดรายละเอียดกิจกรรม (สมมติ)", cur.ym, false); err != nil {
			return fmt.Errorf("ตีกลับ DM190102 %s: %w", cur.ym, err)
		}
	}

	// 2. Staff review (appointed pairs only — the others wait for an order).
	for i, m := range past {
		sp := p.periodByYM[m.ym]
		for _, pr := range pairs {
			if phaseOf[pr.code] != "1" {
				continue
			}
			if i == k-1 && (!lastReviewed[pr.code] || held(pr.a)) {
				continue
			}
			if countRows(pr.a, m.ym, "approved") == 0 {
				continue // no class that month (term started late in it)
			}
			// In the first days of a month the six-day draft window reaches
			// back into last month, and staff cannot sign off a month the TA
			// has not finished sending.
			if countRows(pr.a, m.ym, "draft") > 0 {
				continue
			}
			// A TA who has not sent documents yet cannot be signed off
			// (staff-review certifies the documents too); their months wait.
			if !p.hasApprovedProfile(ctx, pr.a.ta) {
				continue
			}
			if err := p.svc.SubmissionPeriods.MarkStaffReviewed(ctx, p.staffID, sp.ID, pr.a.ta, p.courseIDs[pr.code], ""); err != nil {
				return fmt.Errorf("ตรวจเบิกจ่าย %s %s: %w", pr.code, m.ym, err)
			}
		}
	}

	// 3. Export every month but the last, per course.
	if k >= 2 {
		for _, c := range presentationCourses {
			if c.phase != "1" {
				continue
			}
			var months []string
			for i, m := range past[:k-1] {
				if i == k-2 && readyExport[c.code] {
					continue
				}
				months = append(months, m.ym)
			}
			if len(months) == 0 || len(p.taOf[c.code]) == 0 {
				continue
			}
			if _, err := p.svc.SubmissionPeriods.MarkCourseExported(ctx, p.staffID, p.courseIDs[c.code], months); err != nil {
				return fmt.Errorf("ส่งออก %s: %w", c.code, err)
			}
		}
		// 4. One exported month sent back for correction.
		for _, pr := range pairs {
			if pr.code == "DM110102" {
				sp := p.periodByYM[past[k-2].ym]
				// Send-back refuses a closed month (the TA could not resend and
				// the rows would be forfeited), so do what staff are told to:
				// extend the due date first.
				if _, err := p.svc.Pool.Exec(ctx,
					`UPDATE submission_periods SET due_date = $2::date, is_closed = FALSE WHERE id = $1`,
					sp.ID, isoDate(p.now.AddDate(0, 0, 14))); err != nil {
					return fmt.Errorf("ขยายกำหนดส่ง %s: %w", pr.code, err)
				}
				if err := p.svc.SubmissionPeriods.MarkSentBack(ctx, p.adminID, sp.ID, pr.a.ta, p.courseIDs[pr.code],
					"pending", "จำนวนชั่วโมงไม่ตรงกับเอกสารลงนาม ขอให้ตรวจสอบใหม่ (สมมติ)"); err != nil {
					return fmt.Errorf("ส่งกลับ %s: %w", pr.code, err)
				}
				break
			}
		}
	}
	return nil
}

func (p *presBuilder) hasApprovedProfile(ctx context.Context, ta uuid.UUID) bool {
	var ok bool
	_ = p.svc.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ta_profiles WHERE user_id = $1 AND status = 'approved')`, ta).Scan(&ok)
	return ok
}

// returnProfiles sends back the documents chosen in profiles(), after the
// months that were already paid have been reviewed and exported.
func (p *presBuilder) returnProfiles(ctx context.Context) error {
	return slotExec(ctx, p.slot, func(tx pgx.Tx) error {
		for _, r := range p.returned {
			if _, err := tx.Exec(ctx,
				`UPDATE ta_profiles SET status = $2::doc_status, reject_reason = $3 WHERE user_id = $1`,
				r.ta, r.status, r.reason); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE ta_profile_submissions SET status = $2::doc_status, reject_reason = $3 WHERE user_id = $1`,
				r.ta, r.status, r.reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// closePeriods closes every month whose due date has passed — what the
// scheduler's AutoCloseExpired does overnight.
func (p *presBuilder) closePeriods(ctx context.Context) error {
	today := dateOnly(p.now)
	keys := make([]string, 0, len(p.periodByYM))
	for k := range p.periodByYM {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sp := *p.periodByYM[k]
		due, err := time.ParseInLocation("2006-01-02", sp.DueDate[:10], timeutil.Bangkok)
		if err != nil {
			return err
		}
		if !due.Before(today) {
			continue
		}
		sp.IsClosed = true
		if _, err := p.svc.SubmissionPeriods.Upsert(ctx, p.adminID, sp); err != nil {
			return err
		}
	}
	return nil
}

func thaiMonthName(m time.Month) string {
	return []string{"", "มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน", "พฤษภาคม", "มิถุนายน",
		"กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม", "พฤศจิกายน", "ธันวาคม"}[m]
}
