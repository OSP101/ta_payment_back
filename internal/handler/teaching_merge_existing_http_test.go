package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

type mergeHTTPFixture struct {
	app   *fiber.App
	pool  *pgxpool.Pool
	svc   *service.Container
	actor uuid.UUID
	token string
}

func newMergeHTTPFixture(t *testing.T) *mergeHTTPFixture {
	t.Helper()
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{JWTSecret: "merge-integration-test-only"}
	aud := audit.New(pool)
	svc := service.NewContainer(pool, store, mail.New(cfg), aud, cfg, nil, nil)
	actor, session := uuid.New(), uuid.New()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO users(id,email,first_name,last_name,is_active,must_change_password) VALUES($1::uuid,$1::text||'@example.test','Merge','Tester',true,false)`,
		`INSERT INTO user_roles(user_id,role) VALUES($1,'staff'),($1,'lecturer')`,
	} {
		if _, err = pool.Exec(ctx, q, actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sessions(id,user_id,expires_at,last_activity_at) VALUES($1,$2,NOW()+INTERVAL '1 hour',NOW())`, session, actor); err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokenService(cfg.JWTSecret, "merge-test")
	token, err := tokens.Issue(actor, []string{rbac.RoleStaff}, session, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler, DisableStartupMessage: true})
	Mount(app, svc, tokens, rbac.New(pool), aud)
	return &mergeHTTPFixture{app: app, pool: pool, svc: svc, actor: actor, token: token}
}

func (f *mergeHTTPFixture) courses(t *testing.T, year int, extraRegular ...bool) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	term := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO academic_terms(id,academic_year,semester) VALUES($1,$2,1)`, term, year); err != nil {
		t.Fatal(err)
	}
	create := func(code string, sections []service.CourseSectionInput) uuid.UUID {
		id, err := f.svc.Teaching.Create(context.Background(), f.actor, service.CreateTeachingCourseInput{TermID: term, Code: code, NameTH: "HUMAN-COMPUTER INTERACTION", Credits: 3, LectureHrs: 3, SelfHrs: 6, LecturerIDs: []uuid.UUID{f.actor}, Sections: sections})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	targetSections := []service.CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 40}, {SecNo: "3", Track: "special", NumStudents: 30}}
	if len(extraRegular) > 0 && extraRegular[0] {
		targetSections = append(targetSections, service.CourseSectionInput{SecNo: "2", Track: "regular", NumStudents: 40})
	}
	target := create("CP321004", targetSections)
	source := create("SC361004", []service.CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 5}, {SecNo: "2", Track: "special", NumStudents: 5}})
	return term, target, source
}

func TestTeachingCourseListSnapshotCoversFilteredCoursesOnlyAsCollection(t *testing.T) {
	f := newMergeHTTPFixture(t)
	term, target, _ := f.courses(t, 2569)
	for _, tc := range []struct {
		path       string
		collection bool
	}{
		{"/teaching-courses?term_id=" + term.String(), true},
		{"/teaching-courses", true},
		{"/teaching-courses/" + target.String(), false},
	} {
		req := httptest.NewRequest("GET", "/api/v1"+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		res, err := f.app.Test(req, 10000)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: status %d", tc.path, res.StatusCode)
		}
		var views map[string]string
		if err := json.Unmarshal([]byte(res.Header.Get("X-Edit-Views")), &views); err != nil {
			t.Fatal(err)
		}
		if (views["course:*"] != "") != tc.collection {
			t.Fatalf("%s: collection=%t want=%t", tc.path, views["course:*"] != "", tc.collection)
		}
	}
}

func TestExistingMergeFullRouterWorkflow(t *testing.T) {
	f := newMergeHTTPFixture(t)
	term, target, source := f.courses(t, 2569)
	views := ""
	call := func(method, path, key string, body any, want int) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		if views != "" {
			req.Header.Set("X-Edit-Views", views)
		}
		res, err := f.app.Test(req, 10000)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, res.StatusCode, want, data)
		}
		if v := res.Header.Get("X-Edit-Views"); v != "" {
			views = v
		}
		return data
	}
	call("GET", "/teaching-courses?term_id="+term.String(), "", nil, 200)
	path := "/teaching-courses/" + target.String() + "/merge-existing"
	in := service.ExistingMergeInput{SourceID: source}
	decode := func(data []byte) service.ExistingMergePreview {
		var p service.ExistingMergePreview
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	key := uuid.NewString()
	var p service.ExistingMergePreview
	for i := 0; i < 3; i++ {
		p = decode(call("POST", path, key, in, 200))
	}
	if len(p.Blockers) == 0 {
		t.Fatal("missing explicit mappings were accepted")
	}
	for _, s := range p.Source.Sections {
		for _, dst := range p.Target.Sections {
			if s.Track == dst.Track {
				id := dst.ID
				in.Mappings = append(in.Mappings, service.ExistingMergeMapping{Source: s.ID, Target: &id})
			}
		}
	}
	key = uuid.NewString()
	p = decode(call("POST", path, key, in, 200))
	if len(p.Blockers) > 0 {
		t.Fatalf("complete preview blocked: %v", p.Blockers)
	}
	for i := 0; i < 3; i++ {
		next := decode(call("POST", path, key, in, 200))
		if next.Token != p.Token {
			t.Fatal("unchanged preview token changed")
		}
	}
	in.Token = p.Token
	in.Confirmed = true
	// Reuse the preview intent to prove it was never consumed by inspection.
	call("POST", path+"?commit=1", key, in, 200)
	call("POST", path+"?commit=1", key, in, 409)
	var regular, special, remaining int
	if err := f.pool.QueryRow(context.Background(), `SELECT num_students_regular,num_students_special FROM teaching_courses WHERE id=$1`, target).Scan(&regular, &special); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM teaching_courses WHERE id=$1`, source).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if regular != 45 || special != 35 || remaining != 0 {
		t.Fatalf("merge result: regular=%d special=%d source=%d", regular, special, remaining)
	}
}

