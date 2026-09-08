package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// PDPA-03: the public checklist board (an anonymous link pasted into a LINE
// group) reused SignatureItem as-is, which carries signer_id — users.id, the
// join key /users/:id/avatar and friends require login for. This pins that
// the public projection never serialises signer_id, and that SignerRef
// stands in for it without being reversible to the real id.
func TestToPublicSignatureItems_NeverExposesSignerID(t *testing.T) {
	signerID := uuid.New()
	termID := uuid.New()
	items := []SignatureItem{
		{TeachingCourseID: uuid.New(), Code: "SC362104", Role: "ta", SignerID: &signerID, Responsible: "ทดสอบ ทดสอบ"},
		{TeachingCourseID: uuid.New(), Code: "SC362104", Role: "certifier", SignerID: nil, Responsible: "ผู้รับรอง"},
	}

	out := ToPublicSignatureItems(items, termID)
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(b)

	if strings.Contains(body, "signer_id") {
		t.Fatalf("public projection must never carry the signer_id key, got: %s", body)
	}
	if strings.Contains(body, signerID.String()) {
		t.Fatalf("public projection leaked the raw signer uuid, got: %s", body)
	}

	if out[0].SignerRef == "" {
		t.Fatal("a row with a real signer must get a non-empty SignerRef")
	}
	if out[1].SignerRef != "" {
		t.Fatalf("a row with no signer (certifier) must not get a SignerRef, got %q", out[1].SignerRef)
	}

	// Bound to the term: the same person's ref must not carry across terms,
	// which would let a public viewer link one person's rows across boards.
	otherTermRef := ToPublicSignatureItems(items, uuid.New())[0].SignerRef
	if otherTermRef == out[0].SignerRef {
		t.Fatal("SignerRef must differ across terms for the same signer")
	}
}
