package service

import (
	"testing"

	"github.com/google/uuid"
)

// Targeting decides who is told about a deadline. Getting it wrong is silent
// in both directions: too wide and people learn to ignore announcements, too
// narrow and the person who needed it never hears.
//
// Every test here goes through ResolveAudience — the one resolver the feed,
// the fanout and the email all use — rather than asserting on SQL.

// targetWorld builds a small faculty: two TAs (one has filed documents and a
// timetable, one has filed neither), a lecturer with a course, and a staff
// officer. Returns the ids by name.
type targetWorld struct {
	*annFixture
	term                       uuid.UUID
	taReady, taBehind          uuid.UUID
	lecturer, officer, courseA uuid.UUID
}

func newTargetWorld(t *testing.T) *targetWorld {
	t.Helper()
	f := newAnnFixture(t)
	w := &targetWorld{annFixture: f}

	w.term = uuid.New()
	f.exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active)
	        VALUES ($1, 2569, 1, TRUE)`, w.term)

	w.officer = f.user("staff", "เจ้าหน้าที่")
	w.lecturer = f.user("lecturer", "อาจารย์")
	w.taReady = f.user("ta", "ทีเอพร้อม")
	w.taBehind = f.user("ta", "ทีเอค้าง")

	// The ready TA has all three documents and a timetable of their own.
	for _, kind := range []string{"national_id", "bank_book", "creditor_form"} {
		f.exec(`INSERT INTO ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key)
		        VALUES (gen_random_uuid(), $1, $2, 'f.pdf', 'application/pdf', 10, 'k/'||gen_random_uuid())`,
			w.taReady, kind)
	}
	f.exec(`INSERT INTO ta_class_schedules
	          (id, user_id, term_id, course_code, course_name, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, 'ZZ000', 'วิชาของทีเอ', '1', 'lecture', 1, '09:00', '10:00')`,
		w.taReady, w.term)

	// A course the lecturer teaches.
	w.courseA = uuid.New()
	f.exec(`INSERT INTO teaching_courses (id, term_id, code, name_th, level, credits, lecture_hrs, num_students)
	        VALUES ($1, $2, 'CP101', 'วิชาทดสอบ', 'undergrad', 3, 3, 30)`, w.courseA, w.term)
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, TRUE)`, w.courseA, w.lecturer)

	return w
}

