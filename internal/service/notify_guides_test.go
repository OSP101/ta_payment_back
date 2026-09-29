package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every manual page an e-mail links to must exist in the frontend's manual.
// Runs when the frontend checkout sits beside this repo (the local layout);
// skips in a backend-only checkout such as CI.
func TestGuides_PointAtRealManualPages(t *testing.T) {
	pages := filepath.Join("..", "..", "..", "ta_payment_front", "content", "docs", "pages")
	if _, err := os.Stat(pages); err != nil {
		t.Skip("frontend checkout not found beside the backend")
	}
	slugRe := regexp.MustCompile(`slug:\s*"([^"]+)"`)
	slugs := map[string]map[string]bool{}
	for _, aud := range []string{"lecturer", "ta"} {
		src, err := os.ReadFile(filepath.Join(pages, aud+".ts"))
		if err != nil {
			t.Fatal(err)
		}
		slugs[aud] = map[string]bool{}
		for _, m := range slugRe.FindAllStringSubmatch(string(src), -1) {
			slugs[aud][m[1]] = true
		}
	}
	for _, set := range [][]MailGuide{lecturerRequestGuides, lecturerStartGuides, taAppointedGuides, taTimetableGuides} {
		for _, g := range set {
			parts := strings.SplitN(strings.TrimPrefix(g.Path, "/docs/"), "/", 2)
			if len(parts) == 1 {
				continue // the audience index, always present
			}
			if !slugs[parts[0]][parts[1]] {
				t.Errorf("%q links to %s, but the %s manual has no page %q", g.Label, g.Path, parts[0], parts[1])
			}
		}
	}
}

// The first-contact notices carry the getting-started guide.
func TestFirstContactNoticesLinkTheManual(t *testing.T) {
	w := noticeWindow{term: "ภาคการศึกษาที่ 2 ปีการศึกษา 2569", closesAt: time.Now().Add(72 * time.Hour)}
	c := []noticeCourse{{code: "CP1", name: "วิชา", sections: 1}}
	_, _, appointed := appointNoticeContent("CP1 วิชา", []string{"01"}, true, nil)
	_, _, timetable := timetableReminderContent("ภาคการศึกษาที่ 2 ปีการศึกษา 2569", []waitingCourse{{"CP1", "วิชา", ""}})
	cases := map[string]struct {
		got  []MailGuide
		want string
	}{
		"window open":        {openNoticeLayout(w, c).Guides, "/docs/lecturer/start"},
		"window closing":     {closingNoticeLayout(w, c).Guides, "/docs/lecturer/request/overview"},
		"TA appointed":       {appointed.Guides, "/docs/ta/start"},
		"timetable reminder": {timetable.Guides, "/docs/ta/schedule/overview"},
	}
	for name, c := range cases {
		found := false
		for _, g := range c.got {
			if g.Path == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: guides %+v lack %s", name, c.got, c.want)
		}
	}
}

func TestGuides_RenderAndGoAbsolute(t *testing.T) {
	guides := absoluteGuides("https://tas.coco.kku.ac.th", taAppointedGuides)
	if guides[0].Path != "https://tas.coco.kku.ac.th/docs/ta/start" {
		t.Errorf("path = %q", guides[0].Path)
	}
	if taAppointedGuides[0].Path != "/docs/ta/start" {
		t.Error("absoluteGuides modified the shared guide set")
	}
	m := mailContent{Title: "t", Body: "b", Recipient: "คุณก ข", Closing: closingInform,
		Layout: MailLayout{Guides: guides}}
	html := renderMailHTML(m)
	if !strings.Contains(html, "คู่มือการใช้งาน") || !strings.Contains(html, `href="https://tas.coco.kku.ac.th/docs/ta/start"`) {
		t.Error("guide box missing from the HTML")
	}
	if text := renderMailText(m); !strings.Contains(text, "เริ่มใช้งานครั้งแรก (ผู้ช่วยสอน): https://tas.coco.kku.ac.th/docs/ta/start") {
		t.Errorf("guide missing from the text part:\n%s", text)
	}
}
