package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// One timetable import filed 336 "makeup auto-filled" rows on 29/09/2026 —
// seven pages of the same line, with the eighteen things people actually did
// that day somewhere underneath. Folded, a burst is one line that says how
// many rows it stands for; unfolded, every row is still there.
func TestListAudit_FoldsOneRequestsBurstIntoOneRow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)

	req := uuid.New()
	ctx := audit.WithRequest(f.ctx, audit.RequestInfo{RequestID: req, IP: "203.0.113.7", ActorRole: "staff"})
	a := audit.New(f.Pool)
	for i := 0; i < 12; i++ {
		if err := a.Log(ctx, audit.Entry{Action: "makeup.auto_fill_from_tdbm", Entity: "section",
			EntityID: f.SectionID.String()}); err != nil {
			t.Fatal(err)
		}
	}
	// The same request also did something else: a different action is a
	// different line, even on the same request.
	if err := a.Log(ctx, audit.Entry{Action: "schedule.import", Entity: "term", EntityID: f.TermID.String()}); err != nil {
		t.Fatal(err)
	}
	// Rows with no request behind them (a scheduled job) never fold together.
	seedAudit(t, f,
		audit.Entry{Action: "ta_doc.expire", Entity: "ta_document", EntityID: "a"},
		audit.Entry{Action: "ta_doc.expire", Entity: "ta_document", EntityID: "b"})

	folded, total, err := svc.ListAudit(f.ctx, AuditQuery{Fold: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(folded) != 4 {
		t.Fatalf("folded: total=%d rows=%d, want 4 lines (burst, import, two job rows)", total, len(folded))
	}
	var burst *AuditRow
	for i := range folded {
		if folded[i].Action == "makeup.auto_fill_from_tdbm" {
			burst = &folded[i]
		} else if folded[i].Count != 1 {
			t.Errorf("%s stands for %d rows, want 1", folded[i].Action, folded[i].Count)
		}
	}
	if burst == nil || burst.Count != 12 {
		t.Fatalf("the burst line = %+v, want one line standing for 12 rows", burst)
	}

	_, rawTotal, err := svc.ListAudit(f.ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rawTotal != 15 {
		t.Errorf("unfolded total = %d, want all 15 rows", rawTotal)
	}

	// Opening the burst: its request and action, unfolded, is exactly its rows.
	inside, n, err := svc.ListAudit(f.ctx, AuditQuery{RequestID: &req, Action: "makeup.auto_fill_from_tdbm"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 12 || len(inside) != 12 {
		t.Errorf("opening the burst returned %d/%d rows, want 12", len(inside), n)
	}
}

// "Only the failures", "only money", "what needs a second look" are questions
// in the catalog's terms. They are answered as a list of actions, which is what
// lets them reach rows written before the catalog existed.
func TestListAudit_FiltersByTheCatalogsTerms(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "auth.login", Entity: "user", EntityID: "a"},
		audit.Entry{Action: "auth.login_failed", Entity: "user", EntityID: "a"},
		audit.Entry{Action: "worklog.approve", Entity: "assignment", EntityID: "b"},
		audit.Entry{Action: "submission_period.finance_revert", Entity: "submission_period_status", EntityID: "c"},
		audit.Entry{Action: "ta_profile.citizen_id.reveal", Entity: "ta_profile", EntityID: "d"},
	)
	cases := []struct {
		name string
		q    AuditQuery
		want []string
	}{
		{"outcome failed", AuditQuery{Outcome: "failed"}, []string{"auth.login_failed"}},
		{"category access", AuditQuery{Category: "access"}, []string{"auth.login_failed", "auth.login"}},
		{"severity warn+danger", AuditQuery{Severity: "warn,danger"},
			[]string{"submission_period.finance_revert", "auth.login_failed"}},
		{"sensitive reads", AuditQuery{Severity: "notice"}, []string{"ta_profile.citizen_id.reveal"}},
		{"either kind of refusal", AuditQuery{Outcome: "failed,denied"}, []string{"auth.login_failed"}},
		{"category and outcome together", AuditQuery{Category: "access", Outcome: "ok"}, []string{"auth.login"}},
		// A mistyped category must match nothing, never everything.
		{"unknown category", AuditQuery{Category: "nonsense"}, nil},
	}
	for _, tc := range cases {
		rows, total, err := svc.ListAudit(f.ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[r.Action] = true
		}
		if total != len(tc.want) || len(got) != len(tc.want) {
			t.Errorf("%s: got %v (total %d), want %v", tc.name, got, total, tc.want)
			continue
		}
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("%s: missing %s in %v", tc.name, w, got)
			}
		}
	}
}

