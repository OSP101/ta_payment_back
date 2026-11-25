package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func workloadCheck(t *testing.T, checks []DecisionCheck, passed bool, total string) {
	t.Helper()
	for _, c := range checks {
		if c.Rule == "workload" {
			if c.Passed != passed || !strings.Contains(c.Message, total) {
				t.Fatalf("workload check = %+v, want passed=%v total=%s", c, passed, total)
			}
			return
		}
	}
	t.Fatal("missing workload check")
}

func TestWorkloadPolicy_NewGraduateBoundaries(t *testing.T) {
	for _, level := range []string{"master", "phd"} {
		for _, total := range []float64{0, 0.5, 3, 9, 10, 12, 12.5, 13} {
			t.Run(fmt.Sprintf("%s/%.1f", level, total), func(t *testing.T) {
				rf := newRequestFixture(t, fixtureOpts{Level: level})
				rf.exec(`UPDATE users SET study_level=$2::study_level WHERE id=$1`, rf.TAID, level)
				rf.addTAClass(0, "07:00", "08:00")
				second := rf.addSection("02", 1)
				w := WorkloadInput{HelpTeachHrs: total / 2}
				res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.inputFor([]SectionWorkload{
					{SectionID: rf.SectionID, Workload: w}, {SectionID: second, Workload: w},
				}, level))
				if err != nil {
					t.Fatal(err)
				}
				workloadCheck(t, res.Checks, total <= 12, fmt.Sprintf("%.2f", total))
			})
		}
	}
}

func TestWorkloadPolicy_LegacyRecheckKeepsOriginalRule(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{Level: "master"})
	rf.exec(`UPDATE users SET study_level='master' WHERE id=$1`, rf.TAID)
	rf.addTAClass(0, "07:00", "08:00")
	second := rf.addSection("02", 1)
	w := WorkloadInput{HelpTeachHrs: 6, PrepHrs: 3, GradeHrs: 2}
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: w}, {SectionID: second, Workload: w},
	}, "master"))
	if err != nil {
		t.Fatal(err)
	}
	workloadCheck(t, res.Checks, false, "22.00")
	// Model a pre-upgrade row (migration default is 1). Recheck must retain 11.
	rf.exec(`UPDATE ta_requests SET workload_rule_version=1 WHERE id=$1`, res.ID)
	tx, err := rf.Pool.Begin(rf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(rf.ctx)
	checks, _, err := rf.Req.autoDecide(rf.ctx, tx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	workloadCheck(t, checks, true, "11.00")
}

func TestWorkloadPolicy_UndergradRawInputsAreImmutable(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.addTAClass(0, "07:00", "08:00")
	second := rf.addSection("02", 1)
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.inputFor([]SectionWorkload{
		{SectionID: rf.SectionID, Workload: WorkloadInput{AttendanceHrs: 2, UGOtherDesc: "original"}},
		{SectionID: second, Workload: WorkloadInput{AttendanceHrs: 3}},
	}, "undergrad"))
	if err != nil {
		t.Fatal(err)
	}
	workloadCheck(t, res.Checks, true, "5.00")
	var snapshotID, assignmentID uuid.UUID
	var original []byte
	if err := rf.Pool.QueryRow(rf.ctx, `SELECT id, inputs FROM ta_request_calculation_snapshots WHERE request_id=$1 AND event='submitted'`, res.ID).Scan(&snapshotID, &original); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version  int `json:"workload_rule_version"`
		Sections []struct {
			Workload  WorkloadInput     `json:"workload"`
			Schedules []json.RawMessage `json:"schedules"`
		} `json:"sections"`
		Rates  json.RawMessage            `json:"pay_rates"`
		Limits map[string]json.RawMessage `json:"eligibility_limits"`
	}
	if err := json.Unmarshal(original, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 2 || len(doc.Sections) != 2 || len(doc.Sections[0].Schedules) == 0 || len(doc.Rates) == 0 {
		t.Fatalf("incomplete source snapshot: %s", original)
	}
	if string(doc.Limits["graduate_min"]) != "null" || string(doc.Limits["graduate_max"]) != "12" {
		t.Fatalf("wrong graduate thresholds: %+v", doc.Limits)
	}
	total := doc.Sections[0].Workload.AttendanceHrs + doc.Sections[1].Workload.AttendanceHrs
	if total != 5 {
		t.Fatalf("raw hours lost: %v", total)
	}
	if err := rf.Pool.QueryRow(rf.ctx, `SELECT id FROM ta_request_assignments WHERE request_id=$1 AND section_id=$2`, res.ID, rf.SectionID).Scan(&assignmentID); err != nil {
		t.Fatal(err)
	}
	if err := rf.Req.UpdateAssignmentWorkload(rf.ctx, rf.LecturerID, assignmentID, WorkloadInput{AttendanceHrs: 1}); err != nil {
		t.Fatal(err)
	}
	rf.exec(`UPDATE section_schedules SET start_time='08:00' WHERE section_id=$1`, rf.SectionID)
	var retained []byte
	if err := rf.Pool.QueryRow(rf.ctx, `SELECT inputs FROM ta_request_calculation_snapshots WHERE id=$1`, snapshotID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(original) {
		t.Fatal("source evidence changed after workload/timetable edit")
	}
	var count int
	if err := rf.Pool.QueryRow(rf.ctx, `SELECT count(*) FROM ta_request_calculation_snapshots WHERE request_id=$1`, res.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("want submission, decision, before/after edit; got %d", count)
	}
	for _, sql := range []string{`UPDATE ta_request_calculation_snapshots SET inputs='{}' WHERE id=$1`, `DELETE FROM ta_request_calculation_snapshots WHERE id=$1`} {
		if _, err := rf.Pool.Exec(rf.ctx, sql, snapshotID); err == nil {
			t.Fatal("snapshot tampering was allowed")
		}
	}
}

func TestWorkloadPolicy_AddSectionsUsesSavedVersion(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			rf := newRequestFixture(t, fixtureOpts{Level: "master"})
			rf.exec(`UPDATE users SET study_level='master' WHERE id=$1`, rf.TAID)
			rf.addTAClass(0, "07:00", "08:00")
			res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.inputFor([]SectionWorkload{
				{SectionID: rf.SectionID, Workload: WorkloadInput{HelpTeachHrs: 6, PrepHrs: 3, GradeHrs: 2}},
			}, "master"))
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != "approved" {
				t.Fatalf("fixture status: %s", res.Status)
			}
			rf.exec(`UPDATE ta_requests SET workload_rule_version=$2 WHERE id=$1`, res.ID, version)
			second := rf.addSection("02", 1)
			_, err = rf.addSecs(res.ID, SectionWorkload{SectionID: second, Workload: WorkloadInput{GradeHrs: 2}})
			if version == 2 {
				if err == nil || !strings.Contains(err.Error(), "13.00") {
					t.Fatalf("new total must enforce 13h ceiling: %v", err)
				}
				if rf.countAssignments(res.ID) != 1 {
					t.Fatal("failed add must not write assignments")
				}
			} else if err != nil {
				t.Fatalf("legacy add should retain counted-once rule: %v", err)
			}
		})
	}
}
