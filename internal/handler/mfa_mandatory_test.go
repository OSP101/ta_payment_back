package handler

import (
	"testing"

	"ta-payment-back/internal/rbac"
)

// mfaMandatoryFor is what decides whether AccountGuard blocks a request at
// mfa_setup_required — pinning it directly (rather than only through a full
// HTTP round trip) means a future edit to the tier list shows up here first.
func TestMFAMandatoryFor(t *testing.T) {
	cases := []struct {
		name        string
		roles       []string
		isExecutive bool
		want        bool
	}{
		{"admin is mandatory", []string{rbac.RoleAdmin}, false, true},
		{"staff is mandatory", []string{rbac.RoleStaff}, false, true},
		{"lecturer alone is optional", []string{rbac.RoleLecturer}, false, false},
		{"ta alone is optional", []string{rbac.RoleTA}, false, false},
		{"lecturer flagged executive is mandatory", []string{rbac.RoleLecturer}, true, true},
		{"ta flagged executive is mandatory", []string{rbac.RoleTA}, true, true},
		{"no roles, not executive", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mfaMandatoryFor(tc.roles, tc.isExecutive); got != tc.want {
				t.Errorf("mfaMandatoryFor(%v, executive=%v) = %v, want %v",
					tc.roles, tc.isExecutive, got, tc.want)
			}
		})
	}
}