// A reference quoted from a memo names one row whenever it was written. The
// default window is seven days; the reader should not also need the month.
func TestListAudit_AReferenceFindsItsRowOutsideTheWindow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f, audit.Entry{Action: "worklog.approve", Entity: "assignment", EntityID: "old"})
	var id int64
	if err := f.Pool.QueryRow(f.ctx, `SELECT MAX(id) FROM audit_logs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ref := AuditRef(id)

	// Ask about a window the row is not in.
	past := time.Now().Add(-48 * time.Hour)
	for _, typed := range []string{ref, strings.ToLower(ref), " " + strings.Replace(ref, "-", "", 1) + " "} {
		rows, total, err := svc.ListAudit(f.ctx, AuditQuery{Q: typed, From: past.Add(-time.Hour), To: past})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(rows) != 1 || rows[0].ID != id || rows[0].Ref != ref {
			t.Errorf("typing %q returned %d rows, want exactly row %s", typed, len(rows), ref)
		}
	}
	if _, isRef := ParseAuditRef("AL-"); isRef {
		t.Error(`"AL-" alone parsed as a reference`)
	}
	if _, isRef := ParseAuditRef("ALICE-12"); isRef {
		t.Error(`"ALICE-12" parsed as a reference`)
	}
}

// Nobody types "worklog.approve". They type the words on the screen, a course
// code, or a person's name — none of which is stored in audit_logs, which holds
// ids.
func TestListAudit_SearchesTheWayPeopleType(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	f.exec(`UPDATE users SET first_name='พิมพ์ชนก', last_name='ศรีสุข' WHERE id=$1`, f.TAID)
	f.exec(`UPDATE teaching_courses SET code='ZZ987654' WHERE id=$1`, f.CourseID)
	other := f.insertUser("staff", "bystander")

	seedAudit(t, f,
		// About the TA's assignment — the row carries only the assignment id.
		audit.Entry{ActorID: &f.LecturerID, Action: "worklog.approve", Entity: "assignment", EntityID: f.AssignmentID.String()},
		// About a section of the course — only the section id.
		audit.Entry{Action: "makeup.add", Entity: "section", EntityID: f.SectionID.String()},
		// Done BY the TA.
		audit.Entry{ActorID: &f.TAID, Action: "worklog.submit", Entity: "assignment", EntityID: "elsewhere"},
		// Unrelated.
		audit.Entry{ActorID: &other, Action: "holiday.create", Entity: "holiday", EntityID: "h"},
	)

	count := func(q string) (map[string]bool, int) {
		rows, total, err := svc.ListAudit(f.ctx, AuditQuery{Q: q})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[r.Action] = true
		}
		return got, total
	}

	if got, total := count("อนุมัติชั่วโมง"); total != 1 || !got["worklog.approve"] {
		t.Errorf("searching the label on screen: %v (total %d), want worklog.approve", got, total)
	}
	if got, total := count("ZZ987654"); total != 2 || !got["worklog.approve"] || !got["makeup.add"] {
		t.Errorf("searching the course code: %v (total %d), want the assignment and section rows", got, total)
	}
	if got, total := count("พิมพ์ชนก"); total != 2 || !got["worklog.approve"] || !got["worklog.submit"] {
		t.Errorf("searching the TA's name: %v (total %d), want rows about them and by them", got, total)
	}
	// A wildcard typed by the user is text, not a pattern.
	if _, total := count("%"); total != 0 {
		t.Errorf(`searching "%%" matched %d rows, want none`, total)
	}
}

func TestListAudit_NamesTheSubjectForEveryKindOfThing(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	f.exec(`UPDATE users SET first_name='พิมพ์ชนก', last_name='ศรีสุข' WHERE id=$1`, f.TAID)
	f.exec(`UPDATE teaching_courses SET code='CP363205', name_th='วิชาทดสอบ' WHERE id=$1`, f.CourseID)
	var secNo string
	if err := f.Pool.QueryRow(f.ctx, `SELECT sec_no FROM sections WHERE id=$1`, f.SectionID).Scan(&secNo); err != nil {
		t.Fatal(err)
	}
	gone := uuid.New().String()

	seedAudit(t, f,
		audit.Entry{Action: "probe.section", Entity: "section", EntityID: f.SectionID.String()},
		audit.Entry{Action: "probe.assignment", Entity: "assignment", EntityID: f.AssignmentID.String()},
		audit.Entry{Action: "probe.course", Entity: "teaching_course", EntityID: f.CourseID.String()},
		audit.Entry{Action: "probe.profile", Entity: "ta_profile", EntityID: f.TAID.String()},
		audit.Entry{Action: "probe.term", Entity: "term", EntityID: f.TermID.String()},
		// The read-audit middleware leaves entity_id empty and writes the id in
		// the note for term-wide screens.
		audit.Entry{Action: "probe.virtual", Entity: "academic_term", Note: "term_id=" + f.TermID.String()},
		// The staff sign-off names its TA and course in the note.
		audit.Entry{Action: "probe.signoff", Entity: "submission_period_status", EntityID: gone,
			Note: "ta=" + f.TAID.String() + " course=" + f.CourseID.String()},
		// Something deleted: only the row's own before-image still names it.
		audit.Entry{Action: "probe.deleted", Entity: "teaching_course", EntityID: gone,
			Before: map[string]any{"code": "CP000001", "name_th": "วิชาที่ถูกลบ"}},
		// Something deleted with nothing to go on, and an id that is no id.
		audit.Entry{Action: "probe.unknown", Entity: "teaching_course", EntityID: gone},
		audit.Entry{Action: "probe.oddid", Entity: "section", EntityID: "not-a-uuid'; --"},
	)

	rows, _, err := svc.ListAudit(f.ctx, AuditQuery{Action: "probe."})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Action] = r.SubjectName
	}
	var termLabel string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT 'ภาค ' || semester || '/' || academic_year FROM academic_terms WHERE id=$1`, f.TermID).Scan(&termLabel); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"probe.section":    "CP363205 กลุ่ม " + secNo,
		"probe.assignment": "พิมพ์ชนก ศรีสุข · CP363205 กลุ่ม " + secNo,
		"probe.course":     "CP363205 วิชาทดสอบ",
		"probe.profile":    "พิมพ์ชนก ศรีสุข",
		"probe.term":       termLabel,
		"probe.virtual":    termLabel,
		"probe.signoff":    "พิมพ์ชนก ศรีสุข · CP363205 วิชาทดสอบ",
		"probe.deleted":    "CP000001 วิชาที่ถูกลบ",
		"probe.unknown":    "",
		"probe.oddid":      "",
	}
	for action, w := range want {
		if g, found := got[action]; !found {
			t.Errorf("%s: row missing", action)
		} else if g != w {
			t.Errorf("%s: subject = %q, want %q", action, g, w)
		}
	}
}

