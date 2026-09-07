package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// TransferCoverMonthStatus is shared by two coverage endpoints, and only ONE of
// them has a readiness gate.
//
// ปะหน้าจ่ายตรง is keyed into the university's ERP, so a month still waiting on
// somebody must not be selectable. The course export ZIP has no such rule. Both
// render through the same month picker, so a plain `Ready bool` shipped
// `"ready": false` from the course endpoint by omission and greyed out every
// month on a page that never had this gate — the readiness rule of one document
// silently disabling another.
func TestMonthStatus_ReadyIsAbsentWhereThereIsNoSuchGate(t *testing.T) {
	// As CourseExportCoverage builds it: no readiness answer at all.
	b, err := json.Marshal(TransferCoverMonthStatus{
		TermMonth: TermMonth{YearMonth: "2026-06", Label: "มิถุนายน 2569"},
		Issued:    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ready") {
		t.Errorf("got %s — a month with no readiness gate must not claim to be "+
			"un-ready; the picker reads a present `false` as \"do not allow\"", b)
	}
}

// And where the gate does apply, both answers have to survive the wire.
func TestMonthStatus_ReadyIsCarriedWhenTheGateApplies(t *testing.T) {
	for _, want := range []bool{true, false} {
		v := want
		b, err := json.Marshal(TransferCoverMonthStatus{
			TermMonth: TermMonth{YearMonth: "2026-06", Label: "มิถุนายน 2569"},
			Ready:     &v,
		})
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			Ready *bool `json:"ready"`
		}
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.Ready == nil || *back.Ready != want {
			t.Errorf("ready=%v did not survive: %s", want, b)
		}
	}
}
