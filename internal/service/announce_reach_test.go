package service

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// The announcement feature had never been exercised — the table was empty in
// every environment. These tests cover the two bugs that reading found, plus
// the reach added on 06/08/2026 (named email recipients, public share links).

type annFixture struct {
	t   *testing.T
	ctx context.Context
	svc *AnnounceService
}

func newAnnFixture(t *testing.T) *annFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	notify := &NotifyService{pool: pool, mailer: mail.New(config.Config{})}
	return &annFixture{
		t: t, ctx: context.Background(),
		svc: &AnnounceService{pool: pool, aud: audit.New(pool), notify: notify},
	}
}

func (f *annFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.svc.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture exec: %v\nSQL: %s", err, sql)
	}
}

// staffUser returns an actor id that satisfies the created_by foreign key.
func (f *annFixture) user(role, tag string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO users (id, email, first_name, last_name, is_active)
	        VALUES ($1, $2, $3, 'ทดสอบ', TRUE)`,
		id, tag+"-"+id.String()+"@example.test", tag)
	if role != "" {
		f.exec(`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, role)
	}
	return id
}

func boolPtr(b bool) *bool { return &b }

func past() *time.Time   { t := time.Now().Add(-time.Hour); return &t }
func future() *time.Time { t := time.Now().Add(24 * time.Hour); return &t }

// ---------------------------------------------------------------------------
// The two bugs reading found
// ---------------------------------------------------------------------------

// Thai is three bytes per character, so every length gate written with len()
// rejected Thai at a third of the limit its own message promised. A 100-char
// Thai title was refused as "ยาวเกิน 200 ตัวอักษร".
func TestAnnounce_LengthLimitsCountCharactersNotBytes(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")

	title := strings.Repeat("ประกาศ", 33) // 198 characters, 594 bytes
	if utf8.RuneCountInString(title) > 200 || len(title) <= 200 {
		t.Fatalf("fixture wrong: %d chars / %d bytes", utf8.RuneCountInString(title), len(title))
	}
	if _, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: title, Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
	}); err != nil {
		t.Fatalf("a %d-character Thai title is inside the 200-character limit: %v",
			utf8.RuneCountInString(title), err)
	}

	// ...and the limit still bites one character past it.
	tooLong := strings.Repeat("ก", 201)
	if _, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: tooLong, Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
	}); err == nil {
		t.Fatal("201 characters must still be refused")
	}
}