// The before/after images used to be shown as raw JSON. A finance officer
// reading "hours 3 → 2" should not need to know what JSON is.
func TestDecorate_ReadsARowBackInWords(t *testing.T) {
	actor := uuid.New()
	req := uuid.New()
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	ip := "10.88.4.21"
	note := "อาจารย์แจ้งแก้ทางบันทึกข้อความ"
	r := AuditRow{
		ID: 1365, Action: "worklog.staff_edit", ActorID: &actor, RequestID: &req,
		UserAgent: &ua, IP: &ip, Note: &note,
		Before: json.RawMessage(`{"hours":3,"activity":"lecture","work_date":"2026-09-12","id":"x","approved_by":"y","password_hash":"[redacted]"}`),
		After:  json.RawMessage(`{"hours":2,"activity":"lab","work_date":"2026-09-12","id":"x","approved_by":"z","password_hash":"[redacted]"}`),
	}
	decorate(&r)

	if r.Ref != "AL-001365" {
		t.Errorf("ref = %q", r.Ref)
	}
	if r.Label != "เจ้าหน้าที่แก้ชั่วโมง" || r.Category != "hours" || r.Severity != "warn" || r.Outcome != "ok" {
		t.Errorf("reading = %q/%q/%q/%q", r.Label, r.Category, r.Severity, r.Outcome)
	}
	if r.ActorKind != "user" || r.Device != "Chrome บน Windows" || r.Network != "private" {
		t.Errorf("who/where = %q/%q/%q", r.ActorKind, r.Device, r.Network)
	}
	if len(r.Details) != 1 || r.Details[0] != note {
		t.Errorf("details = %v, want the reason as typed", r.Details)
	}
	// Only what moved, in words; ids, signers and unchanged fields stay out.
	if len(r.Changes) != 2 {
		t.Fatalf("changes = %+v, want hours and activity only", r.Changes)
	}
	if c := r.Changes[0]; c.Key != "hours" || c.Label != "ชั่วโมง" || *c.Before != "3" || *c.After != "2" {
		t.Errorf("first change = %+v", c)
	}
	if c := r.Changes[1]; c.Key != "activity" || *c.Before != "บรรยาย" || *c.After != "ปฏิบัติการ" {
		t.Errorf("second change = %+v", c)
	}
}

