package service

import "testing"

// QUAL-04: cover_image_key checked only the "announcements/" prefix, unlike
// saveAttachments (announce_media.go) which checks prefix AND "..". A key
// like "announcements/../../etc/passwd" passed the prefix check here. This
// pins the same two checks now apply to cover_image_key.

func TestUpsert_RejectsCoverImageKeyWithTraversal(t *testing.T) {
	f := newAnnFixture(t)
	officer := f.user("staff", "เจ้าหน้าที่")

	bad := "announcements/../../../etc/passwd"
	_, err := f.svc.Upsert(f.ctx, officer, UpsertInput{
		Title: "ทดสอบ", Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
		CoverImageKey: &bad,
	})
	if err == nil {
		t.Fatalf("cover_image_key = %q must be refused, got no error", bad)
	}
}

func TestUpsert_AcceptsOrdinaryCoverImageKey(t *testing.T) {
	f := newAnnFixture(t)
	officer := f.user("staff", "เจ้าหน้าที่")

	good := "announcements/2026/01/01/abc.jpg"
	_, err := f.svc.Upsert(f.ctx, officer, UpsertInput{
		Title: "ทดสอบ", Body: "เนื้อหา", Category: "info", Audience: []string{"ta"},
		CoverImageKey: &good,
	})
	if err != nil {
		t.Fatalf("an ordinary cover_image_key must be accepted: %v", err)
	}
}
