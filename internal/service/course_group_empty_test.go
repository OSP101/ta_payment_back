package service

import (
	"encoding/json"
	"testing"
)

// A term with no duplicate course codes is the ORDINARY case, and its answer
// crosses the wire as JSON. A nil slice marshals to `null`, and the staff
// download button reaches straight for `.length` on the result — so returning
// nil here broke the export for exactly the terms that had nothing wrong with
// them ("Cannot read properties of null").
func TestDetectCourseGroups_EmptyAnswerMarshalsAsAList(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &TeachingService{pool: f.Pool, aud: f.Svc.aud}

	got, err := svc.DetectCourseGroups(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the fixture term should have no duplicate codes, got %d", len(got))
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Errorf("marshals to %s, want [] — `null` crashes every caller that "+
			"asks the result for its length", b)
	}
}