func TestDecorate_SaysWhoWasBehindARowWithNoActor(t *testing.T) {
	req := uuid.New()
	cases := []struct {
		row  AuditRow
		want string
	}{
		{AuditRow{Action: "ta_doc.expire"}, "system"},
		// Raised by the system even though a person's request triggered it.
		{AuditRow{Action: "makeup.auto_fill_from_tdbm", RequestID: &req}, "system"},
		{AuditRow{Action: "auth.login_unknown_account", RequestID: &req}, "anonymous"},
		// Nobody recorded one, and it is not a job: say so rather than "ระบบ".
		{AuditRow{Action: "signature_checklist.toggle"}, "unknown"},
	}
	for _, tc := range cases {
		r := tc.row
		decorate(&r)
		if r.ActorKind != tc.want {
			t.Errorf("%s: actor kind = %q, want %q", r.Action, r.ActorKind, tc.want)
		}
	}
}

func TestDecorate_Changes(t *testing.T) {
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	// A create has no before; the fields read as plain values.
	r := AuditRow{Action: "worklog.approve", After: json.RawMessage(`{"count":42,"hours":68,"status":"approved"}`)}
	decorate(&r)
	if len(r.Changes) != 3 || r.Changes[0].Key != "status" || str(r.Changes[0].Before) != "<nil>" ||
		str(r.Changes[0].After) != "อนุมัติแล้ว" {
		t.Errorf("create changes = %+v", r.Changes)
	}

	// Cleared to nothing is a change to "", not a missing side.
	r = AuditRow{Action: "term.set_budget",
		Before: json.RawMessage(`{"budget_baht":40000,"budget_note":""}`),
		After:  json.RawMessage(`{"budget_baht":null,"budget_note":""}`)}
	decorate(&r)
	if len(r.Changes) != 1 || str(r.Changes[0].Before) != "40,000" || str(r.Changes[0].After) != "" {
		t.Errorf("cleared budget = %+v", r.Changes)
	}

	// A list of records says how many; a secret says it is a secret; a list of
	// plain values is spelled out.
	r = AuditRow{Action: "user.update",
		Before: json.RawMessage(`{"password_hash":"[redacted]","roles":["ta"],"sections":[{"a":1},{"a":2}]}`),
		After:  json.RawMessage(`{"password_hash":"[redacted] ","roles":["ta","staff"],"sections":[{"a":1}]}`)}
	decorate(&r)
	got := map[string]string{}
	for _, c := range r.Changes {
		got[c.Key] = str(c.Before) + " → " + str(c.After)
	}
	if got["roles"] != "ผู้ช่วยสอน → ผู้ช่วยสอน, เจ้าหน้าที่" {
		t.Errorf("roles = %q", got["roles"])
	}
	if got["sections"] != "2 รายการ → 1 รายการ" {
		t.Errorf("sections = %q", got["sections"])
	}
	if _, shown := got["password_hash"]; shown {
		t.Errorf("an unchanged secret was listed as a change: %q", got["password_hash"])
	}

	// Periods store their month with a Buddhist year already ("2569-08");
	// adding 543 again printed 08/3112. And a year is not a quantity.
	r = AuditRow{Action: "submission_period.upsert",
		After: json.RawMessage(`{"year_month":"2569-08","due_date":"2026-09-30","academic_year":2569,"total_baht":21330.34,"ta_count":12000}`)}
	decorate(&r)
	got = map[string]string{}
	for _, c := range r.Changes {
		got[c.Key] = str(c.After)
	}
	for k, w := range map[string]string{
		"year_month": "08/2569", "due_date": "30/09/2569", "academic_year": "2569",
		"total_baht": "21330.34", "ta_count": "12,000",
	} {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}

	// Malformed or non-object images must not break the row.
	r = AuditRow{Action: "x.y", Before: json.RawMessage(`[1,2]`), After: json.RawMessage(`not json`)}
	decorate(&r)
	if r.Changes != nil {
		t.Errorf("non-object images produced changes: %+v", r.Changes)
	}
}

