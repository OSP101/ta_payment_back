package service

import (
	"math"
	"testing"
)

// The claim pack computes the graduate-special split ONCE (GradLumpSnapshot);
// every document reads it and the freeze writes exactly it, whatever moves in
// between.

func TestGradLumpSnapshot_FreezeWritesWhatThePackPrinted(t *testing.T) {
	f, exp, lump, m1, _ := gradLumpFixture(t)
	snap, err := exp.computeGradLumpSnapshot(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithGradLumpSnapshot(f.ctx, snap)

	// Weights move after the pack's split was taken (another month approved).
	addLaterHours(t, f)

	// Every document inside the pack still reads the pack's split.
	inPack, err := exp.gradLumpByMonth(ctx, f.CourseID, f.TAID, lump, true)
	if err != nil {
		t.Fatal(err)
	}
	if inPack[m1] != lump {
		t.Fatalf("inside the pack %s must stay at the snapshot's %.2f, got %.2f", m1, lump, inPack[m1])
	}
	comp, err := exp.buildExportRows(ctx, f.CourseID, []string{m1})
	if err != nil {
		t.Fatal(err)
	}
	var printed float64
	for _, r := range comp.records {
		if r.taID == f.TAID {
			printed = r.paySpecial
		}
	}
	if printed != lump {
		t.Fatalf("export rows printed %.2f for %s, want the snapshot's %.2f", printed, m1, lump)
	}
	// The forecast is not a document: it computes live.
	live, err := exp.gradLumpByMonth(ctx, f.CourseID, f.TAID, lump, false)
	if err != nil {
		t.Fatal(err)
	}
	if live[m1] == lump {
		t.Fatalf("approvedOnly=false must not read the snapshot, got %v", live)
	}

	if err := exp.FreezeGradLumpSnapshot(f.ctx, f.StaffID, snap, []string{m1}); err != nil {
		t.Fatal(err)
	}
	var frozen float64
	f.Pool.QueryRow(f.ctx, `SELECT baht::float8 FROM grad_lump_ledger
		WHERE teaching_course_id=$1 AND ta_id=$2 AND year_month=$3`, f.CourseID, f.TAID, m1).Scan(&frozen)
	if math.Abs(frozen-lump) > 0.005 {
		t.Fatalf("ledger froze %.2f, want what the pack printed %.2f", frozen, lump)
	}
	// Idempotent: freezing the same pack again is fine.
	if err := exp.FreezeGradLumpSnapshot(f.ctx, f.StaffID, snap, []string{m1}); err != nil {
		t.Fatalf("re-freezing the same pack: %v", err)
	}
}

func TestGradLumpSnapshot_ConcurrentExportWithOtherFiguresIsRefused(t *testing.T) {
	f, exp, _, m1, _ := gradLumpFixture(t)
	a, err := exp.computeGradLumpSnapshot(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	addLaterHours(t, f)
	b, err := exp.computeGradLumpSnapshot(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.FreezeGradLumpSnapshot(f.ctx, f.StaffID, b, []string{m1}); err != nil {
		t.Fatal(err)
	}
	err = exp.FreezeGradLumpSnapshot(f.ctx, f.StaffID, a, []string{m1})
	if userErrStatus(err) != 409 {
		t.Fatalf("a pack whose figures differ from the frozen ones must be refused, got %v", err)
	}
	var n int
	var frozen float64
	f.Pool.QueryRow(f.ctx, `SELECT COUNT(*), COALESCE(SUM(baht),0)::float8 FROM grad_lump_ledger
		WHERE teaching_course_id=$1`, f.CourseID).Scan(&n, &frozen)
	if n != 1 || math.Abs(frozen-b.byTA[f.TAID][m1]) > 0.005 {
		t.Fatalf("ledger must still hold the first export's figure: n=%d baht=%.2f", n, frozen)
	}
}
