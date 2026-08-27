package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// signer_authority.go answers one question the appointment order could not ask
// before: is the person signing this order the dean, or standing in for them?
//
// A คำสั่ง is issued under the dean's authority (มาตรา 40 แห่ง พ.ร.บ.
// มหาวิทยาลัยขอนแก่น พ.ศ. 2558, cited in the order itself). When the dean is
// away or the seat is vacant, a รองคณบดี signs — but NOT as themselves: Thai
// official-document practice requires the block to name their own position,
// the acting phrase, and the position whose authority they are exercising:
//
//	(ผู้ช่วยศาสตราจารย์ ดร.ณกร วัฒนกิจ)
//	รองคณบดีฝ่ายวิชาการ รักษาการแทน
//	คณบดีวิทยาลัยการคอมพิวเตอร์
//
// Printing only "รองคณบดีฝ่ายวิชาการ" — which is what the order did until now —
// produces a document signed by someone with no authority to issue it.
//
// The rule is derived from the title rather than stored as a flag: the officer
// roster is free text staff maintain themselves, and a boolean would be one
// more field to keep in sync with the words next to it. Derived here, in one
// place, so the screen and the printed page can never disagree.

// deanTitlePrefix marks the dean's own seat. A PREFIX, not a substring:
// "รองคณบดีฝ่ายวิชาการ" and "ผู้ช่วยคณบดีฝ่ายดิจิทัล" both contain "คณบดี" and
// both are deputies who would have to sign as acting.
const deanTitlePrefix = "คณบดี"

// headTitlePrefix marks the head of department, who certifies claim forms. The
// same prefix-not-substring reasoning applies — "รองหัวหน้าสาขาวิชา" would act.
const headTitlePrefix = "หัวหน้าสาขา"

// fallbackDeanTitle is used only when no dean is on the roster at all — which
// is precisely the vacancy that makes someone act in the first place. The
// college name matches the letterhead the order already prints.
const fallbackDeanTitle = "คณบดีวิทยาลัยการคอมพิวเตอร์"

// fallbackHeadTitle matches the position line the claim-form template prints
// above the ผู้รับรอง signature line.
const fallbackHeadTitle = "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"

// actingPhrase is the connector between the signer's own position and the one
// they are exercising.
//
// "รักษาการแทน", not the ministry-wide "รักษาราชการแทน": KKU is a university
// under its own act, and พ.ร.บ.มหาวิทยาลัยขอนแก่น พ.ศ. 2558 — the same act
// มาตรา 40 of which this order is issued under — words it "ผู้รักษาการแทน".
// A single constant, so the whole system changes wording in one edit.
const actingPhrase = "รักษาการแทน"

// IsDeanTitle reports whether a roster title is the dean's own seat.
func IsDeanTitle(title string) bool {
	return strings.HasPrefix(strings.TrimSpace(title), deanTitlePrefix)
}

// IsHeadTitle reports whether a roster title is the head-of-department seat,
// the one that certifies claim forms.
func IsHeadTitle(title string) bool {
	return strings.HasPrefix(strings.TrimSpace(title), headTitlePrefix)
}

// signerAuthority is who signs and under whose authority, already worded for
// the page. The renderers place lines; they do not decide what a line says.
type signerAuthority struct {
	Name string // "รองศาสตราจารย์ ดร.สิรภัทร เชี่ยวชาญวัฒนา"
	// Title is the position line: the signer's own seat, with the acting phrase
	// appended when they are standing in ("รองคณบดีฝ่ายวิชาการ รักษาการแทน").
	Title string
	// ActingFor is the seat being exercised, printed on the following line.
	// Empty when the signer holds it themselves.
	ActingFor string
}

// loadSignerAuthority resolves one officer into a signature block.
func loadSignerAuthority(ctx context.Context, pool *pgxpool.Pool, officerID uuid.UUID) (signerAuthority, error) {
	var a signerAuthority
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(ao.academic_prefix,'') || COALESCE(u.first_name || ' ' || u.last_name, ao.full_name), ao.title
		FROM admin_officers ao
		LEFT JOIN users u ON u.id = ao.user_id
		WHERE ao.id = $1`, officerID).Scan(&a.Name, &a.Title); err != nil {
		return signerAuthority{}, Invalid("ไม่พบข้อมูลผู้ลงนามในระบบ")
	}
	a.applyActing(ctx, pool, deanTitlePrefix, fallbackDeanTitle)
	return a, nil
}

// applyActing rewrites the block into the acting form unless the signer already
// holds the seat. Shared by the two seats the system prints — the dean on the
// คำสั่ง and the head of department on the claim form — because the rule is one
// rule, and two copies of it would drift the moment one was corrected.
func (a *signerAuthority) applyActing(
	ctx context.Context, pool *pgxpool.Pool, seatPrefix, fallbackSeat string,
) {
	if strings.HasPrefix(strings.TrimSpace(a.Title), seatPrefix) {
		return
	}
	a.ActingFor = seatTitle(ctx, pool, seatPrefix, fallbackSeat)
	a.Title = a.Title + " " + actingPhrase
}

// seatTitle returns a seat's title as the roster words it, so the acting line
// reads exactly like the seat it names. Prefers an active holder; falls back to
// an inactive row before the constant, because a roster that still remembers the
// previous holder words the seat better than a hardcoded guess.
func seatTitle(ctx context.Context, pool *pgxpool.Pool, seatPrefix, fallback string) string {
	var t string
	if err := pool.QueryRow(ctx, `
		SELECT title FROM admin_officers
		WHERE title LIKE $1 || '%'
		ORDER BY is_active DESC, created_at
		LIMIT 1`, seatPrefix).Scan(&t); err == nil && strings.TrimSpace(t) != "" {
		return t
	}
	return fallback
}