func TestAuditDetails_ReadsNotes(t *testing.T) {
	cases := []struct {
		action, note string
		want         []string
	}{
		{"course.settlement_mode", "chronological → spread", []string{"จ่ายตามลำดับเวลา → เฉลี่ยทุกเดือน"}},
		{"auth.login", "sso", []string{"ผ่าน KKU SSO"}},
		{"holiday.bulk_create", "inserted 3/5", []string{"เพิ่ม 3 จาก 5 รายการ"}},
		{"makeup.delete", "2026-07-25", []string{"25/07/2569"}},
		{"ta_request.cancel", "สถานะเดิม: approved", []string{"สถานะเดิม: อนุมัติแล้ว"}},
		{"auth.login_unknown_account", "domain=kku.ac.th sha256=174ffe7030841233", []string{"โดเมนอีเมล kku.ac.th"}},
		{"makeup.auto_fill_from_tdbm", "original=2026-10-13 kind=lab makeup=2026-10-11 13:00-15:00 (extra_class_id=3948)",
			[]string{"วันเดิม 13/10/2569", "ประเภทคาบ ปฏิบัติการ", "วันชดเชย 11/10/2569 13:00-15:00"}},
		{"export.course.preview", "months=2026-06%2C2026-07", []string{"เดือน 06/2569, 07/2569"}},
		// Ids and paging parameters are noise to the reader.
		{"dashboard.executive.view", "term_id=ba4f1b82-923a-4b02-8330-add4b8836a1e", nil},
		{"users.list.view", "limit=15&offset=0&sort=name&dir=asc", nil},
		{"audit_log.search", "actor=b8eda87f-3d8d-4619-8aac-f03a0a8d5b98 q=สมชาย", []string{"คำค้น สมชาย"}},
		// A reason a person typed can contain "=" and must not be swallowed.
		{"worklog.staff_edit", "แก้เพราะ ชั่วโมง=3 ผิด", []string{"แก้เพราะ ชั่วโมง=3 ผิด"}},
		{"worklog.staff_edit", "แก้ตามบันทึก x=3", []string{"แก้ตามบันทึก x=3"}},
		{"x.y", "   ", nil},
	}
	for _, tc := range cases {
		note := tc.note
		r := AuditRow{Action: tc.action, Note: &note}
		got := auditDetails(&r)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s %q:\n got  %q\n want %q", tc.action, tc.note, got, tc.want)
		}
	}
}

