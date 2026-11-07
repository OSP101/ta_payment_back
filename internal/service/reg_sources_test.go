package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
	"ta-payment-back/internal/regkku"
)

func TestRegSources_CachesAvailabilityAndChecksPermission(t *testing.T) {
	f := regFixture(t, `'{}'`)
	newServer, newHits, _ := fakeReg(t, nil)
	var oldHits atomic.Int32
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { oldHits.Add(1); w.WriteHeader(403) }))
	defer oldServer.Close()
	s := NewRegEnrolmentService(f.Pool, newServer.URL, oldServer.URL)
	s.reg.Gap, s.reg.Jitter, s.legacy.Gap, s.legacy.Jitter = 0, 0, 0, 0
	if _, err := s.Sources(f.ctx, f.LecturerID); err == nil {
		t.Fatal("lecturer allowed")
	}
	if *newHits != 0 || oldHits.Load() != 0 {
		t.Fatal("unauthorized request touched REG")
	}
	for i := 0; i < 2; i++ {
		got, err := s.Sources(f.ctx, f.StaffID)
		if err != nil || len(got) != 2 || !got[0].Available || got[1].State != "blocked" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	}
	if atomic.LoadInt32(newHits) != 2 || oldHits.Load() != 1 {
		t.Fatal("reopening modal hammered sources")
	}
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll, regkku.SourceLegacy); err == nil {
		t.Fatal("known unavailable source started")
	}
	if oldHits.Load() != 1 {
		t.Fatal("start hit a blocked source")
	}
}

func TestRegEnrolment_SourceCachesAndJobsAreSeparate(t *testing.T) {
	f := regFixture(t, `'{}'`)
	newServer, newHits, _ := fakeReg(t, nil)
	list, err := os.ReadFile("../regkku/testdata/class_info_1_SC363001.tis620.html")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := os.ReadFile("../regkku/testdata/class_info_2_SC363001.html")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := charmap.Windows874.NewEncoder().Bytes(detail)
	if err != nil {
		t.Fatal(err)
	}
	var oldHits atomic.Int32
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldHits.Add(1)
		if r.URL.Path == "/class_info_1.asp" {
			w.Write(list)
		} else {
			w.Write(encoded)
		}
	}))
	defer oldServer.Close()
	s := NewRegEnrolmentService(f.Pool, newServer.URL, oldServer.URL)
	s.reg.Gap, s.reg.Jitter, s.legacy.Gap, s.legacy.Jitter = 0, 0, 0, 0
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	newJob := waitRegJob(t, s, f)
	before := atomic.LoadInt32(newHits)
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll, regkku.SourceLegacy); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var legacyJob *RegEnrolmentJob
	for time.Now().Before(deadline) {
		legacyJob, err = s.Status(context.Background(), f.StaffID, f.TermID, regkku.SourceLegacy)
		if err != nil {
			t.Fatal(err)
		}
		if legacyJob.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if legacyJob.Status != "done" || legacyJob.Reused != 0 || legacyJob.Source != regkku.SourceLegacy || oldHits.Load() != 2 || atomic.LoadInt32(newHits) != before {
		t.Fatalf("legacy job=%+v old hits=%d", legacyJob, oldHits.Load())
	}
	if len(legacyJob.Rows) != 1 || legacyJob.Rows[0].Regular != 73 || legacyJob.Rows[0].Special != 7 || legacyJob.Rows[0].Error != "" {
		t.Fatalf("legacy counts=%+v", legacyJob.Rows)
	}
	stillNew, err := s.Status(f.ctx, f.StaffID, f.TermID)
	if err != nil || stillNew.Source != regkku.SourceNew || stillNew.StartedAt != newJob.StartedAt {
		t.Fatal("source job overwritten")
	}
}
