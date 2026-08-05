package service

import "testing"

// needsStaff is the single answer both staff screens read for "is this month my
// work". They used to decide it separately and disagreed on exactly one case: a
// month whose rows were ALL forfeited. The grid skipped it (nothing to sign);
// the payout list counted it, so SC362102 and SC363001 sat under
// "รอคุณดำเนินการ" over months worth ฿0 that no click could ever clear.
func TestNeedsStaff(t *testing.T) {
	cases := []struct {
		name string
		row  ReviewQueueRow
		want bool
	}{
		{"approved work waiting for sign-off",
			ReviewQueueRow{Status: "pending", OpenRows: 0, RowCount: 12}, true},
		{"everything forfeited — nothing to sign, nothing to pay",
			ReviewQueueRow{Status: "pending", OpenRows: 0, RowCount: 0, Forfeited: 10}, false},
		{"still with the TA or lecturer",
			ReviewQueueRow{Status: "pending", OpenRows: 4, RowCount: 6}, false},
		{"already signed off",
			ReviewQueueRow{Status: StatusStaffReviewed, OpenRows: 0, RowCount: 12}, false},
		{"already exported",
			ReviewQueueRow{Status: "exported", OpenRows: 0, RowCount: 12}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.row.needsStaff(); got != c.want {
				t.Errorf("needsStaff() = %v, want %v", got, c.want)
			}
		})
	}
}