// The overview answers "is anything wrong?" before a row is read. One wrong
// password is a person typing; the list would be nothing else if it counted.
func TestSummarizeAudit_PicksOutWhatNeedsASecondLook(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	clumsy := f.insertUser("ta", "clumsy")
	attacked := f.insertUser("ta", "attacked")
	f.exec(`UPDATE users SET first_name='ถูก', last_name='เดารหัส' WHERE id=$1`, attacked)

	// Twice wrong, then in: routine.
	for i := 0; i < 2; i++ {
		seedAudit(t, f, audit.Entry{ActorID: &clumsy, Action: "auth.login_failed", Entity: "user", IP: "203.0.113.1"})
	}
	// Five failures from five addresses: not routine.
	for _, ip := range []string{"203.0.113.2", "203.0.113.3", "203.0.113.4", "203.0.113.5", "203.0.113.6"} {
		seedAudit(t, f, audit.Entry{ActorID: &attacked, Action: "auth.login_failed", Entity: "user", IP: ip})
	}
	// A lock-out counts even once.
	seedAudit(t, f, audit.Entry{ActorID: &clumsy, Action: "auth.login_locked", Entity: "user"})
	// A reversal, and ordinary work that must not appear.
	seedAudit(t, f,
		audit.Entry{ActorID: &f.StaffID, Action: "course.unexport", Entity: "teaching_course", EntityID: f.CourseID.String()},
		audit.Entry{ActorID: &f.StaffID, Action: "worklog.approve", Entity: "assignment", EntityID: f.AssignmentID.String()},
		audit.Entry{ActorID: &f.StaffID, Action: "ta_profile.citizen_id.reveal", Entity: "ta_profile", EntityID: f.TAID.String()},
	)

	sum, err := svc.SummarizeAudit(f.ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		action string
		actor  uuid.UUID
	}
	got := map[key]AuditAttention{}
	for _, a := range sum.Attention {
		if a.ActorID != nil {
			got[key{a.Action, *a.ActorID}] = a
		}
	}
	if len(sum.Attention) != 3 {
		t.Errorf("attention has %d items, want 3 (the attack, the lock-out, the reversal): %+v", len(sum.Attention), sum.Attention)
	}
	if _, listed := got[key{"auth.login_failed", clumsy}]; listed {
		t.Error("two wrong passwords were listed as something to check")
	}
	atk, listed := got[key{"auth.login_failed", attacked}]
	if !listed || atk.Count != 5 || atk.DistinctIPs != 5 || atk.ActorName != "ถูก เดารหัส" || atk.Outcome != "failed" {
		t.Errorf("the attack = %+v (listed=%v), want 5 failures from 5 addresses", atk, listed)
	}
	if _, listed := got[key{"auth.login_locked", clumsy}]; !listed {
		t.Error("a lock-out was not listed")
	}
	rev, listed := got[key{"course.unexport", f.StaffID}]
	if !listed || rev.SubjectName == "" || rev.Severity != "warn" || rev.Ref == "" {
		t.Errorf("the reversal = %+v (listed=%v), want it named and referenced", rev, listed)
	}

	cats := map[string]int{}
	for _, c := range sum.Categories {
		cats[c.ID] = c.Events
	}
	if cats["access"] != 8 || cats["hours"] != 1 || cats["payment"] != 1 || cats["view"] != 1 {
		t.Errorf("category counts = %v", cats)
	}
	if sum.Rows != 11 || sum.Events != 11 {
		t.Errorf("rows/events = %d/%d, want 11/11", sum.Rows, sum.Events)
	}
}