// The in-app preview and the email body used to be cut with body[:240], which
// lands inside a Thai glyph and ships invalid UTF-8 to the reader.
func TestTruncateRunes_NeverSplitsACharacter(t *testing.T) {
	body := strings.Repeat("ขอเชิญผู้ช่วยสอนเข้าร่วมประชุมชี้แจงการเบิกจ่าย ", 20)
	if len(body) <= 240 {
		t.Fatal("fixture body must be longer than the cut")
	}
	got := truncateRunes(body, 240)
	if !utf8.ValidString(got) {
		t.Fatalf("cut produced invalid UTF-8: %q", got[len(got)-8:])
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n > 240 {
		t.Fatalf("kept %d characters, want at most 240", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated preview should show that it was cut")
	}
	// Short text passes through untouched, ellipsis and all.
	if got := truncateRunes("สั้น", 240); got != "สั้น" {
		t.Errorf("short text was altered: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Named email recipients
// ---------------------------------------------------------------------------

// The trap this API shape sets: several screens edit an announcement by
// rebuilding the whole document from a LIST row, which carries neither the
// share flag nor the recipient ledger. Pinning a post must not quietly switch
// off its public link or delete everyone queued for email.
func TestUpsert_APayloadWithoutTheNewFieldsChangesNeither(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")

	id, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: "ประกาศ", Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
		IsPublic: boolPtr(true), TargetFilters: &[]string{"ta_missing_documents"},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Exactly what the pin button sends: the document as the list knows it.
	if _, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		ID: id, Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, Pinned: true,
	}); err != nil {
		t.Fatalf("Upsert (pin): %v", err)
	}

	got, err := f.svc.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pinned {
		t.Error("the pin itself did not take")
	}
	if !got.IsPublic {
		t.Error("pinning switched off the public share link")
	}
	if len(got.TargetFilters) != 1 || got.TargetFilters[0] != "ta_missing_documents" {
		t.Errorf("pinning wiped the targeting rule: %+v", got.TargetFilters)
	}
}

// ---------------------------------------------------------------------------
// Public share links
// ---------------------------------------------------------------------------

// Everything an anonymous reader must NOT reach, in one place. Each case is a
// way the share link could leak an internal notice.
func TestPublicGet_OnlyOpensWhatStaffDeliberatelyShared(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")

	mk := func(name string, in UpsertInput) uuid.UUID {
		in.Title, in.Body, in.Category = name, "เนื้อหา", "info"
		in.Audience = []string{"ta"}
		id, err := f.svc.Upsert(f.ctx, actor, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return id
	}

	shared := mk("เปิดสาธารณะและเผยแพร่แล้ว", UpsertInput{IsPublic: boolPtr(true), PublishedAt: past()})
	internal := mk("ไม่ได้เปิดสาธารณะ", UpsertInput{PublishedAt: past()})
	draft := mk("เปิดสาธารณะแต่ยังไม่เผยแพร่", UpsertInput{IsPublic: boolPtr(true)})
	scheduled := mk("เปิดสาธารณะ ตั้งเวลาไว้", UpsertInput{IsPublic: boolPtr(true), PublishedAt: future()})

	expired := mk("เปิดสาธารณะแต่หมดอายุแล้ว", UpsertInput{IsPublic: boolPtr(true), PublishedAt: past()})
	f.exec(`UPDATE announcements SET expires_at = NOW() - INTERVAL '1 minute' WHERE id=$1`, expired)

	if got, err := f.svc.PublicGet(f.ctx, shared); err != nil || got == nil {
		t.Fatalf("a shared, live announcement must be readable: %v", err)
	}
	for name, id := range map[string]uuid.UUID{
		"ไม่ได้เปิดสาธารณะ": internal,
		"ยังเป็นฉบับร่าง":   draft,
		"ตั้งเวลาไว้":       scheduled,
		"หมดอายุแล้ว":       expired,
		"ไม่มีอยู่จริง":     uuid.New(),
	} {
		if _, err := f.svc.PublicGet(f.ctx, id); err != ErrNotFound {
			t.Errorf("%s: err=%v, want ErrNotFound — anything else tells an anonymous "+
				"caller that the announcement exists", name, err)
		}
	}
}

// The public payload is the notice, not the bookkeeping around it.
func TestPublicGet_DoesNotLeakAudienceOrRecipients(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")

	id, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: "ประกาศสาธารณะ", Body: "เนื้อหา", Category: "news",
		Audience: []string{"ta", "lecturer"}, IsPublic: boolPtr(true), PublishedAt: past(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := f.svc.PublicGet(f.ctx, id)
	if err != nil {
		t.Fatalf("PublicGet: %v", err)
	}
	if len(got.Audience) != 0 {
		t.Errorf("audience leaked to an anonymous reader: %v", got.Audience)
	}
	if len(got.Recipients) != 0 {
		t.Errorf("the recipient email list leaked to an anonymous reader: %+v", got.Recipients)
	}
	if got.Title != "ประกาศสาธารณะ" || got.Body != "เนื้อหา" {
		t.Error("the notice itself should still come through")
	}
}

// Delivery is one pass over the frozen audience. It used to be two loops (a
// role fanout plus a separate list), which after targeting merged the two sets
// would have mailed everybody twice.
func TestDeliver_ReachesEachPersonExactlyOnce(t *testing.T) {
	w := newTargetWorld(t)

	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ประกาศถึงผู้ช่วยสอน", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, TargetTermID: &w.term, PublishedAt: past(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// notify.Send deliberately writes two rows per person — the in-app notice
	// and a record that the email went out. Counting the in-app one answers
	// "how many times was this person told".
	countNotices := func(u uuid.UUID) int {
		var n int
		if err := w.svc.pool.QueryRow(w.ctx,
			`SELECT COUNT(*) FROM notifications
			  WHERE user_id=$1 AND link=$2 AND channel='in_app'`,
			u, "/announcements/"+id.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := countNotices(w.taReady); got != 1 {
		t.Fatalf("the TA got %d notifications for one announcement, want 1", got)
	}

	// Pressing "send again" must reach nobody a second time.
	sent, err := w.svc.Deliver(w.ctx, id)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if sent != 0 {
		t.Fatalf("a second delivery sent to %d people", sent)
	}
	if got := countNotices(w.taReady); got != 1 {
		t.Fatalf("after re-sending the TA has %d notifications, want 1", got)
	}
}

// Widening the target after publishing must reach only the people added.
func TestDeliver_OnlyReachesPeopleAddedSincePublishing(t *testing.T) {
	w := newTargetWorld(t)

	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, TargetTermID: &w.term, PublishedAt: past(),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Now also aim it at the lecturer.
	if _, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		ID: id, Title: "ประกาศ", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta", "lecturer"}, TargetTermID: &w.term, PublishedAt: past(),
	}); err != nil {
		t.Fatalf("Upsert (widen): %v", err)
	}

	var lecturerNotices, taNotices int
	link := "/announcements/" + id.String()
	if err := w.svc.pool.QueryRow(w.ctx,
		`SELECT COUNT(*) FROM notifications
		  WHERE user_id=$1 AND link=$2 AND channel='in_app'`, w.lecturer, link).Scan(&lecturerNotices); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.pool.QueryRow(w.ctx,
		`SELECT COUNT(*) FROM notifications
		  WHERE user_id=$1 AND link=$2 AND channel='in_app'`, w.taReady, link).Scan(&taNotices); err != nil {
		t.Fatal(err)
	}
	if lecturerNotices != 1 {
		t.Errorf("the newly targeted lecturer got %d notifications, want 1", lecturerNotices)
	}
	if taNotices != 1 {
		t.Errorf("the TA who already had it got %d notifications, want 1", taNotices)
	}
}

// Delivering a draft would point people at a page they cannot open.
func TestDeliver_RefusesWhileUnpublished(t *testing.T) {
	w := newTargetWorld(t)
	id, err := w.svc.Upsert(w.ctx, w.officer, UpsertInput{
		Title: "ยังไม่เผยแพร่", Body: "เนื้อหา", Category: "info",
		Audience: []string{"ta"}, TargetTermID: &w.term,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := w.svc.Deliver(w.ctx, id); err == nil {
		t.Fatal("a draft must not be delivered")
	}
}
