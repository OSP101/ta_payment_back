package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These cover the Batch 1 security fixes whose guard runs before any database
// access, so they need no fixture and stay fast. The guards are deliberately
// placed ahead of the first query — that is part of what is being asserted.

func TestReplaceClasses_RejectsOversizedSchedule(t *testing.T) {
	// A nil pool proves the cap short-circuits before any query: if the check
	// ever moves below ScheduleLockedReason this test panics instead of passing.
	svc := &WorkloadService{}
	blocks := make([]ClassBlock, maxClassBlocksPerTerm+1)

	err := svc.ReplaceClasses(context.Background(), uuid.New(), uuid.New(), blocks)
	if err == nil {
		t.Fatalf("expected %d blocks to be refused, got nil error", len(blocks))
	}
	if !strings.Contains(err.Error(), "ไม่เกิน") {
		t.Fatalf("expected a cap message, got %q", err.Error())
	}
}

func TestMintAllApprovedZipToken_RejectsOversizedSelection(t *testing.T) {
	svc := &DocsService{}
	ids := make([]uuid.UUID, maxBulkUserIDs+1)
	for i := range ids {
		ids[i] = uuid.New()
	}

	_, _, err := svc.MintAllApprovedZipToken(context.Background(), uuid.New(), "irrelevant", ids)
	if err == nil {
		t.Fatalf("expected %d user ids to be refused, got nil error", len(ids))
	}
	if !strings.Contains(err.Error(), "ไม่เกิน") {
		t.Fatalf("expected a cap message, got %q", err.Error())
	}
}

// TestValidatePassword_RejectsBlocklisted pins the property UserService.Create
// now relies on: an operator-supplied initial password goes through the same
// blocklist the self-service change path uses. "password1" is a literal entry
// in data/common_passwords.txt and is long enough to pass the old length-only
// check, which is exactly why it was the audit's reproduction value.
func TestValidatePassword_RejectsBlocklisted(t *testing.T) {
	if err := ValidatePassword("password1"); err == nil {
		t.Fatal("expected a blocklisted password to be refused")
	}
	// Length alone must not be what saves it.
	if len("password1") < 8 {
		t.Fatal("test value no longer exercises the length-passes-but-blocklisted case")
	}
}