// Events count bursts, so the numbers on the category chips agree with the
// folded timeline underneath them.
func TestSummarizeAudit_CountsABurstAsOneEvent(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	ctx := audit.WithRequest(f.ctx, audit.RequestInfo{RequestID: uuid.New()})
	a := audit.New(f.Pool)
	for i := 0; i < 9; i++ {
		if err := a.Log(ctx, audit.Entry{Action: "makeup.auto_fill_from_tdbm", Entity: "section", EntityID: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := svc.SummarizeAudit(f.ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows != 9 || sum.Events != 1 {
		t.Errorf("rows/events = %d/%d, want 9 rows as 1 event", sum.Rows, sum.Events)
	}
}

func TestActiveSessions_ListsOnlySessionsThatCanStillAct(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	f.exec(`UPDATE users SET first_name='วิไล', last_name='ทำงาน' WHERE id=$1`, f.StaffID)
	ua := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"

	ins := func(user uuid.UUID, lastActivity, expires string, revoked bool) uuid.UUID {
		id := uuid.New()
		f.exec(`INSERT INTO sessions (id, user_id, last_activity_at, expires_at, revoked_at, ip, user_agent)
		        VALUES ($1, $2, NOW() - $3::interval, NOW() + $4::interval,
		                CASE WHEN $5 THEN NOW() END, '203.0.113.9', $6)`,
			id, user, lastActivity, expires, revoked, ua)
		return id
	}
	live := ins(f.StaffID, "1 minute", "1 hour", false)
	ins(f.TAID, "20 minutes", "1 hour", false)        // idle past the timeout
	ins(f.LecturerID, "1 minute", "1 hour", true)     // revoked
	ins(f.LecturerID, "1 minute", "-1 minute", false) // token expired

	got, err := svc.ActiveSessions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != live {
		t.Fatalf("active sessions = %+v, want only the live one", got)
	}
	s := got[0]
	if s.Name != "วิไล ทำงาน" || s.Device != "Safari บน iPhone" || s.Network != "public" || len(s.Roles) == 0 {
		t.Errorf("session = %+v", s)
	}
}

// Opening an item from the overview asks for one action by name. Read as a
// prefix it also returned the action's longer-named siblings — rows the item
// had not counted.
func TestListAudit_AWholeActionNameIsNotAPrefix(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "worklog.staff_edit", Entity: "work_log", EntityID: "a"},
		audit.Entry{Action: "worklog.staff_edit_batch", Entity: "worklog_edit_batch", EntityID: "b"},
		audit.Entry{Action: "worklog.approve", Entity: "assignment", EntityID: "c"},
	)
	for q, want := range map[string]int{
		"worklog.staff_edit":                 1,
		"worklog.":                           3,
		"worklog.staff_edit,worklog.approve": 2,
		"worklog.staff_edit, worklog.":       3,
		"worklog":                            0,
	} {
		_, total, err := svc.ListAudit(f.ctx, AuditQuery{Action: q})
		if err != nil {
			t.Fatal(err)
		}
		if total != want {
			t.Errorf("action=%q matched %d rows, want %d", q, total, want)
		}
	}
}

// The trail records every targeted search of itself, with the words searched.
// Matching those notes made a search find its own earlier record: the second
// time "zzzz" was typed it was no longer an empty result.
func TestListAudit_ASearchDoesNotFindTheRecordOfEarlierSearches(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "audit_log.search", Entity: "audit_log", Note: "q=needle-in-notes"},
		audit.Entry{Action: "audit_log.export", Entity: "audit_log", Note: "rows=3 q=needle-in-notes"},
		audit.Entry{Action: "worklog.staff_edit", Entity: "work_log", EntityID: "x", Note: "แก้ตาม needle-in-notes"},
	)
	rows, total, err := svc.ListAudit(f.ctx, AuditQuery{Q: "needle-in-notes"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || rows[0].Action != "worklog.staff_edit" {
		t.Errorf("search returned %d rows (%v), want only the row whose own note says it", total, rows)
	}
	// Still reachable when asked for by what they are.
	if _, total, err = svc.ListAudit(f.ctx, AuditQuery{Action: "audit_log."}); err != nil || total != 2 {
		t.Errorf("the search/export records themselves: total=%d err=%v, want 2", total, err)
	}
}

// course.settlement_mode writes "old → new" in its note and also records the
// images. Said twice, the same transition read as two different facts.
func TestDecorate_DoesNotSayTheSameChangeTwice(t *testing.T) {
	note := "chronological → spread"
	r := AuditRow{Action: "course.settlement_mode", Note: &note,
		Before: json.RawMessage(`{"settlement_mode":"chronological"}`),
		After:  json.RawMessage(`{"settlement_mode":"spread"}`)}
	decorate(&r)
	if len(r.Changes) != 1 || r.Details != nil {
		t.Errorf("changes=%+v details=%v, want the change once and no detail restating it", r.Changes, r.Details)
	}

	// With no images the note is the only place the change is written down.
	r = AuditRow{Action: "course.settlement_mode", Note: &note}
	decorate(&r)
	if len(r.Details) != 1 || r.Details[0] != "จ่ายตามลำดับเวลา → เฉลี่ยทุกเดือน" {
		t.Errorf("details=%v, want the note kept when nothing else says it", r.Details)
	}
}
