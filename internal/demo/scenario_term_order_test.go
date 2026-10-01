package demo

import (
	"context"
	"testing"

	"ta-payment-back/internal/timeutil"
)

// Step 1 of the guided walkthrough failed with "ช่วงภาคเรียนทับซ้อนกับภาคเรียน
// 2569/2" when the presentation dataset had been loaded first: both terms are
// anchored on today, and UpsertTerm refuses overlapping terms. Either order
// must work.
func TestScenarioTermWorksAfterPresentation(t *testing.T) {
	ctx := context.Background()
	slot, _ := newTestSlot(t)
	// Only the presentation's TERM step: that is the row the walkthrough
	// clashed with, and the rest of the dataset is not under test here.
	if err := (&presBuilder{slot: slot, svc: slot.Container, now: timeutil.Now()}).term(ctx); err != nil {
		t.Fatalf("presentation term: %v", err)
	}
	if _, err := stepTerm(ctx, slot.Container); err != nil {
		t.Fatalf("stepTerm after presentation: %v", err)
	}
	if _, err := activeTermID(ctx, slot.Container); err != nil {
		t.Fatalf("no active term after step 1: %v", err)
	}
	if _, err := stepCourses(ctx, slot.Container); err != nil {
		t.Fatalf("stepCourses after presentation: %v", err)
	}
}

func TestPresentationWorksAfterScenarioTerm(t *testing.T) {
	ctx := context.Background()
	slot, _ := newTestSlot(t)
	if _, err := stepTerm(ctx, slot.Container); err != nil {
		t.Fatalf("stepTerm: %v", err)
	}
	if err := (&presBuilder{slot: slot, svc: slot.Container, now: timeutil.Now()}).term(ctx); err != nil {
		t.Fatalf("presentation term after step 1: %v", err)
	}
	var months int
	if err := slot.Pool.QueryRow(ctx, `SELECT months FROM academic_terms WHERE semester = $1`, PresentationSemester).Scan(&months); err != nil {
		t.Fatal(err)
	}
	if months != 6 {
		t.Errorf("presentation months = %d, want the 6 its budgets are tuned for", months)
	}
}
