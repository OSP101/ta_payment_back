// payee.go keeps the TA's bank account and postal address — the second
// exception to migration 0047's "nothing sensitive in the database" rule,
// after the citizen ID (citizen_id.go). See migration 0152: the finance
// office's Template-Suppliers sheet needs both, and nowhere else reads them.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// payeeKeyVersion is bumped with citizenIDKeyVersion whenever PII_ENC_KEY is
// rotated (cmd/rotate-pii-key re-seals both columns in one run).
const payeeKeyVersion = 1

// PayeeDetails is the sealed JSON in ta_profiles.payee_enc. Never serialised
// to an API response — only the Suppliers export reads it.
type PayeeDetails struct {
	BankName    string `json:"bank_name"`
	BankBranch  string `json:"bank_branch"`
	BranchCode  string `json:"branch_code"`
	AccountNo   string `json:"account_no"`
	AccountName string `json:"account_name"`
	// Address is the full joined line the Suppliers sheet prints; the parts
	// are kept too, in case finance later wants them in separate columns.
	Address       string `json:"address"`
	PostalCode    string `json:"postal_code"`
	AddressLine   string `json:"address_line,omitempty"`
	SubDistrictID int    `json:"sub_district_id,omitempty"`
	SubDistrict   string `json:"sub_district,omitempty"`
	District      string `json:"district,omitempty"`
	Province      string `json:"province,omitempty"`
}

// payeeAAD binds the ciphertext to its row AND to this column: the citizen ID
// uses the bare user id, so the two values cannot be swapped for each other.
func payeeAAD(userID uuid.UUID) []byte {
	return append(append([]byte{}, userID[:]...), "payee"...)
}

// storePayee seals and stores the bank/address fields of a validated form.
// Runs in UpsertProfile's transaction after the ta_profiles upsert, like
// storeCitizenID.
func (s *DocsService) storePayee(ctx context.Context, tx pgx.Tx, userID uuid.UUID, in TAProfile) error {
	if s.pii == nil {
		return errors.New("payee encryption is not configured (PII_ENC_KEY missing)")
	}
	sd, _ := LookupSubDistrict(in.SubDistrictID)
	plain, err := json.Marshal(PayeeDetails{
		BankName: strings.TrimSpace(in.BankName), BankBranch: strings.TrimSpace(in.BankBranch),
		BranchCode: stripNonDigits(in.BranchCode),
		AccountNo:  stripNonDigits(in.AccountNo), AccountName: strings.TrimSpace(in.AccountName),
		Address: in.Address, PostalCode: in.PostalCode,
		AddressLine: in.AddressLine, SubDistrictID: in.SubDistrictID,
		SubDistrict: sd.Name, District: sd.District, Province: sd.Province,
	})
	if err != nil {
		return err
	}
	sealed, err := s.pii.Seal(payeeAAD(userID), plain)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE ta_profiles SET payee_enc = $2, payee_key_version = $3 WHERE user_id = $1`,
		userID, sealed, payeeKeyVersion)
	return err
}

// RevealPayee decrypts userID's stored bank account and address. Audited on
// every call, same as RevealCitizenID. ErrNotFound = nothing on file (a TA
// approved before migration 0152 who has not re-sent the form).
func (s *DocsService) RevealPayee(ctx context.Context, actor, userID uuid.UUID, reason string) (*PayeeDetails, error) {
	if s.pii == nil {
		return nil, errors.New("payee encryption is not configured (PII_ENC_KEY missing)")
	}
	var sealed []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT payee_enc FROM ta_profiles WHERE user_id = $1`, userID,
	).Scan(&sealed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(sealed) == 0 {
		return nil, ErrNotFound
	}
	plain, err := s.pii.Open(payeeAAD(userID), sealed)
	if err != nil {
		return nil, err
	}
	var out PayeeDetails
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "ta_profile.payee.reveal", Entity: "ta_profile",
		EntityID: userID.String(), Note: reason,
	}); err != nil {
		return nil, err
	}
	return &out, nil
}
