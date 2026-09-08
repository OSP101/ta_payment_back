package service

import "testing"

// AUTH-01: the whole point of the fix is that an attacker cannot tell a real
// account from a made-up one by watching when the lockout kicks in. This
// drives both a real fixture account and a fabricated address through
// loginMaxFails wrong-password attempts side by side and asserts the status
// code AND message are identical at every step — not just that both
// eventually return 429.
func TestLogin_KnownAndUnknownEmailsAreIndistinguishableUnderLockout(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	const unknownEmail = "no-such-account@example.test"
	t.Cleanup(func() {
		loginGateReset(f.StaffID)
		unknownGateReset(unknownEmail)
	})

	knownEmail, err := f.users().GetEmail(f.ctx, f.StaffID)
	if err != nil {
		t.Fatalf("get fixture email: %v", err)
	}

	for i := 1; i <= loginMaxFails+1; i++ {
		_, knownErr := f.users().Authenticate(f.ctx, knownEmail, "not-the-password", "127.0.0.1", "test")
		_, unknownErr := f.users().Authenticate(f.ctx, unknownEmail, "not-the-password", "127.0.0.1", "test")

		kue, ok := knownErr.(*UserError)
		if !ok {
			t.Fatalf("attempt #%d: known-email error is not a UserError: %#v", i, knownErr)
		}
		uue, ok := unknownErr.(*UserError)
		if !ok {
			t.Fatalf("attempt #%d: unknown-email error is not a UserError: %#v", i, unknownErr)
		}
		if kue.Status != uue.Status {
			t.Fatalf("attempt #%d: status differs — known=%d unknown=%d (this is the enumeration oracle)",
				i, kue.Status, uue.Status)
		}
		if kue.Msg != uue.Msg {
			t.Fatalf("attempt #%d: message differs — known=%q unknown=%q (this is the enumeration oracle)",
				i, kue.Msg, uue.Msg)
		}
	}
}
