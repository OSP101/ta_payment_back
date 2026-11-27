package service

import "testing"

func TestTDBMReferences_OnlyProvenSameDocumentReferencesAreExcluded(t *testing.T) {
	for _, scenario := range []string{"original_and_moved", "zero_duration_superseded", "restored_original", "different_document", "standalone_original", "two_positive_moves"} {
		t.Run(scenario, func(t *testing.T) {
			original := "2026-11-23"
			start, end := "09:00:00", "12:00:00"
			base := tdbmExtraTeachingRow{ExtraClassID: 1, OptStatus: "D", TeachingID: tdbmPtr(100), ClassID: tdbmPtr(200), DocumentID: tdbmPtr(300), OriginalDate: &original, OriginalStart: &start, OriginalEnd: &end, ClassDate: original, StartTime: start, EndTime: end, SourceDuration: tdbmPtr(180)}
			moved := base
			moved.ExtraClassID = 2
			moved.ClassDate = "2026-11-28"
			rows := []tdbmExtraTeachingRow{base, moved}
			wantReference := true
			switch scenario {
			case "restored_original":
				rows[0].ClassDate = "2026-11-27"
				rows[0].SourceDuration = tdbmPtr(0)
				rows[1].ClassDate = original
			case "zero_duration_superseded":
				rows[0].ClassDate = "2026-11-27"
				rows[0].SourceDuration = tdbmPtr(0)
			case "different_document":
				rows[0].DocumentID = tdbmPtr(301)
				wantReference = false
			case "standalone_original":
				rows = rows[:1]
				wantReference = false
			case "two_positive_moves":
				rows[0].ClassDate = "2026-11-27"
				wantReference = false
			}
			normalized := tdbmNormalizeReferences(rows)
			if normalized[0].Reference != wantReference {
				t.Fatalf("reference=%v want=%v", normalized[0].Reference, wantReference)
			}
			if len(normalized) > 1 && normalized[1].Reference {
				t.Fatal("actual moved sitting excluded")
			}
			if rows[0].Reference {
				t.Fatal("normalization mutated the source slice")
			}
		})
	}
}

func TestTDBMReferences_SourceReferenceCannotCreateOrReviewAMakeup(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	moved := f.mariaEntry(995001, firstMonday(), day(27), "lecture")
	moved.DocumentID = tdbmPtr(700)
	moved.SourceDuration = tdbmPtr(180)
	reference := moved
	reference.ExtraClassID++
	reference.ClassDate = *reference.OriginalDate
	result := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{moved, reference})
	if result.Error != "" || result.Warning != "" || f.makeupCount() != 1 {
		t.Fatalf("reference normalization failed: %+v", result)
	}
	var isReference bool
	var linked *string
	if err := f.Pool.QueryRow(f.ctx, `SELECT source_reference,applied_makeup_id::text FROM tdbm_extra_teachings WHERE extra_class_id=$1`, reference.ExtraClassID).Scan(&isReference, &linked); err != nil || !isReference || linked != nil {
		t.Fatalf("reference still applied: %v %v %v", isReference, linked, err)
	}
	rows, err := s.ListExtraTeachings(f.ctx, f.AcademicYear, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ExtraClassID == reference.ExtraClassID && (!r.SourceReference || len(r.MatchOptions) > 0) {
			t.Fatal("reference still reviewable")
		}
	}
}
