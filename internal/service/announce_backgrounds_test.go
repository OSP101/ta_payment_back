package service

import (
	"errors"
	"testing"
)

// The cover maker draws on a 1600×900 canvas. A background that is smaller is
// blown up and blurs; one that is not 16:9 loses part of itself to the crop.
func TestCheckBackgroundSize(t *testing.T) {
	for _, c := range []struct {
		w, h int
		ok   bool
	}{
		{1600, 900, true},
		{1920, 1080, true},
		{1672, 941, true}, // the faculty's own template, 0.06% off 16:9
		{3840, 2160, true},
		{1280, 720, false}, // 16:9 but too small
		{1600, 1200, false},
		{2000, 900, false},
		{900, 1600, false},
	} {
		if err := CheckBackgroundSize(c.w, c.h); (err == nil) != c.ok {
			t.Errorf("%d×%d: err=%v, want ok=%v", c.w, c.h, err, c.ok)
		}
	}
}

func TestBackgrounds_AddListDelete(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("staff", "officer")

	if _, err := f.svc.AddBackground(f.ctx, actor, "  ", "announcements/x.png", 1600, 900, "dark"); err == nil {
		t.Error("a background with no name was accepted")
	}
	if _, err := f.svc.AddBackground(f.ctx, actor, "เล็กไป", "announcements/x.png", 800, 450, "dark"); err == nil {
		t.Error("an undersized background was accepted")
	}
	b, err := f.svc.AddBackground(f.ctx, actor, "พื้นหลังสีเข้ม", "announcements/2026/10/02/a.png", 1920, 1080, "light")
	if err != nil {
		t.Fatalf("AddBackground: %v", err)
	}
	if b.TextTone != "light" || b.URL != "/api/v1/announcements/images/announcements/2026/10/02/a.png" {
		t.Errorf("stored = %+v", b)
	}
	list, err := f.svc.ListBackgrounds(f.ctx)
	if err != nil || len(list) != 1 || list[0].Name != "พื้นหลังสีเข้ม" {
		t.Fatalf("ListBackgrounds = %+v, %v", list, err)
	}
	key, err := f.svc.DeleteBackground(f.ctx, actor, b.ID)
	if err != nil || key != "announcements/2026/10/02/a.png" {
		t.Fatalf("DeleteBackground = %q, %v", key, err)
	}
	if _, err := f.svc.DeleteBackground(f.ctx, actor, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrNotFound", err)
	}
}
