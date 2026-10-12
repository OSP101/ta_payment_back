package errtrack

import "testing"

func TestMaskDigits(t *testing.T) {
	for in, want := range map[string]string{
		"citizen 1100000000001 rejected":                      "citizen [digits hidden] rejected",
		"account 123-4-56789-0, phone 081 234 5678":           "account [digits hidden], phone [digits hidden]",
		"claim 123 for 2568/10 (id 12345678)":                 "claim 123 for 2568/10 (id 12345678)",
		"request 550e8400-e29b-41d4-a716-446655440000 failed": "request 550e8400-e29b-41d4-a716-446655440000 failed",
	} {
		if got := maskDigits(in); got != want {
			t.Errorf("maskDigits(%q) = %q, want %q", in, got, want)
		}
	}
}
