package service

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTDBMReconcile_AdoptsIdenticalManualSittingWithoutChangingEvidence(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(990001, firstMonday(), day(27), "lecture")
	id := uuid.New()
	f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,kind,makeup_date,start_time,end_time,note) VALUES($1,$2,$3::date,'lecture',$4::date,'09:00','12:00','manual evidence')`, id, f.SectionID, firstMonday(), day(27))
	f.insertLog(day(27), "lecture", "submitted", "09:00", "12:00")
	f.addSubmissionPeriod(firstMonday()[5:7], day(28), "exported", false)
	r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a})
	if r.Error != "" || r.Warning != "" || f.makeupCount() != 1 {
		t.Fatalf("identical manual evidence rejected: %+v", r)
	}
	var got uuid.UUID
	var note string
	if err := f.Pool.QueryRow(f.ctx, `SELECT t.applied_makeup_id,m.note FROM tdbm_extra_teachings t JOIN makeup_schedules m ON m.id=t.applied_makeup_id WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&got, &note); err != nil || got != id || note != "manual evidence" {
		t.Fatalf("changed evidence: %s/%s %v", got, note, err)
	}
}

func TestTDBMReconcile_RepairsLegacyPermutationAtomically(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlocked", true: "submitted"}[protected], func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			s := f.reviewService()
			original, _ := time.Parse("2006-01-02", firstMonday())
			next := original.AddDate(0, 0, 7).Format("2006-01-02")
			a := f.mariaEntry(990001, firstMonday(), day(27), "lecture")
			b := f.mariaEntry(990002, next, day(28), "lecture")
			s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b})
			var aid, bid uuid.UUID
			if err := f.Pool.QueryRow(f.ctx, `SELECT applied_makeup_id FROM tdbm_extra_teachings WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&aid); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(f.ctx, `SELECT applied_makeup_id FROM tdbm_extra_teachings WHERE extra_class_id=$1`, b.ExtraClassID).Scan(&bid); err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE makeup_schedules SET makeup_date=CASE WHEN id=$1 THEN $3::date ELSE $4::date END WHERE id IN($1,$2)`, aid, bid, day(28), day(27))
			f.exec(`UPDATE tdbm_extra_teachings SET applied_makeup_id=CASE WHEN extra_class_id=$1 THEN $3::uuid ELSE $4::uuid END WHERE extra_class_id IN($1,$2)`, a.ExtraClassID, b.ExtraClassID, bid, aid)
			if protected {
				f.insertLog(day(27), "lecture", "submitted", "09:00", "12:00")
			}
			r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b})
			if r.Error != "" || (r.Warning != "") != protected || f.makeupCount() != 2 {
				t.Fatalf("permutation %+v count=%d", r, f.makeupCount())
			}
			var got uuid.UUID
			var date string
			if err := f.Pool.QueryRow(f.ctx, `SELECT t.applied_makeup_id,m.makeup_date::text FROM tdbm_extra_teachings t JOIN makeup_schedules m ON m.id=t.applied_makeup_id WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&got, &date); err != nil {
				t.Fatal(err)
			}
			if protected {
				if got != bid {
					t.Fatal("protected links changed")
				}
			} else {
				if got != aid || date != day(27) {
					t.Fatalf("legacy permutation persisted: %s %s", got, date)
				}
			}
		})
	}
}

func TestTDBMReconcile_AuthoritativeChangeAndWaiver(t *testing.T) {
	for _, waived := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "waived"}[waived], func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			s := f.reviewService()
			a := f.mariaEntry(990001, firstMonday(), day(27), "lecture")
			id := uuid.New()
			if waived {
				f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,kind,waived) VALUES($1,$2,$3::date,'lecture',true)`, id, f.SectionID, firstMonday())
			} else {
				f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,kind,makeup_date,start_time,end_time,note) VALUES($1,$2,$3::date,'lecture',$4::date,'10:00','12:00','manual evidence')`, id, f.SectionID, firstMonday(), day(28))
			}
			r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a})
			if r.Error != "" || (r.Warning != "") != waived {
				t.Fatalf("authoritative correction: %+v", r)
			}
			var got uuid.UUID
			var reason *string
			if waived {
				if err := f.Pool.QueryRow(f.ctx, `SELECT reconciliation_error FROM tdbm_extra_teachings WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&reason); err != nil || reason == nil || *reason != tdbmWaivedConflict {
					t.Fatalf("waiver lost: %v %v", reason, err)
				}
			} else {
				if err := f.Pool.QueryRow(f.ctx, `SELECT applied_makeup_id FROM tdbm_extra_teachings WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&got); err != nil || got != id {
					t.Fatalf("manual row identity lost: %s %v", got, err)
				}
			}
		})
	}
}

func TestTDBMReconcile_SourceAmbiguityIsPartialWarning(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(990001, firstMonday(), day(27), "lecture")
	b := a
	b.ExtraClassID++
	b.ClassDate = day(28)
	c := f.mariaEntry(990003, firstMonday(), day(27), "lab")
	r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b, c})
	if r.Error != "" || r.Warning == "" || r.Conflicts != 2 || f.makeupCount() != 1 {
		t.Fatalf("not a partial success: %+v count=%d", r, f.makeupCount())
	}
	logs, err := s.RecentSyncLog(f.ctx, 10, f.AcademicYear, 1)
	if err != nil || len(logs) != 1 || logs[0].Error != nil || logs[0].Warning == nil {
		t.Fatalf("partial sync reported as failure: %+v %v", logs, err)
	}
}

// Optional replay from a read-only production export. No identities, contact
// details, credentials or payroll amounts are needed to reproduce the failure.
func TestTDBMReconcile_ProductionReplay(t *testing.T) {
	path := os.Getenv("TDBM_REPLAY_FILE")
	if path == "" {
		t.Skip("set TDBM_REPLAY_FILE for isolated production-state replay")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err = json.Unmarshal(blob, &snapshot); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fixtureOpts{NoRequest: true, TermStart: "2026-11-16", TermEnd: "2027-03-14"})
	f.exec(`UPDATE academic_terms SET semester=2 WHERE id=$1`, f.TermID)
	f.exec(`INSERT INTO teaching_courses(id,term_id,code,name_th,level,alt_codes,starts_on,ends_on,exported_at)
 SELECT id,$2,code,'Replay course','undergrad',alt_codes,starts_on,ends_on,exported_at FROM jsonb_to_recordset($1::jsonb) AS x(id uuid,code text,alt_codes text[],starts_on date,ends_on date,exported_at timestamptz)`, string(snapshot["courses"]), f.TermID)
	f.exec(`INSERT INTO sections(id,teaching_course_id,sec_no,track) SELECT id,teaching_course_id,sec_no,track::section_track FROM jsonb_to_recordset($1::jsonb) AS x(id uuid,teaching_course_id uuid,sec_no text,track text)`, string(snapshot["sections"]))
	f.exec(`INSERT INTO makeup_schedules(id,section_id,original_date,kind,makeup_date,start_time,end_time,waived,note) SELECT id,section_id,original_date,kind,makeup_date,start_time,end_time,waived,note FROM jsonb_to_recordset($1::jsonb) AS x(id uuid,section_id uuid,original_date date,kind text,makeup_date date,start_time time,end_time time,waived bool,note text)`, string(snapshot["makeups"]))
	f.exec(`INSERT INTO tdbm_extra_teachings SELECT * FROM jsonb_populate_recordset(NULL::tdbm_extra_teachings,$1::jsonb)`, string(snapshot["entries"]))
	var sourceRows []tdbmExtraTeachingRow
	if err = json.Unmarshal(snapshot["entries"], &sourceRows); err != nil {
		t.Fatal(err)
	}
	for _, r := range tdbmNormalizeReferences(sourceRows) {
		f.exec(`UPDATE tdbm_extra_teachings SET source_reference=$1 WHERE extra_class_id=$2`, r.Reference, r.ExtraClassID)
	}
	var before, after, linked int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings WHERE reconciliation_error IS NOT NULL`).Scan(&before)
	if _, err = f.reviewService().reconcileMakeups(f.ctx, 2569, 2); err != nil {
		t.Fatal(err)
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FILTER(WHERE reconciliation_error IS NOT NULL),count(*) FILTER(WHERE applied_makeup_id IS NOT NULL) FROM tdbm_extra_teachings`).Scan(&after, &linked)
	if after != 0 || linked <= 302 {
		t.Fatalf("replay did not recover: before=%d after=%d applied=%d", before, after, linked)
	}
	var unusable, wrong int
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings WHERE opt_status IN('A','D') AND NOT source_missing AND NOT source_reference AND applied_makeup_id IS NULL`).Scan(&unusable); err != nil || unusable != 0 {
		t.Fatalf("eligible sessions still unusable=%d err=%v", unusable, err)
	}
	if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings t JOIN makeup_schedules m ON m.id=t.applied_makeup_id
	 WHERE t.source_missing OR t.source_reference OR t.opt_status NOT IN('A','D') OR m.section_id IS DISTINCT FROM t.section_id
	 OR m.original_date IS DISTINCT FROM t.source_original_date OR m.kind IS DISTINCT FROM t.source_original_kind
	 OR m.makeup_date IS DISTINCT FROM t.class_date OR m.start_time IS DISTINCT FROM t.start_time OR m.end_time IS DISTINCT FROM t.end_time`).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("incorrect applied source links=%d err=%v", wrong, err)
	}
	t.Logf("isolated production replay: conflicts %d -> %d; applied=%d", before, after, linked)
	diagnostics, err := f.Pool.Query(f.ctx, `SELECT reconciliation_error,count(*),string_agg(extra_class_id::text,',' ORDER BY extra_class_id) FROM tdbm_extra_teachings WHERE reconciliation_error IS NOT NULL GROUP BY reconciliation_error`)
	if err != nil {
		t.Fatal(err)
	}
	for diagnostics.Next() {
		var reason, sourceIDs string
		var n int
		if err = diagnostics.Scan(&reason, &n, &sourceIDs); err != nil {
			t.Fatal(err)
		}
		t.Logf("remaining: %s count=%d source_ids=%s", reason, n, sourceIDs)
	}
	diagnostics.Close()
	// A second sync must be stable, including previous conflicting components.
	if _, err = f.reviewService().reconcileMakeups(f.ctx, 2569, 2); err != nil {
		t.Fatal(err)
	}
	var again int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings WHERE reconciliation_error IS NOT NULL`).Scan(&again)
	if again != after {
		t.Fatalf("non-idempotent replay: %d -> %d", after, again)
	}
}
