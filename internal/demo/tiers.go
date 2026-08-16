// tiers.go implements the permission-tier restriction on top of demo entry:
// staff testers may log into any seeded account, lecturer testers may log
// into lecturer or TA accounts, and TA testers may log into TA accounts
// only. This exists because /api/demo/enter used to hand back every seeded
// account (admin through TA) to whoever typed any email — fine while only
// staff tried the sandbox themselves, wrong once real TAs and lecturers get
// invited: a TA has no legitimate reason to see the staff/admin screens,
// and since every seeded account shares one password (DemoPassword, by
// design — see seed.go), nothing but this check stood between "type any
// email" and "log into admin@demo.local". See demo_authorized_testers'
// migration doc comment and authorized.go for where a tester's tier comes
// from; this file only defines what each tier is allowed to do with it.
package demo

// Tier is a demo tester's permission ceiling, assigned by staff (see
// authorized.go) and re-checked on every login — never trusted from a
// cached or client-supplied value.
type Tier string

const (
	TierStaff    Tier = "staff"
	TierLecturer Tier = "lecturer"
	TierTA       Tier = "ta"
)

var validTiers = map[Tier]bool{
	TierStaff:    true,
	TierLecturer: true,
	TierTA:       true,
}

// ParseTier validates a tier string from a request body — used by the
// admin-facing add-tester endpoint, the one place a tier arrives as
// untrusted input rather than being read back out of the database.
func ParseTier(s string) (Tier, bool) {
	t := Tier(s)
	return t, validTiers[t]
}

// tierAllowsRole reports whether a tester at tier t may log into a demo
// account carrying the given production roles. Mirrors this system's own
// staff > lecturer > ta shape: a staff tester needs to exercise every
// screen (that's the point of the sandbox for them), a lecturer tester
// should only ever see what a lecturer or the TAs they supervise can see,
// and a TA tester should only ever see their own TA-level surface.
func tierAllowsRole(t Tier, roles []string) bool {
	switch t {
	case TierStaff:
		return true
	case TierLecturer:
		for _, r := range roles {
			if r == "admin" || r == "staff" {
				return false
			}
		}
		return true
	case TierTA:
		for _, r := range roles {
			if r != "ta" {
				return false
			}
		}
		return len(roles) > 0
	default:
		return false
	}
}

// AccountsForTier filters demoAccounts down to what /api/demo/enter hands a
// tester at tier t — the role picker never shows a card the tester isn't
// allowed to click in the first place. This is a UX filter, not the
// enforcement: LoginHandler.Login re-checks with TierAllowsAccountEmail
// against whatever the request actually asks for, since hiding a card
// client-side stops nobody who opens devtools.
func AccountsForTier(t Tier) []DemoAccount {
	out := make([]DemoAccount, 0, len(demoAccounts))
	for _, a := range demoAccounts {
		if tierAllowsRole(t, a.Roles) {
			out = append(out, a)
		}
	}
	return out
}

// TierAllowsAccountEmail is the actual enforcement point — LoginHandler.Login
// calls this against whatever demo account email the request names, using a
// tier freshly resolved for the CURRENT slot owner (see
// Manager.TierForSlotIndex), not one cached from an earlier request. An
// account email demoAccounts doesn't recognize at all is never allowed,
// tier notwithstanding.
func TierAllowsAccountEmail(t Tier, accountEmail string) bool {
	for _, a := range demoAccounts {
		if a.Email == accountEmail {
			return tierAllowsRole(t, a.Roles)
		}
	}
	return false
}
