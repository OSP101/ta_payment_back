package service

import (
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"ta-payment-back/internal/audit"
	"testing"
)

// Reproduce the two courses from the registrar's actual 2/2569 workbook,
// including the passive TDBM matches and personal request draft seen live.
func realTerm2MergeFixture(t *testing.T) (*fixture, *TeachingService, uuid.UUID, uuid.UUID, ExistingMergeInput) {
	t.Helper()
	path := os.Getenv("MERGE_REAL_REGISTRAR_FILE")
	if path == "" {
		path = "../../../docs/รายวิชาที่เปิดสอนภาค 2-2569.xls"
	}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("real registrar workbook not available")
	}
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fixtureOpts{NoRequest: true})
	s := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	f.exec(`UPDATE academic_terms SET academic_year=2569,semester=2 WHERE id=$1`, f.TermID)
	preview, err := s.PreviewREGReview(f.ctx, f.StaffID, f.TermID, "term2.xls", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	review := preview.Review
	for i := range review.Courses {
		c := &review.Courses[i]
		if c.Code != "CP321004" && c.Code != "SC361004" {
			c.Skip = true
			c.SkipReason = "Only reproduce reported pair"
		} else {
			c.LecturerIDs = []uuid.UUID{f.LecturerID}
		}
	}
	preview, err = s.PreviewREGReview(f.ctx, f.StaffID, f.TermID, "term2.xls", body, review)
	if err != nil {
		t.Fatal(err)
	}
	review = preview.Review
	for _, issue := range review.Issues {
		if issue.Severity == "error" {
			t.Fatalf("import review: %+v", issue)
		}
		review.Acknowledged = append(review.Acknowledged, issue.ID)
	}
	preview, err = s.PreviewREGReview(f.ctx, f.StaffID, f.TermID, "term2.xls", body, review)
	if err != nil {
		t.Fatal(err)
	}
	review = preview.Review
	review.Confirmed = true
	result, err := s.CommitREGReview(f.ctx, f.StaffID, f.TermID, "term2.xls", body, review)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedIDs) != 2 {
		t.Fatalf("import: %+v", result)
	}
	var target, source uuid.UUID
	for code, dst := range map[string]*uuid.UUID{"CP321004": &target, "SC361004": &source} {
		if err = f.Pool.QueryRow(f.ctx, `SELECT id FROM teaching_courses WHERE term_id=$1 AND code=$2`, f.TermID, code).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$3,true),($2,$3,true) ON CONFLICT DO NOTHING`, target, source, f.LecturerID)
	for i := 0; i < 4; i++ {
		f.CourseID = target
		f.RequestID = f.insertRequest(fixtureOpts{RequestStatus: "approved", ReimburseScope: "both"})
		for _, secNo := range []string{"1", "2"} {
			if err = f.Pool.QueryRow(f.ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 AND sec_no::int=$2::int`, target, secNo).Scan(&f.SectionID); err != nil {
				t.Fatal(err)
			}
			f.AssignmentID = f.insertAssignment(fixtureOpts{Level: "undergrad", Workload: workloadHours{Attendance: .5, Lab: 1, CheckWork: 1}})
			f.exec(`INSERT INTO ta_request_counts(request_id,section_id,undergrad_count) VALUES($1,$2,1)`, f.RequestID, f.SectionID)
		}
	}
	f.exec(`INSERT INTO ta_request_drafts(user_id,teaching_course_id,payload) VALUES($1,$2,'{"v":1,"scope":"both","assignments":[]}'::jsonb)`, f.LecturerID, target)
	data, err := os.ReadFile("testdata/merge-term2-tdbm.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []map[string]any
	if err = json.Unmarshal(data, &refs); err != nil {
		t.Fatal(err)
	}
	for i, row := range refs {
		tc := target
		if row["course_code"] == "SC361004" {
			tc = source
		}
		var sec uuid.UUID
		if err = f.Pool.QueryRow(f.ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 AND sec_no::int=$2::int`, tc, row["section_no"]).Scan(&sec); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(row)
		f.exec(`INSERT INTO tdbm_extra_teachings(extra_class_id,academic_year,semester,course_code,teaching_course_id,section_id,section_label,semester_type,opt_status,class_date,start_time,end_time,duration_minutes) SELECT $1,$2,2,r.course_code,$3,$4,r.section_label,r.semester_type,r.opt_status,r.class_date,r.start_time,r.end_time,r.duration_minutes FROM jsonb_to_record($5::jsonb) AS r(course_code text,section_label text,semester_type text,opt_status text,class_date date,start_time time,end_time time,duration_minutes int)`, 980000+i, 2569, tc, sec, raw)
	}
	in := ExistingMergeInput{SourceID: source}
	p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range p.Source.Sections {
		want := "1"
		if src.Track == "special" {
			want = "3"
		}
		for _, dst := range p.Target.Sections {
			if dst.SecNo == want || dst.SecNo == "0"+want {
				id := dst.ID
				in.Mappings = append(in.Mappings, ExistingMergeMapping{Source: src.ID, Target: &id})
			}
		}
	}
	return f, s, target, source, in
}

func TestExistingCourseMergeRealTerm2WithTDBMAndApprovedRequests(t *testing.T) {
	f, s, target, source, in := realTerm2MergeFixture(t)
	before := mergeApprovalGraph(t, f)
	importedBefore := mergeImportedEvidence(t, f, target)
	p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil || len(p.Blockers) > 0 {
		t.Fatalf("real CP321004/SC361004 preview: blockers=%v error=%v", p.Blockers, err)
	}
	if len(p.ApprovedRequestIDs) != 4 {
		t.Fatalf("approved requests=%d", len(p.ApprovedRequestIDs))
	}
	in.Token = p.Token
	in.Confirmed = true
	if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err != nil {
		t.Fatal(err)
	}
	if mergeApprovalGraph(t, f) != before {
		t.Fatal("approval graph changed")
	}
	var regular, special, remaining, tdbm, drafts int
	f.Pool.QueryRow(f.ctx, `SELECT num_students_regular,num_students_special FROM teaching_courses WHERE id=$1`, target).Scan(&regular, &special)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=$1`, source).Scan(&remaining)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings WHERE teaching_course_id=$1 AND section_id IS NOT NULL`, target).Scan(&tdbm)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_request_drafts WHERE teaching_course_id=$1`, target).Scan(&drafts)
	if mergeImportedEvidence(t, f, target) != importedBefore {
		t.Fatal("TDBM source facts or retained request draft changed")
	}
	if regular != 85 || special != 35 || remaining != 0 || tdbm != 32 || drafts != 1 {
		t.Fatalf("counts=%d/%d source=%d tdbm=%d drafts=%d", regular, special, remaining, tdbm, drafts)
	}
	// The next sync must respect SC section 2 -> CP special section 3, even
	// though CP has another regular section numbered 2.
	tdbmSvc := &TDBMService{pool: f.Pool}
	if _, err = tdbmSvc.resolveSectionMatches(f.ctx, 2569, 2); err != nil {
		t.Fatal(err)
	}
	var matched int
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings t JOIN sections sec ON sec.id=t.section_id WHERE t.course_code='SC361004' AND t.section_label='กลุ่มที่ 2' AND sec.track='special' AND sec.sec_no::int=3`).Scan(&matched); err != nil || matched != 8 {
		t.Fatalf("rematched special=%d error=%v", matched, err)
	}
}

func TestExistingCourseMergePassiveImportReferencesAndDraft(t *testing.T) {
	f, s, target, source, in := approvedMergeFixture(t)
	var sourceSection uuid.UUID
	for _, m := range in.Mappings {
		if m.Target == nil {
			sourceSection = m.Source
		}
	}
	f.exec(`INSERT INTO tdbm_extra_teachings(extra_class_id,academic_year,semester,course_code,teaching_course_id,section_id,section_label,semester_type) VALUES(989001,2569,1,'SC999001',$1,$2,'กลุ่มที่ 2','ภาคพิเศษ')`, source, sourceSection)
	f.exec(`INSERT INTO ta_request_drafts(user_id,teaching_course_id,payload) VALUES($1,$2,'{"v":1}')`, f.LecturerID, target)
	p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil || len(p.Blockers) > 0 {
		t.Fatalf("preview: %v %v", p.Blockers, err)
	}
	in.Token = p.Token
	in.Confirmed = true
	if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err != nil {
		t.Fatal(err)
	}
	var tc, sec uuid.UUID
	if err = f.Pool.QueryRow(f.ctx, `SELECT teaching_course_id,section_id FROM tdbm_extra_teachings WHERE extra_class_id=989001`).Scan(&tc, &sec); err != nil || tc != target || sec != sourceSection {
		t.Fatalf("lost kept TDBM match: %s %s %v", tc, sec, err)
	}
	tdbm := &TDBMService{pool: f.Pool}
	if _, err = tdbm.resolveSectionMatches(f.ctx, 2569, 1); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(f.ctx, `SELECT section_id FROM tdbm_extra_teachings WHERE extra_class_id=989001`).Scan(&sec); err != nil || sec != sourceSection {
		t.Fatalf("kept match after sync: %s %v", sec, err)
	}
}

func TestExistingCourseMergeRechecksPassiveReferences(t *testing.T) {
	for _, kind := range []string{"tdbm", "draft"} {
		t.Run(kind, func(t *testing.T) {
			f, s, target, source, in := approvedMergeFixture(t)
			f.exec(`INSERT INTO tdbm_extra_teachings(extra_class_id,academic_year,semester,teaching_course_id) VALUES(989001,2569,1,$1)`, source)
			f.exec(`INSERT INTO ta_request_drafts(user_id,teaching_course_id,payload) VALUES($1,$2,'{"v":1}')`, f.LecturerID, target)
			p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
			if err != nil || len(p.Blockers) > 0 {
				t.Fatalf("preview: %v %v", p.Blockers, err)
			}
			if kind == "tdbm" {
				f.exec(`UPDATE tdbm_extra_teachings SET opt_status='D' WHERE extra_class_id=989001`)
			} else {
				f.exec(`UPDATE ta_request_drafts SET payload='{"v":2}' WHERE teaching_course_id=$1`, target)
			}
			in.Token = p.Token
			in.Confirmed = true
			if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err == nil {
				t.Fatal("stale passive reference committed")
			}
		})
	}
}

func TestExistingCourseMergeAppliedMakeupStillBlocked(t *testing.T) {
	f, s, target, source, in := approvedMergeFixture(t)
	var section uuid.UUID
	if err := f.Pool.QueryRow(f.ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 LIMIT 1`, source).Scan(&section); err != nil {
		t.Fatal(err)
	}
	makeup := uuid.New()
	f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,makeup_date,kind) VALUES($1,$2,'2026-07-01','2026-07-04','lecture')`, makeup, section)
	f.exec(`INSERT INTO tdbm_extra_teachings(extra_class_id,academic_year,semester,teaching_course_id,section_id,applied_makeup_id) VALUES(989001,2569,1,$1,$2,$3)`, source, section, makeup)
	p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil || len(p.Blockers) == 0 {
		t.Fatalf("applied makeup not blocked: %v %v", p.Blockers, err)
	}
}

func mergeImportedEvidence(t *testing.T, f *fixture, target uuid.UUID) string {
	t.Helper()
	var raw string
	if err := f.Pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
 'tdbm',(SELECT jsonb_agg(to_jsonb(t)-'teaching_course_id'-'section_id' ORDER BY extra_class_id) FROM tdbm_extra_teachings t WHERE extra_class_id BETWEEN 980000 AND 980031),
 'drafts',(SELECT jsonb_agg(to_jsonb(d) ORDER BY user_id) FROM ta_request_drafts d WHERE teaching_course_id=$1))::text`, target).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
