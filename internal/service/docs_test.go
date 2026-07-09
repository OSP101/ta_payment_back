package service

import "testing"

// The docs pipeline touches Postgres so a full end-to-end test needs a live
// database. These tests cover the pure helpers that gate every submission —
// bad input here should be rejected before any row is written.

func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"1234567890123", true},
		{"1234-5678", false},
		{"12a34", false},
		{" 123", false},
	}
	for _, c := range cases {
		if got := isAllDigits(c.in); got != c.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStripNonDigits(t *testing.T) {
	cases := map[string]string{
		"1234567890":         "1234567890",
		"123-456-7":          "1234567",
		" 12 34 ":            "1234",
		"1-2345-67890-12-3":  "1234567890123",
		"abc":                "",
	}
	for in, want := range cases {
		if got := stripNonDigits(in); got != want {
			t.Errorf("stripNonDigits(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStudentIDPattern(t *testing.T) {
	ok := []string{"653020123-4", "999999999-0"}
	bad := []string{
		"",
		"6530201234",     // no dash
		"65302012-34",    // dash at wrong position
		"6530201234-",    // trailing dash
		"6530a0123-4",    // non-digit
		"653020123-45",   // too long
		"65302012-4",     // too short
	}
	for _, s := range ok {
		if !studentIDPattern(s) {
			t.Errorf("studentIDPattern(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if studentIDPattern(s) {
			t.Errorf("studentIDPattern(%q) = true, want false", s)
		}
	}
}

func TestValidateBank(t *testing.T) {
	// A commercial bank (10-digit account).
	if err := validateBank(TAProfile{
		BankName:    "ธนาคารไทยพาณิชย์",
		AccountName: "Test",
		AccountNo:   "123-4-56789-0",
	}); err != nil {
		t.Errorf("SCB baseline should validate: %v", err)
	}
	// Government Savings Bank uses 12 digits — 10 should now be rejected.
	if err := validateBank(TAProfile{
		BankName:    "ธนาคารออมสิน",
		AccountName: "Test",
		AccountNo:   "1234567890",
	}); err == nil {
		t.Error("GSB with 10-digit account should be rejected")
	}
	// GSB 12-digit account should pass.
	if err := validateBank(TAProfile{
		BankName:    "ธนาคารออมสิน",
		AccountName: "Test",
		AccountNo:   "020-1-23456-789",
	}); err != nil {
		t.Errorf("GSB with 12-digit account should pass: %v", err)
	}
	// BAAC accepts both 10 and 12.
	for _, acc := range []string{"1234567890", "123456789012"} {
		if err := validateBank(TAProfile{
			BankName:    "ธนาคารเพื่อการเกษตรและสหกรณ์การเกษตร",
			AccountName: "Test",
			AccountNo:   acc,
		}); err != nil {
			t.Errorf("BAAC with %d-digit account should pass: %v", len(acc), err)
		}
	}
	// Unknown bank name — must be rejected.
	if err := validateBank(TAProfile{
		BankName:    "Made Up Bank",
		AccountName: "Test",
		AccountNo:   "1234567890",
	}); err == nil {
		t.Error("expected unknown bank_name to be rejected")
	}
	// Empty account name.
	if err := validateBank(TAProfile{
		BankName:    "ธนาคารไทยพาณิชย์",
		AccountName: " ",
		AccountNo:   "1234567890",
	}); err == nil {
		t.Error("blank account_name should be rejected")
	}
}

func TestDocKindsClosed(t *testing.T) {
	// Regression guard: adding a new document kind requires updating the
	// TA form and the staff review label map together, so the enum is
	// intentionally small and closed.
	for _, k := range []string{"national_id", "bank_book", "creditor_form"} {
		if !DocKinds[k] {
			t.Errorf("expected DocKinds[%q] to be true", k)
		}
	}
	for _, k := range []string{"", "passport", "other", "creditor"} {
		if DocKinds[k] {
			t.Errorf("did not expect DocKinds[%q] to be true", k)
		}
	}
}