// Reinspection must recover from stale or missing client edit snapshots.
func TestExistingMergeReinspectionRefreshesOnlyReviewedCourseSnapshots(t *testing.T) {
	for _, mode := range []string{"concurrent-target-edit", "missing-client-snapshot"} {
		t.Run(mode, func(t *testing.T) {
			f := newMergeHTTPFixture(t)
			term, target, source := f.courses(t, 2569)
			views := map[string]string{}
			call := func(method, path string, body any, want int) ([]byte, map[string]string) {
				t.Helper()
				raw, _ := json.Marshal(body)
				header, _ := json.Marshal(views)
				req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer "+f.token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Edit-Views", string(header))
				req.Header.Set("Idempotency-Key", uuid.NewString())
				res, err := f.app.Test(req, 10000)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				data, _ := io.ReadAll(res.Body)
				if res.StatusCode != want {
					t.Fatalf("%s: got %d want %d: %s", path, res.StatusCode, want, data)
				}
				updated := map[string]string{}
				_ = json.Unmarshal([]byte(res.Header.Get("X-Edit-Views")), &updated)
				for scope, token := range updated {
					views[scope] = token
				}
				return data, updated
			}
			call("GET", "/teaching-courses?term_id="+term.String(), nil, 200)
			path := "/teaching-courses/" + target.String() + "/merge-existing"
			in := service.ExistingMergeInput{SourceID: source}
			data, _ := call("POST", path, in, 200)
			var p service.ExistingMergePreview
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			for _, s := range p.Source.Sections {
				for _, dst := range p.Target.Sections {
					if s.Track == dst.Track {
						id := dst.ID
						in.Mappings = append(in.Mappings, service.ExistingMergeMapping{Source: s.ID, Target: &id})
					}
				}
			}
			data, _ = call("POST", path, in, 200)
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			in.Token = p.Token
			in.Confirmed = true
			if mode == "concurrent-target-edit" {
				if _, err := f.pool.Exec(context.Background(), `UPDATE teaching_courses SET name_th='Changed after inspection' WHERE id=$1`, target); err != nil {
					t.Fatal(err)
				}
				call("POST", path+"?commit=1", in, 409)
			} else {
				views = map[string]string{}
			}
			data, updated := call("POST", path, in, 200)
			if len(updated) != 2 || updated["course:"+target.String()] == "" || updated["course:"+source.String()] == "" {
				t.Fatalf("preview must refresh exactly the two reviewed courses, got scopes %v", updated)
			}
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			in.Token = p.Token
			call("POST", path+"?commit=1", in, 200)
		})
	}
}

