package service

import (
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// A course TDBM had matched — and whose holiday it had auto-filled with a
// makeup — must still delete (03/10/2026). The TDBM row stays, unmatched.
func TestDelete_CourseWithATDBMFilledMakeup(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	makeup := uuid.New()
	f.exec(`INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, kind)
	        VALUES ($1, $2, '2026-07-01', '2026-07-04', 'lecture')`, makeup, f.SectionID)
	f.exec(`INSERT INTO tdbm_extra_teachings (extra_class_id, academic_year, semester,
	            teaching_course_id, section_id, applied_makeup_id)
	        VALUES (987654, 2569, 1, $1, $2, $3)`, f.CourseID, f.SectionID, makeup)

	if err := svc.Delete(f.ctx, f.StaffID, f.CourseID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var linked bool
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT teaching_course_id IS NOT NULL OR section_id IS NOT NULL OR applied_makeup_id IS NOT NULL
		  FROM tdbm_extra_teachings WHERE extra_class_id = 987654`).Scan(&linked); err != nil {
		t.Fatalf("TDBM row must survive the delete: %v", err)
	}
	if linked {
		t.Error("TDBM row still points at the deleted course")
	}
}
