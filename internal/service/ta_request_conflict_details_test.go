package service

import (
	"github.com/google/uuid"
	"testing"
)

func TestConflictPairs(t *testing.T) {
	requested := ConflictScheduleSlot{Source: "requested", CourseID: "new", Kind: "lab", Day: 1, Start: "09:00", End: "12:00"}
	for _, tt := range []struct {
		name, source, course, kind, start, end string
		day                                    int
		want                                   int
		blocking                               bool
	}{
		{"other lab", "teaching", "other", "lab", "10:00", "13:00", 1, 1, true},
		{"other lecture", "teaching", "other", "lecture", "10:00", "13:00", 1, 1, true},
		{"own class", "own_class", "", "own_class", "10:00", "13:00", 1, 1, false},
		{"same course", "teaching", "new", "lab", "10:00", "13:00", 1, 0, false},
		{"touching", "teaching", "other", "lab", "12:00", "13:00", 1, 0, false},
		{"other day", "teaching", "other", "lab", "10:00", "13:00", 2, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := conflictPairs([]ConflictScheduleSlot{requested, {Source: tt.source, CourseID: tt.course, Kind: tt.kind, Start: tt.start, End: tt.end, Day: tt.day}})
			if len(got) != tt.want {
				t.Fatalf("pairs=%+v", got)
			}
			if len(got) > 0 && (got[0].Blocking != tt.blocking || got[0].OverlapStart != "10:00" || got[0].OverlapEnd != "12:00") {
				t.Fatalf("wrong overlap: %+v", got[0])
			}
		})
	}
	requested.Kind = "lecture"
	if got := conflictPairs([]ConflictScheduleSlot{requested, {Source: "teaching", CourseID: "other", Kind: "lecture", Day: 1, Start: "09:00", End: "12:00"}}); len(got) != 1 || !got[0].Blocking {
		t.Fatalf("lecture/lecture must block: %+v", got)
	}
}

func TestPreviewConflictScheduleTermAndStatus(t *testing.T) {
	f := newCapFixture(t)
	sec := f.addSection("1", [3]string{"lab", "09:00", "12:00"})
	f.ownClassAt(1, "10:00", "11:00")
	var term uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT term_id FROM teaching_courses WHERE id=$1`, f.tc).Scan(&term); err != nil {
		t.Fatal(err)
	}
	otherTerm := uuid.New()
	f.exec(`INSERT INTO academic_terms (id,academic_year,semester,is_active) VALUES ($1,$2,2,FALSE)`, otherTerm, capTermSeq)
	for _, testCase := range []struct {
		status string
		term   uuid.UUID
	}{{"approved", term}, {"rejected", term}, {"cancelled", term}, {"submitted", term}, {"approved", otherTerm}} {
		status := testCase.status
		course, section, request := uuid.New(), uuid.New(), uuid.New()
		f.exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students) VALUES ($1,$2,$3,'วิชาอื่น','undergrad',3,1,1,5,40)`, course, testCase.term, status)
		f.exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,'2','regular')`, section, course)
		f.exec(`INSERT INTO section_schedules (section_id,kind,day_of_week,start_time,end_time) VALUES ($1,'lab',1,'10:00','13:00')`, section)
		f.exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at) VALUES ($1,$2,$3,'both',$4::ta_request_status,NOW())`, request, course, f.lec, status)
		f.exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level) VALUES (gen_random_uuid(),$1,$2,$3,'undergrad')`, request, section, f.ta)
	}
	got, err := f.svc.PreviewConflictSchedule(f.ctx, f.ta, f.tc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasSchedule || len(got.Slots) != 3 || len(got.Conflicts) != 2 {
		t.Fatalf("details=%+v", got)
	}
	for _, pair := range got.Conflicts {
		if pair.Requested.SectionID != sec.String() {
			t.Fatalf("wrong section: %+v", pair)
		}
		if pair.Blocking && pair.Existing.CourseCode != "approved" {
			t.Fatalf("inactive request blocked: %+v", pair)
		}
	}
}
