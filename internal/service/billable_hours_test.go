package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/testutil"
)

// The sitting rule is implemented twice — once in SQL for the aggregates the
// screens read, once in Go for the rows the claim form prints. That is only safe
// while the two agree, so this file feeds identical data to both and fails on
// any disagreement. Every case below is a shape the co-taught data actually
// takes.

type sittingCase struct {
	name string
	// logs are (section, start, end, activity) on one date.
	logs [][4]string
	want float64
	why  string
}

var sittingCases = []sittingCase{
	{
		name: "two sections, same sitting",
		logs: [][4]string{
			{"1", "13:00", "15:00", "lab"},
			{"2", "13:00", "15:00", "lab"},
		},
		want: 2,
		why:  "one room, one sitting, two sections — paid once",
	},
	{
		name: "two sections back to back",
		logs: [][4]string{
			{"1", "13:00", "15:00", "lab"},
			{"2", "15:00", "17:00", "lab"},
		},
		want: 4,
		why:  "contiguous halves of one four-hour sitting",
	},
	{
		name: "chained overlap",
		logs: [][4]string{
			{"1", "13:00", "15:00", "lab"},
			{"2", "14:00", "16:00", "lab"},
			{"3", "15:00", "17:00", "lab"},
		},
		want: 4,
		why:  "13-17 once, not three separate two-hour blocks",
	},
	{
		name: "separate sittings on one day",
		logs: [][4]string{
			{"1", "09:00", "11:00", "lab"},
			{"1", "13:00", "15:00", "lab"},
		},
		want: 4,
		why:  "a gap means two sittings",
	},
	{
		name: "same hour, different activity",
		logs: [][4]string{
			{"1", "13:00", "15:00", "lab"},
			{"1", "13:00", "15:00", "review"},
		},
		want: 4,
		why:  "lab duty and grading bill separately even at the same hour",
	},
	{
		name: "one section only",
		logs: [][4]string{{"1", "13:00", "17:00", "lab"}},
		want: 4,
		why:  "nothing to merge",
	},
	{
		name: "identical duplicate rows",
		logs: [][4]string{
			{"1", "13:00", "15:00", "lecture"},
			{"1", "13:00", "15:00", "lecture"},
		},
		want: 2,
		why:  "a duplicated log must not double the claim",
	},
	{
		name: "nested interval",
		logs: [][4]string{
			{"1", "13:00", "17:00", "lab"},
			{"2", "14:00", "15:00", "lab"},
		},
		want: 4,
		why:  "an interval inside another adds nothing",
	},
}

// goHoursFor runs the Go merger over one case.
func goHoursFor(c sittingCase) float64 {
	day := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	var rows []claimLogRow
	for _, l := range c.logs {
		rows = append(rows, claimLogRow{
			SecNo: l[0], Track: "regular", Date: day,
			StartMin: hhmmToMin(l[1]), EndMin: hhmmToMin(l[2]), Activity: l[3],
		})
	}
	return billableHoursGo(rows)
}

func hhmmToMin(s string) int {
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	return h*60 + m
}

func TestBillableHours_GoMergerFollowsTheSittingRule(t *testing.T) {
	for _, c := range sittingCases {
		if got := goHoursFor(c); math.Abs(got-c.want) > 0.001 {
			t.Errorf("%s: Go merger = %v, want %v — %s", c.name, got, c.want, c.why)
		}
	}
}

// The one that matters: the SQL the screens read must produce exactly what the
// Go code prints on the form. Two implementations of one rule are a liability
// unless something forces them to agree.
func TestBillableHours_SQLAgreesWithTheGoMerger(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	for _, c := range sittingCases {
		t.Run(c.name, func(t *testing.T) {
			courseID := seedSittingCase(t, pool, ctx, c)

			svc := &ExportService{pool: pool}
			byTrack, err := svc.billableHoursByTATrack(ctx, courseID, nil)
			if err != nil {
				t.Fatal(err)
			}
			var sql float64
			for _, v := range byTrack {
				sql += v
			}
			goSide := goHoursFor(c)

			if math.Abs(sql-c.want) > 0.001 {
				t.Errorf("SQL = %v, want %v — %s", sql, c.want, c.why)
			}
			if math.Abs(sql-goSide) > 0.001 {
				t.Errorf("SQL says %v, the printed form says %v — the screens and the "+
					"claim would disagree again", sql, goSide)
			}
		})
	}
}

var seedTermSeq int

// seedSittingCase writes one case's logs into a fresh course and returns its id.
// Sections are created on demand so a case naming "1", "2", "3" gets three of
// them, all on one assignment set for one TA.
func seedSittingCase(t *testing.T, pool *pgxpool.Pool, ctx context.Context, c sittingCase) uuid.UUID {
	t.Helper()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, sql)
		}
	}
	// academic_terms is unique on (year, semester), so every case gets its own
	// year rather than fighting over one.
	seedTermSeq++
	term, lec, ta := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active)
	      VALUES ($1, $2, 1, FALSE)`, term, 2400+seedTermSeq)
	exec(`INSERT INTO users (id,email,first_name,last_name,is_active)
	      VALUES ($1,$2,'อาจารย์','ทดสอบ',TRUE)`, lec, "lec-"+lec.String()+"@example.test")
	exec(`INSERT INTO users (id,email,first_name,last_name,study_level,is_active)
	      VALUES ($1,$2,'ผู้ช่วย','ทดสอบ','undergrad',TRUE)`, ta, "ta-"+ta.String()+"@example.test")

	tc, req := uuid.New(), uuid.New()
	exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	      VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',3,2,2,5,40)`, tc, term, "CP"+tc.String()[:6])
	exec(`INSERT INTO teaching_lecturers (teaching_course_id,lecturer_id,is_primary) VALUES ($1,$2,TRUE)`, tc, lec)
	exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
	      VALUES ($1,$2,$3,'both','approved',NOW())`, req, tc, lec)

	assignOf := map[string]uuid.UUID{}
	for _, l := range c.logs {
		if _, ok := assignOf[l[0]]; ok {
			continue
		}
		sec, asg := uuid.New(), uuid.New()
		exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,$3,'regular')`, sec, tc, l[0])
		exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level)
		      VALUES ($1,$2,$3,$4,'undergrad')`, asg, req, sec, ta)
		assignOf[l[0]] = asg
	}
	for _, l := range c.logs {
		exec(`INSERT INTO work_logs (id,assignment_id,work_date,start_time,end_time,hours,activity,status)
		      VALUES (gen_random_uuid(),$1,'2026-06-15',$2::time,$3::time,
		              EXTRACT(EPOCH FROM ($3::time - $2::time))/3600,$4,'approved')`,
			assignOf[l[0]], l[1], l[2], l[3])
	}
	return tc
}
