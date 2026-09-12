package service

import (
	"testing"

	"github.com/google/uuid"
)

// ListByCourse backs GET /teaching-courses/:tcId/submission-timeline — the
// "สถานะเบิกจ่ายรายเดือน" panel on the course overview page. It broke with
// "subquery uses ungrouped column trm.academic_year from outer query"
// (SQLSTATE 42803): two correlated subqueries in its SELECT list read
// trm.academic_year (via workLogInPeriodSQL) but the query's own GROUP BY
// never listed it — a query-planning error Postgres raises regardless of how
// many rows would have matched. This test just needs the call to not error;
// it isn't about the fixture having submission_periods data.
func TestSubmissionPeriodListByCourse_DoesNotErrorOnValidQuery(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, err := f.Periods.ListByCourse(f.ctx, f.StaffID, f.CourseID, uuid.Nil)
	if err != nil {
		t.Fatalf("ListByCourse: %v", err)
	}
}
