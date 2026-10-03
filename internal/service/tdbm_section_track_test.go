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
