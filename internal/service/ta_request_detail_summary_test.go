package service

import "testing"

// UAT DEF-009: Detail embeds the list's summary but did not select its fields,
// so a late request came back as on time (and trimmed/dropped/can_cancel as
// zero values) from GET /ta-requests/:id while the list said otherwise.
func TestDetail_CarriesTheSummaryFieldsTheListHas(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.exec(`UPDATE ta_requests SET is_late = TRUE WHERE id = $1`, f.RequestID)
	f.exec(`UPDATE ta_request_assignments SET state = 'trimmed' WHERE id = $1`, f.AssignmentID)

	svc := &TARequestService{pool: f.Pool}
	d, err := svc.Detail(f.ctx, f.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsLate {
		t.Error("is_late = false, want true — the list already reports it late")
	}
	if d.TrimmedCount != 1 {
		t.Errorf("trimmed_count = %d, want 1", d.TrimmedCount)
	}
}
