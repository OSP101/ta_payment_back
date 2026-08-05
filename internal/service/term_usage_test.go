package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// TermUsage counted a table named budget_allocations that has never existed in
// any migration. Nothing in Go caught it — the name lives inside a query string
// — so every "ลบภาคเรียน" attempt died on an undefined-table error and the
// officer only ever saw "ระบบขัดข้อง". Deleting a term was impossible.
//
// These tests run the real query against the real schema, which is the only
// thing that can catch a table that isn't there, and pin Blocking() to exactly
// the FKs that are ON DELETE NO ACTION — the set Postgres itself refuses on.

func newTermSvc(t *testing.T) (*TeachingService, context.Context, uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &TeachingService{pool: pool, aud: audit.New(pool)}

	term := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1, 2569, 2, FALSE)`,
		term); err != nil {
		t.Fatalf("insert term: %v", err)
	}
	return svc, ctx, term
}

func TestTermUsageQueriesRealTables(t *testing.T) {
	svc, ctx, term := newTermSvc(t)

	u, err := svc.TermUsage(ctx, term)
	if err != nil {
		t.Fatalf("TermUsage on an empty term must succeed: %v", err)
	}
	if u.TeachingCourses != 0 || u.ClassSchedules != 0 || u.Exports != 0 || u.RequestWindows != 0 {
		t.Fatalf("fresh term should have no references, got %+v", u)
	}
	if u.Blocking() {
		t.Fatal("a term with nothing attached must be deletable")
	}
}

// An empty term deletes; the same term with a NO ACTION reference does not, and
// reports why instead of erroring.
func TestDeleteTermFreeAndBlocked(t *testing.T) {
	svc, ctx, term := newTermSvc(t)
	actor := uuid.New()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'จนท','ทดสอบ',TRUE)`,
		actor, "staff-"+actor.String()+"@example.test"); err != nil {
		t.Fatalf("insert actor: %v", err)
	}

	if _, err := svc.DeleteTerm(ctx, actor, term); err != nil {
		t.Fatalf("an unreferenced term must delete: %v", err)
	}
	var n int
	if err := svc.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM academic_terms WHERE id=$1`, term).Scan(&n); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if n != 0 {
		t.Fatalf("term row survived the delete (%d rows)", n)
	}

	// Now a term that owns a course: Postgres would refuse the DELETE, so the
	// service has to refuse first and hand back the count that explains it.
	svc2, ctx2, term2 := newTermSvc(t)
	if _, err := svc2.pool.Exec(ctx2,
		`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
		 VALUES ($1,$2,'SC999999','วิชาทดสอบ','undergrad',3,3,0,6,30)`,
		uuid.New(), term2); err != nil {
		t.Fatalf("insert course: %v", err)
	}
	usage, err := svc2.DeleteTerm(ctx2, actor, term2)
	if err == nil {
		t.Fatal("a term with a teaching course must not delete")
	}
	if usage == nil || usage.TeachingCourses != 1 {
		t.Fatalf("the refusal must carry the blocking count, got %+v", usage)
	}
}
