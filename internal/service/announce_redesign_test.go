package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// 01/10/2026 — the ประชาสัมพันธ์ page was rebuilt after reading found that
// several things it promised on screen were not what the server did. Each test
// here pins one of them.

// appoint puts a TA on the fixture course.
func (w *targetWorld) appoint(ta uuid.UUID) uuid.UUID {
	w.t.Helper()
	req, sec, asg := uuid.New(), uuid.New(), uuid.New()
	w.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track) VALUES ($1,$2,$3,'regular')`,
		sec, w.courseA, sec.String()[:4])
	w.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
	        VALUES ($1,$2,$3,'both','approved',NOW())`, req, w.courseA, w.lecturer)
	w.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES ($1,$2,$3,$4,'undergrad')`, asg, req, sec, ta)
	return asg
}

func (w *targetWorld) seenBy(id, u uuid.UUID) bool {
	w.t.Helper()
	list, err := w.svc.List(w.ctx, ListFilter{ViewerID: u})
	if err != nil {
		w.t.Fatalf("List: %v", err)
	}
	for _, a := range list {
		if a.ID == id {
			return true
		}
	}
	return false
}

func (w *targetWorld) notices(id, u uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.svc.pool.QueryRow(w.ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND link=$2 AND channel='in_app'`,
		u, "/announcements/"+id.String()).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// The form said "เจาะกลุ่มให้แคบลง" while the server ORed the course onto the
// roles. With the default roles ticked, adding one course narrowed nothing.
func TestAudience_CourseNarrowsTheRole(t *testing.T) {
	w := newTargetWorld(t)
	w.appoint(w.taReady)

	got := w.resolve(t, AudienceRule{Roles: []string{"ta"}, CourseIDs: []uuid.UUID{w.courseA}})
	if !got[w.taReady] {
		t.Error("the TA appointed to the course must be reached")
	}
	if got[w.taBehind] {
		t.Error("a TA who is not on the course was reached: the course did not narrow the role")
	}
	if got[w.lecturer] {
		t.Error("the course's lecturer was reached by a TA-only target")
	}
	if len(got) != 1 {
		t.Errorf("selected %d people, want exactly 1", len(got))
	}
}

// Naming someone is explicit: a condition on the group must not drop them.
func TestAudience_ConditionDoesNotFilterNamedPeople(t *testing.T) {
	w := newTargetWorld(t)
	got := w.resolve(t, AudienceRule{
		Roles: []string{"ta"}, Filters: []string{"ta_missing_documents"},
		UserIDs: []uuid.UUID{w.lecturer},
	})
	if !got[w.taBehind] || !got[w.lecturer] {
		t.Errorf("want the behind TA and the named lecturer, got %v", got)
	}
	if got[w.taReady] {
		t.Error("the condition stopped applying to the group once a name was added")
	}
}

