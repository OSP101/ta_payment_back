package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The throttle IS the design. Without it these screens — some polled every
// twenty seconds, all revalidated on tab focus — would file thousands of
// identical rows a day into a table that migration 0107 made impossible to
// clean up, burying the reads that mean something.
func TestReadAuditThrottle_RecordsOncePerWindow(t *testing.T) {
	viewer := uuid.New()
	k := readAuditKey{viewer: viewer, action: "test.view", subject: ""}
	t0 := time.Now()

	if !readAuditDue(k, t0) {
		t.Fatal("the first look was not recorded")
	}
	// Everything inside the window is the same look.
	for _, d := range []time.Duration{time.Second, time.Minute, readAuditWindow - time.Second} {
		if readAuditDue(k, t0.Add(d)) {
			t.Errorf("a second look %v later was recorded again — the poll would flood the trail", d)
		}
	}
	// Past it, a fresh look.
	if !readAuditDue(k, t0.Add(readAuditWindow+time.Second)) {
		t.Error("a look after the window was suppressed — the trail would go silent on a real return visit")
	}
}

// Two people reading the same screen, and one person reading two different
// subjects, are different acts. Collapsing either would answer "who has been
// looking at this person's record" with somebody else's window.
func TestReadAuditThrottle_SeparatesViewersAndSubjects(t *testing.T) {
	t0 := time.Now()
	a, b := uuid.New(), uuid.New()

	if !readAuditDue(readAuditKey{viewer: a, action: "test.sep", subject: "x"}, t0) {
		t.Fatal("first viewer not recorded")
	}
	if !readAuditDue(readAuditKey{viewer: b, action: "test.sep", subject: "x"}, t0) {
		t.Error("a second viewer was suppressed by the first viewer's window")
	}
	if !readAuditDue(readAuditKey{viewer: a, action: "test.sep", subject: "y"}, t0) {
		t.Error("a different subject was suppressed — reading two people's records must file two entries")
	}
	if readAuditDue(readAuditKey{viewer: a, action: "test.sep", subject: "x"}, t0.Add(time.Minute)) {
		t.Error("the original (viewer, subject) pair lost its window")
	}
}

// A long-lived process must not hold a key for every viewer of every screen
// forever. The sweep drops what has aged out and keeps what has not.
func TestSweepReadAudit_DropsOnlyExpiredKeys(t *testing.T) {
	t0 := time.Now()
	old := readAuditKey{viewer: uuid.New(), action: "test.sweep.old", subject: ""}
	fresh := readAuditKey{viewer: uuid.New(), action: "test.sweep.fresh", subject: ""}
	readAuditDue(old, t0.Add(-2*readAuditWindow))
	readAuditDue(fresh, t0)

	SweepReadAudit(t0)

	if _, ok := readAuditSeen.Load(old); ok {
		t.Error("an expired key survived the sweep; the map grows without bound")
	}
	if _, ok := readAuditSeen.Load(fresh); !ok {
		t.Error("a live key was swept, so the next poll would be recorded as a fresh look")
	}
}
