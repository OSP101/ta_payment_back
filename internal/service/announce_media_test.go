package service

import (
	"testing"

	"github.com/google/uuid"
)

// Attachments are the part of this feature that can leak. A key served without
// a check is a read of the whole store; a key refused too eagerly is a broken
// image on a page shared to the public. MediaIsPublic is the only thing between
// the two, so it gets tested from every angle.

func attachTo(t *testing.T, f *annFixture, id uuid.UUID, in ...AttachmentInput) {
	t.Helper()
	tx, err := f.svc.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := f.svc.saveAttachments(f.ctx, tx, id, in); err != nil {
		t.Fatalf("saveAttachments: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func img(key string) AttachmentInput {
	return AttachmentInput{
		Kind: "image", StorageKey: key, Filename: "photo.jpg",
		Mime: "image/jpeg", SizeBytes: 1234,
	}
}

// The rule, stated as a table: a key is public only while the announcement
// carrying it is public AND live.
func TestMediaIsPublic_FollowsTheAnnouncementItBelongsTo(t *testing.T) {
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

	sharedID := mk("แชร์สาธารณะ", UpsertInput{IsPublic: boolPtr(true), PublishedAt: past()})
	internalID := mk("ภายใน", UpsertInput{PublishedAt: past()})
	draftID := mk("ร่างแต่เปิดสาธารณะ", UpsertInput{IsPublic: boolPtr(true)})

	attachTo(t, f, sharedID, img("announcements/2026/08/06/shared.jpg"))
	attachTo(t, f, internalID, img("announcements/2026/08/06/internal.jpg"))
	attachTo(t, f, draftID, img("announcements/2026/08/06/draft.jpg"))

	check := func(key string, want bool, why string) {
		t.Helper()
		got, err := f.svc.MediaIsPublic(f.ctx, key)
		if err != nil {
			t.Fatalf("MediaIsPublic(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("%s: public=%v, want %v", why, got, want)
		}
	}
	check("announcements/2026/08/06/shared.jpg", true, "a file on a shared, live announcement")
	check("announcements/2026/08/06/internal.jpg", false, "a file on an internal announcement")
	check("announcements/2026/08/06/draft.jpg", false, "a file on an unpublished announcement")
	check("announcements/2026/08/06/nobody.jpg", false, "a key belonging to no announcement")

	// Withdrawing the announcement must take its pictures down with it —
	// otherwise "ถอนประกาศ" leaves the images reachable to anyone with the URL.
	if err := f.svc.Unpublish(f.ctx, actor, sharedID); err != nil {
		t.Fatal(err)
	}
	check("announcements/2026/08/06/shared.jpg", false, "a file whose announcement was withdrawn")
}

// The cover image is stored on the announcement rather than in the attachment
// table, and the public page renders it — so it needs the same permission. It
// was the actual bug: shared announcements showed a broken cover to everyone
// outside the system.
func TestMediaIsPublic_CoversTheCoverImageToo(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")

	key := "announcements/2026/08/06/cover.jpg"
	if _, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: "มีรูปหน้าปก", Body: "เนื้อหา", Category: "news", Audience: []string{"ta"},
		IsPublic: boolPtr(true), PublishedAt: past(), CoverImageKey: &key,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	ok, err := f.svc.MediaIsPublic(f.ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("the cover image of a shared announcement must be fetchable without an account")
	}
}

// Attachments are replaced wholesale and keep the composer's order, because a
// gallery reads as a sequence.
func TestSaveAttachments_ReplacesAndKeepsOrder(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")
	id, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: "มีไฟล์แนบ", Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
	})
	if err != nil {
		t.Fatal(err)
	}

	attachTo(t, f, id,
		img("announcements/a.jpg"), img("announcements/b.jpg"), img("announcements/c.jpg"))
	got, err := f.svc.loadAttachments(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].StorageKey != "announcements/a.jpg" || got[2].StorageKey != "announcements/c.jpg" {
		t.Fatalf("order not preserved: %+v", got)
	}
	if got[0].URL == "" {
		t.Error("an attachment must carry the URL a client fetches it from")
	}

	// Re-saving with a different order and one removed.
	attachTo(t, f, id, img("announcements/c.jpg"), img("announcements/a.jpg"))
	got, _ = f.svc.loadAttachments(f.ctx, id)
	if len(got) != 2 || got[0].StorageKey != "announcements/c.jpg" {
		t.Fatalf("re-save did not replace the list: %+v", got)
	}
}

// A key that did not come from this service must never be stored: the serve
// route opens whatever key it is given, so the check has to be here.
func TestSaveAttachments_RefusesForeignKeys(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")
	id, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: "ทดสอบ", Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		"ta-documents/2026/08/06/national-id.pdf", // another feature's store
		"announcements/../ta-documents/secret.pdf",
		"/etc/passwd",
	} {
		tx, err := f.svc.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = f.svc.saveAttachments(f.ctx, tx, id, []AttachmentInput{img(bad)})
		tx.Rollback(f.ctx)
		if err == nil {
			t.Errorf("key %q was accepted; the serve route would then read it", bad)
		}
	}
}

// An announcement's files travel with it into the public payload — they are
// most of the post.
func TestPublicGet_CarriesAttachments(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")
	id, err := f.svc.Upsert(f.ctx, actor, UpsertInput{
		Title: "โพสต์มีรูป", Body: "เนื้อหา", Category: "news", Audience: []string{"ta"},
		IsPublic: boolPtr(true), PublishedAt: past(),
		Attachments: &[]AttachmentInput{img("announcements/x.jpg")},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := f.svc.PublicGet(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("public payload has %d attachments, want 1", len(got.Attachments))
	}
}