// Opt-in browser/API integration against an isolated PostgreSQL database.
func TestExistingMergeBrowserIntegration(t *testing.T) {
	if os.Getenv("RUN_MERGE_BROWSER") != "1" {
		t.Skip("set RUN_MERGE_BROWSER=1 for browser/API integration")
	}
	f := newMergeHTTPFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.app.Listener(listener) }()
	t.Cleanup(func() { _ = f.app.Shutdown() })
	cases := []map[string]any{}
	for i, name := range []string{"desktop-chrome", "mobile-chrome", "iphone-safari"} {
		term, target, source := f.courses(t, 2569+i)
		cases = append(cases, map[string]any{"name": name, "term": term, "target": target, "source": source})
	}
	manifest := map[string]any{"api": fmt.Sprintf("http://%s", listener.Addr()), "token": f.token, "cases": cases}
	path := filepath.Join(t.TempDir(), "fixture.json")
	data, _ := json.Marshal(manifest)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	front, err := filepath.Abs("../../../ta_payment_front")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "scripts/merge-existing.integration.browser.test.mjs", path)
	cmd.Dir = front
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var n int
		if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM teaching_courses WHERE id=$1`, c["source"]).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s source still active: %d %v", c["name"], n, err)
		}
	}
}

// This suite enters the actual staff page in the optimized standalone build,
// including its layouts, SWR cache, term provider and dynamically loaded modal.
func TestExistingMergeProductionPageScenarios(t *testing.T) {
	if os.Getenv("RUN_MERGE_PRODUCTION") != "1" {
		t.Skip("set RUN_MERGE_PRODUCTION=1 for production-page scenario tests")
	}
	f := newMergeHTTPFixture(t)
	address := "127.0.0.1:0"
	if os.Getenv("MERGE_REUSE_PRODUCTION") == "1" {
		data, err := os.ReadFile("/tmp/merge-production-build-meta.json")
		if err != nil {
			t.Fatal(err)
		}
		var meta struct {
			API string `json:"api"`
		}
		if err = json.Unmarshal(data, &meta); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(meta.API)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
			t.Fatal("invalid cached test API address")
		}
		address = u.Host
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.app.Listener(listener) }()
	t.Cleanup(func() { _ = f.app.Shutdown() })
	cases := []map[string]any{}
	modes := []string{"desktop", "mobile", "safari", "target-changed", "source-changed", "two-tabs", "ta-request", "reverse-main", "incompatible", "exported", "approved-desktop", "approved-mobile", "approved-safari", "approved-two-tabs", "approved-target-changed", "approved-draft-time", "approved-submitted-time", "approved-reviewed-month", "approved-extra-section"}
	realFile := os.Getenv("MERGE_REAL_REGISTRAR_FILE")
	if realFile == "" {
		realFile = "../../../docs/รายวิชาที่เปิดสอนภาค 2-2569.xls"
	}
	if _, err := os.Stat(realFile); err == nil {
		modes = append(modes, "approved-real-term2-desktop", "approved-real-term2-mobile", "approved-real-term2-safari")
	}
	for i, mode := range modes {
		realTerm2 := strings.Contains(mode, "real-term2")
		var term, target, source uuid.UUID
		if realTerm2 {
			term, target, source = f.realTerm2Courses(t, 2569+i)
		} else {
			term, target, source = f.courses(t, 2569+i, mode == "approved-extra-section")
		}
		merged := i < 6 || (i >= 10 && i < 15) || mode == "approved-extra-section" || realTerm2
		approval := map[string]any{}
		if i >= 10 {
			ta, rid, aid, section := uuid.New(), uuid.New(), uuid.New(), uuid.Nil
			if err = f.pool.QueryRow(context.Background(), `SELECT id FROM sections WHERE teaching_course_id=$1 AND track='regular' AND sec_no::int=1 ORDER BY id LIMIT 1`, target).Scan(&section); err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO users(id,email,first_name,last_name,is_active) VALUES($1,$2,'TA','Tester',true)`, []any{ta, ta.String() + "@example.test"}},
				{`INSERT INTO user_roles(user_id,role) VALUES($1,'ta')`, []any{ta}},
				{`INSERT INTO ta_requests(id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at,decided_at,decided_by) VALUES($1,$2,$3,'both','approved',NOW(),NOW(),$3)`, []any{rid, target, f.actor}},
				{`INSERT INTO ta_request_counts(request_id,section_id,undergrad_count) VALUES($1,$2,1)`, []any{rid, section}},
				{`INSERT INTO ta_request_assignments(id,request_id,section_id,ta_id,level) VALUES($1,$2,$3,$4,'undergrad')`, []any{aid, rid, section, ta}},
				{`INSERT INTO ta_workload_forms(assignment_id,attendance_hrs,check_work_hrs) VALUES($1,1,1)`, []any{aid}},
				{`INSERT INTO course_budget_alerts(teaching_course_id) VALUES($1)`, []any{target}},
			} {
				if _, err = f.pool.Exec(context.Background(), step.sql, step.args...); err != nil {
					t.Fatal(err)
				}
			}
			approval["request_id"] = rid
			approval["assignment_id"] = aid
			if mode == "approved-draft-time" || mode == "approved-submitted-time" {
				status := "draft"
				if mode == "approved-submitted-time" {
					status = "submitted"
				}
				if _, err = f.pool.Exec(context.Background(), `INSERT INTO work_logs(assignment_id,work_date,start_time,end_time,hours,activity,status) VALUES($1,CURRENT_DATE,'09:00','10:00',1,'lecture',$2::worklog_status)`, aid, status); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "approved-reviewed-month" {
				pid := uuid.New()
				if _, err = f.pool.Exec(context.Background(), `INSERT INTO submission_periods(id,term_id,year_month,starts_on,due_date,label) VALUES($1,$2,'2569-06','2026-06-01','2026-07-31','test')`, pid, term); err != nil {
					t.Fatal(err)
				}
				if _, err = f.pool.Exec(context.Background(), `INSERT INTO submission_period_status(id,submission_period_id,ta_id,teaching_course_id,status,staff_reviewed_by) VALUES($1,$2,$3,$4,'staff_reviewed',$5)`, uuid.New(), pid, ta, target, f.actor); err != nil {
					t.Fatal(err)
				}
			}
			if realTerm2 {
				f.seedRealTerm2References(t, term, target, source, 2569+i, i)
			}
			var before string
			if err = f.pool.QueryRow(context.Background(), `SELECT COALESCE(jsonb_agg(jsonb_build_object('request',to_jsonb(r),'assignment',to_jsonb(a),'workload',to_jsonb(w)) ORDER BY r.id,a.id),'[]'::jsonb)::text FROM ta_requests r JOIN ta_request_assignments a ON a.request_id=r.id JOIN ta_workload_forms w ON w.assignment_id=a.id WHERE r.teaching_course_id=$1`, target).Scan(&before); err != nil {
				t.Fatal(err)
			}
			approval["before"] = before
		}
		switch mode {
		case "ta-request":
			_, err = f.pool.Exec(context.Background(), `INSERT INTO ta_requests(teaching_course_id,lecturer_id,reimburse_scope) VALUES($1,$2,'both')`, source, f.actor)
		case "incompatible":
			_, err = f.pool.Exec(context.Background(), `UPDATE teaching_courses SET credits=4 WHERE id=$1`, source)
		case "exported":
			_, err = f.pool.Exec(context.Background(), `UPDATE teaching_courses SET exported_at=NOW() WHERE id=$1`, source)
		}
		if err != nil {
			t.Fatal(err)
		}
		regularBefore, regularAfter := 40, 45
		if mode == "approved-extra-section" || realTerm2 {
			regularBefore, regularAfter = 80, 85
		}
		cases = append(cases, map[string]any{"mode": mode, "term": term, "target": target, "source": source, "merged": merged, "approval": approval, "regular_before": regularBefore, "regular_after": regularAfter})
	}
	manifest := map[string]any{"api": fmt.Sprintf("http://%s", listener.Addr()), "token": f.token, "cases": cases}
	path := filepath.Join(t.TempDir(), "production-fixture.json")
	data, _ := json.Marshal(manifest)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	front, err := filepath.Abs("../../../ta_payment_front")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "scripts/merge-existing.production.browser.test.mjs", path)
	cmd.Dir = front
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var count, regular, special int
		if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM teaching_courses WHERE id=$1`, c["source"]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if err = f.pool.QueryRow(context.Background(), `SELECT num_students_regular,num_students_special FROM teaching_courses WHERE id=$1`, c["target"]).Scan(&regular, &special); err != nil {
			t.Fatal(err)
		}
		if c["merged"].(bool) {
			if count != 0 || regular != c["regular_after"].(int) || special != 35 {
				t.Fatalf("%s: source=%d counts=%d/%d", c["mode"], count, regular, special)
			}
		} else if count != 1 || regular != c["regular_before"].(int) || special != 30 {
			t.Fatalf("blocked %s changed data: source=%d counts=%d/%d", c["mode"], count, regular, special)
		}
		if strings.Contains(c["mode"].(string), "real-term2") {
			var refs, drafts, requests, assignments, linkedSpecial int
			if err = f.pool.QueryRow(context.Background(), `SELECT
  (SELECT count(*) FROM tdbm_extra_teachings WHERE teaching_course_id=$1 AND section_id IS NOT NULL),
  (SELECT count(*) FROM ta_request_drafts WHERE teaching_course_id=$1),
  (SELECT count(*) FROM ta_requests WHERE teaching_course_id=$1 AND status='approved'),
  (SELECT count(*) FROM ta_request_assignments a JOIN ta_requests r ON r.id=a.request_id WHERE r.teaching_course_id=$1),
  (SELECT count(*) FROM tdbm_extra_teachings x JOIN sections sec ON sec.id=x.section_id WHERE x.teaching_course_id=$1 AND x.course_code='SC361004' AND x.section_label='กลุ่มที่ 2' AND sec.track='special')`, c["target"]).Scan(&refs, &drafts, &requests, &assignments, &linkedSpecial); err != nil {
				t.Fatal(err)
			}
			if refs != 32 || drafts != 1 || requests != 4 || assignments != 8 || linkedSpecial != 8 {
				t.Fatalf("%s lost references: tdbm=%d draft=%d requests=%d assignments=%d special=%d", c["mode"], refs, drafts, requests, assignments, linkedSpecial)
			}
		}
		approval := c["approval"].(map[string]any)
		if _, ok := approval["request_id"]; ok {
			var after string
			if err = f.pool.QueryRow(context.Background(), `SELECT COALESCE(jsonb_agg(jsonb_build_object('request',to_jsonb(r),'assignment',to_jsonb(a),'workload',to_jsonb(w)) ORDER BY r.id,a.id),'[]'::jsonb)::text FROM ta_requests r JOIN ta_request_assignments a ON a.request_id=r.id JOIN ta_workload_forms w ON w.assignment_id=a.id WHERE r.teaching_course_id=$1`, c["target"]).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != approval["before"].(string) {
				t.Fatalf("%s changed approval, assignment or workload", c["mode"])
			}
			var snapshots int
			if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM ta_request_calculation_snapshots WHERE request_id IN (SELECT id FROM ta_requests WHERE teaching_course_id=$1)`, c["target"]).Scan(&snapshots); err != nil {
				t.Fatal(err)
			}
			want := 0
			if c["merged"].(bool) {
				want = 2
				if strings.Contains(c["mode"].(string), "real-term2") {
					want = 8
				}
			}
			if snapshots != want {
				t.Fatalf("%s: snapshot count=%d want=%d", c["mode"], snapshots, want)
			}
		}
	}
}
