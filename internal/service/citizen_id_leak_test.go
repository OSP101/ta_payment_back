package service

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// No JSON-serialisable struct anywhere in this package may carry a field
// exposing the new encrypted-storage columns. This is a static guard against
// the class of mistake that would matter most here: someone adding a
// CitizenIDEnc/CitizenIDLast4 field to TAProfile (or any other API response
// struct) later, for a plausible-sounding reason, without reading this file.
//
// TAProfile.NationalID ("national_id") is deliberately NOT flagged here — it
// is the pre-existing REQUEST field a TA submits, functionally proven to come
// back empty from GetProfile by TestUpsertProfile_StoresNothingSensitive and
// TestGetProfile_ResponseNeverContainsCitizenID. What must never exist is a
// field for the columns THIS change added: citizen_id_enc / citizen_id_last4.
func TestNoAPIStructExposesCitizenIDColumns(t *testing.T) {
	suspectTypes := []any{
		TAProfile{},
		PendingProfile{},
		Document{},
		History{},
	}
	for _, v := range suspectTypes {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("json")
			lower := strings.ToLower(tag)
			if strings.Contains(lower, "citizen_id_enc") || strings.Contains(lower, "citizen_id_last4") {
				t.Errorf("%s.%s has json tag %q — the encrypted citizen id storage must never be a response field",
					typ.Name(), typ.Field(i).Name, tag)
			}
		}
	}
}

// Functional check, on top of the static one above: with a citizen ID
// actually stored, GetProfile's response — the exact bytes a client would
// receive — must not contain the plaintext number, the word "citizen_id" in
// any casing, or anything resembling the encrypted column's contents.
func TestGetProfile_ResponseNeverContainsCitizenID(t *testing.T) {
	svc, ctx, userID := citizenIDFixture(t)
	const plainNationalID = "1234567890123"

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.storeCitizenID(ctx, tx, userID, plainNationalID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	p, err := svc.GetProfile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	// The stronger, more direct assertion: the field must be blank outright,
	// not merely "not the full 13 digits" — a last4 value smuggled into this
	// same field (e.g. under a refactor that repurposes it) would pass a
	// substring check on the full number while still leaking something.
	if p.NationalID != "" {
		t.Errorf("TAProfile.NationalID = %q, want empty — GetProfile must never populate this field", p.NationalID)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	// last4 of plainNationalID, checked independently of which field it might
	// end up smuggled into.
	if strings.Contains(text, plainNationalID[len(plainNationalID)-4:]) {
		t.Errorf("GetProfile response contains the citizen id's last 4 digits: %s", text)
	}
	if strings.Contains(text, plainNationalID) {
		t.Errorf("GetProfile response contains the plaintext citizen id: %s", text)
	}
	if strings.Contains(strings.ToLower(text), "citizen_id") {
		t.Errorf("GetProfile response mentions citizen_id at all: %s", text)
	}
}

// Same check for the staff review queue — a different response shape, same
// requirement: nothing about the stored citizen id should be visible to the
// officer reviewing the queue, not even that it exists.
func TestListReview_ResponseNeverContainsCitizenID(t *testing.T) {
	svc, ctx, userID := citizenIDFixture(t)
	const plainNationalID = "9876543210123"

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.storeCitizenID(ctx, tx, userID, plainNationalID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.pool.Exec(ctx,
		`UPDATE ta_profiles SET status = 'submitted', current_round = 1 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	list, err := svc.ListReview(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, plainNationalID) {
		t.Errorf("ListReview response contains the plaintext citizen id: %s", text)
	}
	if strings.Contains(strings.ToLower(text), "citizen_id") {
		t.Errorf("ListReview response mentions citizen_id at all: %s", text)
	}
}
