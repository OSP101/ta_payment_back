package service

import (
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// Track is a tie-breaker for TDBM section matching, not a gate (03/10/2026,
// CP423324): TDBM sent "กลุ่มที่ 2 / ภาคพิเศษ" for the course's only group 2,
// which we store as regular, and none of that group's makeups were matched.

func (f *fixture) addSectionNo(secNo, track string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO sections (id, teaching_course_id, sec_no, track)
	        VALUES ($1, $2, $3, $4::section_track)`, id, f.CourseID, secNo, track)
	return id
}

func (f *fixture) tdbmRow(id int, label, semType string) {
	var code string
	if err := f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&code); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO tdbm_extra_teachings (extra_class_id, academic_year, semester, course_code, section_label, semester_type)
	        VALUES ($1, $2, 1, $3, $4, $5)`, id, f.AcademicYear, code, label, semType)
}

func (f *fixture) tdbmSection(id int) *uuid.UUID {
	var sec *uuid.UUID
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT section_id FROM tdbm_extra_teachings WHERE extra_class_id = $1`, id).Scan(&sec); err != nil {
		f.t.Fatal(err)
	}
	return sec
}

func TestTDBMSectionMatch_TrackMismatchOnAUniqueGroupStillMatches(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	tdbm := &TDBMService{pool: f.Pool, aud: audit.New(f.Pool)}
	sec2 := f.addSectionNo("2", "regular")
	f.tdbmRow(910001, "กลุ่มที่ 2", "ภาคพิเศษ")

	tdbm.ResolveMatchesForTerm(f.ctx, f.AcademicYear, 1)

	if got := f.tdbmSection(910001); got == nil || *got != sec2 {
		t.Errorf("section = %v, want the course's only group 2 (%s) despite the track label", got, sec2)
	}
}

func TestTDBMSectionMatch_TrackStillPicksBetweenTwoSameNumberedGroups(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	tdbm := &TDBMService{pool: f.Pool, aud: audit.New(f.Pool)}
	regular := f.addSectionNo("3", "regular")
	special := f.addSectionNo("03", "special") // same number, as the matcher compares it
	f.tdbmRow(910002, "กลุ่มที่ 3", "ภาคพิเศษ")
	f.tdbmRow(910003, "กลุ่มที่ 3", "ภาคปกติ")

	tdbm.ResolveMatchesForTerm(f.ctx, f.AcademicYear, 1)

	if got := f.tdbmSection(910002); got == nil || *got != special {
		t.Errorf("ภาคพิเศษ row: section = %v, want the special group 3 (%s)", got, special)
	}
	if got := f.tdbmSection(910003); got == nil || *got != regular {
		t.Errorf("ภาคปกติ row: section = %v, want the regular group 3 (%s)", got, regular)
	}
}

// A merged course is filed in TDBM under each of its codes, so one sitting
// arrives as two rows on the one section we keep (04/10/2026: CP352201 +
// SC362201, CP321002 + SC361002). Counted twice, every pairing was skipped as
// a count mismatch and no makeup of a merged course was ever filed.
func TestTDBMAutoFill_MergedCodeTwinsCountOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	s := &TDBMService{pool: f.Pool, aud: audit.New(f.Pool)}
	mon := firstMonday() // the section meets Monday: lecture 09–12, lab 13–16
	f.exec(`INSERT INTO public_holidays (id, holiday_date, name_th, source)
	        VALUES (gen_random_uuid(), $1::date, 'วันหยุดทดสอบ', 'custom')`, mon)
	makeup := day(27)
	for i, code := range []string{"PRIMARY", "ALTCODE"} {
		for j, slot := range [][2]string{{"09:00", "12:00"}, {"13:00", "16:00"}} {
			f.exec(`INSERT INTO tdbm_extra_teachings
			          (extra_class_id, academic_year, semester, course_code, class_date, start_time, end_time,
			           opt_status, section_id, teaching_course_id)
			        VALUES ($1, $2, 1, $3, $4::date, $5::time, $6::time, 'D', $7, $8)`,
				920000+i*10+j, f.AcademicYear, code, makeup, slot[0], slot[1], f.SectionID, f.CourseID)
		}
	}

	filled, err := s.AutoFillMakeupSchedules(f.ctx, f.AcademicYear, 1)
	if err != nil {
		t.Fatalf("auto-fill: %v", err)
	}
	if filled != 2 {
		t.Fatalf("filled %d makeups, want 2 (lecture + lab) from the twin rows", filled)
	}
	var unapplied int
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT COUNT(*) FROM tdbm_extra_teachings
		WHERE extra_class_id BETWEEN 920000 AND 920099 AND applied_makeup_id IS NULL`).Scan(&unapplied); err != nil {
		t.Fatal(err)
	}
	if unapplied != 0 {
		t.Errorf("%d twin row(s) left unmatched; every copy of a filed sitting should point at its makeup", unapplied)
	}
}