// ids extracts the resolved ids as a set for order-independent comparison.
func (w *targetWorld) resolve(t *testing.T, r AudienceRule) map[uuid.UUID]bool {
	t.Helper()
	if r.TermID == nil {
		r.TermID = &w.term
	}
	members, err := w.svc.ResolveAudience(w.ctx, r)
	if err != nil {
		t.Fatalf("ResolveAudience: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, m := range members {
		out[m.ID] = true
	}
	return out
}

func TestAudience_RoleSelectsThatRoleOnly(t *testing.T) {
	w := newTargetWorld(t)
	got := w.resolve(t, AudienceRule{Roles: []string{"ta"}})
	if !got[w.taReady] || !got[w.taBehind] {
		t.Error("both TAs should be selected")
	}
	if got[w.lecturer] || got[w.officer] {
		t.Error("a role target must not reach other roles")
	}
}

// The headline case the officers asked for: a role NARROWED by a condition,
// not a role PLUS everyone matching the condition.
func TestAudience_FilterNarrowsTheRoleRatherThanAddingToIt(t *testing.T) {
	w := newTargetWorld(t)
	got := w.resolve(t, AudienceRule{
		Roles: []string{"ta"}, Filters: []string{"ta_missing_documents"},
	})
	if !got[w.taBehind] {
		t.Error("the TA missing documents must be reached — that is the whole point")
	}
	if got[w.taReady] {
		t.Error("the TA who already filed must NOT be reached; the filter narrows, it does not add")
	}
	if len(got) != 1 {
		t.Errorf("selected %d people, want exactly 1", len(got))
	}
}

func TestAudience_MissingScheduleFilter(t *testing.T) {
	w := newTargetWorld(t)
	got := w.resolve(t, AudienceRule{Filters: []string{"ta_missing_schedule"}})
	if !got[w.taBehind] || got[w.taReady] {
		t.Errorf("wrong set for ยังไม่ทำตารางเรียน: behind=%v ready=%v", got[w.taBehind], got[w.taReady])
	}
}

// Several conditions at once must ALL hold, not any.
func TestAudience_TwoFiltersBothApply(t *testing.T) {
	w := newTargetWorld(t)
	// Give the behind TA a timetable: now they fail documents but pass schedule.
	w.exec(`INSERT INTO ta_class_schedules
	          (id, user_id, term_id, course_code, course_name, sec_no, kind, day_of_week, start_time, end_time)
	        VALUES (gen_random_uuid(), $1, $2, 'ZZ111', 'วิชา', '1', 'lecture', 2, '09:00', '10:00')`,
		w.taBehind, w.term)

	got := w.resolve(t, AudienceRule{
		Filters: []string{"ta_missing_documents", "ta_missing_schedule"},
	})
	if len(got) != 0 {
		t.Errorf("nobody is missing BOTH now, got %d — the filters are being ORed", len(got))
	}
}

// A course target reaches the people attached to that course, and nobody else.
func TestAudience_CourseReachesItsLecturerAndAppointedTAs(t *testing.T) {
	w := newTargetWorld(t)

	// Appoint the ready TA to the course.
	req, sec, asg := uuid.New(), uuid.New(), uuid.New()
	w.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track) VALUES ($1,$2,'1','regular')`, sec, w.courseA)
	w.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
	        VALUES ($1,$2,$3,'both','approved',NOW())`, req, w.courseA, w.lecturer)
	w.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,'undergrad')`, asg, req, sec, w.taReady)

	got := w.resolve(t, AudienceRule{CourseIDs: []uuid.UUID{w.courseA}})
	if !got[w.lecturer] {
		t.Error("the course's lecturer should be reached")
	}
	if !got[w.taReady] {
		t.Error("a TA appointed to the course should be reached")
	}
	if got[w.taBehind] || got[w.officer] {
		t.Error("a course target must not reach people outside the course")
	}

	// A dropped assignment is not on the course any more.
	w.exec(`UPDATE ta_request_assignments SET state='dropped' WHERE id=$1`, asg)
	got = w.resolve(t, AudienceRule{CourseIDs: []uuid.UUID{w.courseA}})
	if got[w.taReady] {
		t.Error("a dropped TA is no longer attached to the course")
	}
}

// "ส่งถึงคนเดียว" has to be exactly one person.
func TestAudience_NamedPersonSelectsOnlyThem(t *testing.T) {
	w := newTargetWorld(t)
	got := w.resolve(t, AudienceRule{UserIDs: []uuid.UUID{w.taBehind}})
	if len(got) != 1 || !got[w.taBehind] {
		t.Errorf("a single-person target selected %d people", len(got))
	}
}

// Bases combine as a union: role OR named person.
func TestAudience_BasesUnion(t *testing.T) {
	w := newTargetWorld(t)
	got := w.resolve(t, AudienceRule{
		Roles: []string{"lecturer"}, UserIDs: []uuid.UUID{w.taBehind},
	})
	if !got[w.lecturer] || !got[w.taBehind] {
		t.Error("role and named person should both be included")
	}
	if got[w.taReady] {
		t.Error("nothing selected the other TA")
	}
}

// An empty rule means everyone — but the preview has to SAY so, because an
// officer reaching that by leaving fields blank is usually an accident.
func TestAudience_EmptyRuleIsEveryoneAndSaysSo(t *testing.T) {
	w := newTargetWorld(t)
	p, err := w.svc.PreviewAudience(w.ctx, AudienceRule{TermID: &w.term})
	if err != nil {
		t.Fatalf("PreviewAudience: %v", err)
	}
	if !p.Everyone {
		t.Error("an empty rule must be flagged as 'everyone', not shown as a plain number")
	}
	if p.Total < 4 {
		t.Errorf("everyone = %d, want at least the 4 people in the fixture", p.Total)
	}
}

// Deactivated accounts never receive anything, whatever the rule says.
func TestAudience_SkipsInactiveAccounts(t *testing.T) {
	w := newTargetWorld(t)
	w.exec(`UPDATE users SET is_active = FALSE WHERE id = $1`, w.taBehind)
	got := w.resolve(t, AudienceRule{Roles: []string{"ta"}})
	if got[w.taBehind] {
		t.Error("a deactivated account must not be targeted")
	}
	if !got[w.taReady] {
		t.Error("the active TA should still be there")
	}
}

// A filter name with no query behind it must be refused, not silently ignored:
// ignoring it would widen the audience without telling anyone.
func TestAudience_RejectsUnknownFilter(t *testing.T) {
	w := newTargetWorld(t)
	if _, err := w.svc.ResolveAudience(w.ctx, AudienceRule{
		Filters: []string{"ta_has_a_nice_haircut"}, TermID: &w.term,
	}); err == nil {
		t.Fatal("an unknown filter must be refused")
	}
}

// The feed and the email must be the same set. This is the property the whole
// materialise-on-publish design exists for.
func TestAudience_FeedShowsExactlyWhoWasTargeted(t *testing.T) {
	w := newTargetWorld(t)

	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ถึงทีเอที่ยังส่งเอกสารไม่ครบ", Body: "กรุณาส่งเอกสารภายในสัปดาห์นี้",
		Category: "warning", Audience: []string{"ta"},
		TargetFilters: &[]string{"ta_missing_documents"},
		TargetTermID:  &w.term,
		PublishedAt:   past(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	seenBy := func(u uuid.UUID) bool {
		list, err := w.svc.List(w.ctx, ListFilter{ViewerID: u})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, a := range list {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	if !seenBy(w.taBehind) {
		t.Error("the targeted TA cannot see the announcement they were emailed about")
	}
	if seenBy(w.taReady) {
		t.Error("a TA outside the target can see it — the feed is wider than the delivery")
	}
	if seenBy(w.lecturer) {
		t.Error("a lecturer outside the target can see a TA-only announcement")
	}
}