func TestUpsert_RefusesToPublishToNobody(t *testing.T) {
	w := newTargetWorld(t)
	in := UpsertInput{Title: "ไม่มีผู้รับ", Body: "เนื้อหา", Category: "info", TargetTermID: &w.term}

	in.PublishedAt = past()
	if _, err := w.svc.Upsert(w.ctx, w.officer, in); !errors.Is(err, ErrNoAudience) {
		t.Fatalf("publishing with no target: err = %v, want ErrNoAudience", err)
	}
	in.PublishedAt = future()
	if _, err := w.svc.Upsert(w.ctx, w.officer, in); !errors.Is(err, ErrNoAudience) {
		t.Fatalf("scheduling with no target: err = %v, want ErrNoAudience", err)
	}
	var n int
	if err := w.svc.pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM announcements`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a refused publish left %d rows behind", n)
	}

	// Opened to the public, the share link is a channel of its own.
	pub := in
	pub.IsPublic, pub.PublishedAt = boolPtr(true), past()
	if _, err := w.svc.Upsert(w.ctx, w.officer, pub); err != nil {
		t.Fatalf("a public announcement with nobody inside to notify was refused: %v", err)
	}
	w.exec(`DELETE FROM announcements`)

	// A draft may be saved before its target is decided.
	in.PublishedAt = nil
	id, err := w.svc.Upsert(w.ctx, w.officer, in)
	if err != nil {
		t.Fatalf("saving a draft with no target: %v", err)
	}
	if err := w.svc.Publish(w.ctx, w.officer, id); !errors.Is(err, ErrNoAudience) {
		t.Fatalf("Publish with no target: err = %v, want ErrNoAudience", err)
	}
}

// Saving a live announcement used to re-run its conditions and mail whoever
// newly matched — on a typo fix, even on the pin toggle.
func TestUpsert_EditingALiveNoticeWithoutTouchingTheTargetTellsNobody(t *testing.T) {
	w := newTargetWorld(t)
	in := UpsertInput{
		Title: "ส่งเอกสารให้ครบ", Body: "เนื้อหา", Category: "warning",
		Audience: []string{"ta"}, TargetFilters: &[]string{"ta_missing_documents"},
		TargetTermID: &w.term, PublishedAt: past(),
	}
	id, err := w.svc.Upsert(w.ctx, w.officer, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if w.notices(id, w.taBehind) != 1 || w.notices(id, w.taReady) != 0 {
		t.Fatal("fixture: only the TA missing documents should have been told")
	}

	// The other TA now matches the condition too.
	w.exec(`DELETE FROM ta_documents WHERE user_id = $1`, w.taReady)

	// The pin toggle's payload: a list row, with no target fields at all.
	if _, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		ID: id, Title: in.Title, Body: in.Body, Category: in.Category,
		Audience: in.Audience, Pinned: true, PublishedAt: in.PublishedAt,
	}); err != nil {
		t.Fatalf("Upsert (pin): %v", err)
	}
	// And a typo fix from the composer, which sends the same rule again.
	in.ID, in.Title = id, "ส่งเอกสารให้ครบภายในวันศุกร์"
	if _, err := w.svc.Upsert(w.ctx, w.officer, in); err != nil {
		t.Fatalf("Upsert (edit): %v", err)
	}

	if n := w.notices(id, w.taReady); n != 0 {
		t.Errorf("a TA who newly matched was told %d times by an edit that did not touch the target", n)
	}
	if w.seenBy(id, w.taReady) {
		t.Error("the frozen audience grew without the officer changing it")
	}
	if !w.seenBy(id, w.taBehind) {
		t.Error("the original recipient lost the announcement")
	}
}

// The ledger only ever grew, so narrowing a published notice changed nothing.
func TestUpsert_NarrowingALiveNoticeTakesItAwayFromThePeopleRemoved(t *testing.T) {
	w := newTargetWorld(t)
	in := UpsertInput{
		Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta", "lecturer"}, TargetTermID: &w.term, PublishedAt: past(),
	}
	id, err := w.svc.Upsert(w.ctx, w.officer, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !w.seenBy(id, w.lecturer) {
		t.Fatal("fixture: the lecturer should see it first")
	}

	in.ID, in.Audience = id, []string{"ta"}
	if _, err := w.svc.Upsert(w.ctx, w.officer, in); err != nil {
		t.Fatalf("Upsert (narrow): %v", err)
	}
	if w.seenBy(id, w.lecturer) {
		t.Error("the lecturer was removed from the target but still sees the announcement")
	}
	if !w.seenBy(id, w.taReady) || w.notices(id, w.taReady) != 1 {
		t.Error("a TA who stayed in the target lost it or was told twice")
	}
}

func TestPublish_RefusesAnExpiredNoticeInsteadOfPretending(t *testing.T) {
	w := newTargetWorld(t)
	gone := time.Now().Add(-time.Minute)
	start := time.Now().Add(-2 * time.Hour)
	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "หมดอายุแล้ว", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, TargetTermID: &w.term,
		PublishedAt: &start, ExpiresAt: &gone,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := w.svc.Publish(w.ctx, w.officer, id); err == nil {
		t.Fatal("publishing an expired announcement reported success while it stayed hidden")
	}
}

// "Publish" on a scheduled row kept its future time, so nothing happened.
func TestPublish_MakesAScheduledNoticeLiveNow(t *testing.T) {
	w := newTargetWorld(t)
	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ตั้งเวลาไว้", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, TargetTermID: &w.term, PublishedAt: future(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if w.seenBy(id, w.taReady) || w.notices(id, w.taReady) != 0 {
		t.Fatal("a scheduled announcement reached someone early")
	}
	if err := w.svc.Publish(w.ctx, w.officer, id); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	a, err := w.svc.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "live" {
		t.Fatalf("status after Publish = %q, want live", a.Status)
	}
	if !w.seenBy(id, w.taReady) || w.notices(id, w.taReady) != 1 {
		t.Error("publishing now did not deliver")
	}
}

// Republishing after narrowing must not resurrect people from the first run,
// and must not tell anybody twice.
func TestPublish_AfterUnpublishUsesTheRuleAsItIsNow(t *testing.T) {
	w := newTargetWorld(t)
	in := UpsertInput{
		Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta", "lecturer"}, TargetTermID: &w.term, PublishedAt: past(),
	}
	id, err := w.svc.Upsert(w.ctx, w.officer, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := w.svc.Unpublish(w.ctx, w.officer, id); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	in.ID, in.Audience, in.PublishedAt = id, []string{"ta"}, nil
	if _, err := w.svc.Upsert(w.ctx, w.officer, in); err != nil {
		t.Fatalf("Upsert (draft): %v", err)
	}
	if err := w.svc.Publish(w.ctx, w.officer, id); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if w.seenBy(id, w.lecturer) {
		t.Error("the lecturer was dropped while it was a draft but sees it again")
	}
	if n := w.notices(id, w.taReady); n != 1 {
		t.Errorf("the TA was told %d times across unpublish and republish, want 1", n)
	}
}

// Every recipient used to be marked "sent" whatever the mailer answered.
func TestDeliver_RecordsAnEmailThatDidNotGoOut(t *testing.T) {
	pool := testutil.NewPool(t)
	// Port 1 on loopback refuses the connection at once.
	deadMail := mail.New(config.Config{SMTPHost: "127.0.0.1", SMTPPort: 1, MailFrom: "noreply@example.test"})
	f := &annFixture{t: t, ctx: context.Background(), svc: &AnnounceService{
		pool: pool, aud: audit.New(pool), notify: &NotifyService{pool: pool, mailer: deadMail},
	}}
	officer, ta := f.user("staff", "officer"), f.user("ta", "ta")

	id, err := f.svc.Upsert(f.ctx, officer, UpsertInput{
		Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, PublishedAt: past(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	a, err := f.svc.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Recipients) != 1 || a.Recipients[0].Status != "failed" || a.Recipients[0].Error == "" {
		t.Fatalf("recipient = %+v, want one row marked failed with the reason", a.Recipients)
	}
	if a.FailedCount != 1 {
		t.Errorf("failed_count = %d, want 1", a.FailedCount)
	}

	// The bell line was still written, and a retry must not write a second one.
	bell := func() (n int) {
		if err := pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND channel='in_app'`, ta).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	if bell() != 1 {
		t.Fatalf("in-app notices = %d, want 1 even though the email failed", bell())
	}
	res, err := f.svc.Redeliver(f.ctx, id)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if res.Failed != 1 || res.Sent != 0 {
		t.Errorf("retry = %+v, want it attempted and failed again", res)
	}
	if bell() != 1 {
		t.Errorf("retrying the email wrote a second bell line (%d)", bell())
	}
	// The automatic path leaves failed rows alone.
	if sent, _ := f.svc.Deliver(f.ctx, id); sent != 0 {
		t.Errorf("Deliver retried a failed row on its own")
	}

	// Staff list carries the count without opening the announcement.
	list, err := f.svc.List(f.ctx, ListFilter{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AudienceCount != 1 || list[0].FailedCount != 1 {
		t.Errorf("staff list counts = %+v", list)
	}
}

func TestRemindUnread_ReachesOnlyThoseWhoHaveNotOpenedIt(t *testing.T) {
	w := newTargetWorld(t)
	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, TargetTermID: &w.term, PublishedAt: past(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	w.svc.MarkRead(w.ctx, id, w.taReady)
	w.svc.MarkRead(w.ctx, id, w.lecturer) // not a recipient: must be a no-op

	a, _ := w.svc.Get(w.ctx, id)
	if a.ReadCount != 1 || a.AudienceCount != 2 {
		t.Fatalf("read %d of %d, want 1 of 2", a.ReadCount, a.AudienceCount)
	}

	n, err := w.svc.RemindUnread(w.ctx, w.officer, id)
	if err != nil {
		t.Fatalf("RemindUnread: %v", err)
	}
	if n != 1 {
		t.Fatalf("reminded %d people, want 1", n)
	}
	if got := w.notices(id, w.taBehind); got != 2 {
		t.Errorf("the unread TA has %d notices, want the original and the reminder", got)
	}
	if got := w.notices(id, w.taReady); got != 1 {
		t.Errorf("the TA who already read it has %d notices, want 1", got)
	}
	if _, err := w.svc.RemindUnread(w.ctx, w.officer, id); err == nil {
		t.Error("a second reminder inside 24 hours was allowed")
	}
}

func TestRemindUnread_RefusesADraft(t *testing.T) {
	w := newTargetWorld(t)
	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ร่าง", Body: "เนื้อหา", Category: "info", Audience: []string{"ta"}, TargetTermID: &w.term,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := w.svc.RemindUnread(w.ctx, w.officer, id); err == nil {
		t.Fatal("a draft must not be reminded about")
	}
}

// Editing a targeted announcement showed UUIDs: the names were never loaded.
func TestGet_NamesTheCoursesAndPeopleOfTheTarget(t *testing.T) {
	w := newTargetWorld(t)
	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ร่าง", Body: "เนื้อหา", Category: "info",
		TargetCourseIDs: &[]uuid.UUID{w.courseA}, TargetUserIDs: &[]uuid.UUID{w.taBehind},
		TargetTermID: &w.term,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	a, err := w.svc.Get(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.TargetCourses) != 1 || a.TargetCourses[0].Code != "CP101" {
		t.Errorf("target_courses = %+v", a.TargetCourses)
	}
	if len(a.TargetUsers) != 1 || a.TargetUsers[0].ID != w.taBehind || a.TargetUsers[0].Name == "" {
		t.Errorf("target_users = %+v", a.TargetUsers)
	}
}
