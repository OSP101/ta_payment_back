package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// One e-mail per review, not one per file (office, 03/10/2026).
//
// Officers judge the three documents one at a time, and every verdict used to
// send its own notice: three e-mails for one sitting, a fourth when the last
// approval completed the profile. That cluttered the TA's inbox and spent the
// faculty relay's quota for no gain. Now a verdict only records itself; the
// notice goes out once no uploaded document is still waiting for a verdict,
// and it lists all three: which passed, which did not and why.

// docReviewOrder is the order the TA's documents page lists the documents in.
var docReviewOrder = []string{"creditor_form", "national_id", "bank_book"}

// docVerdict is one document's line in the review-result notice.
type docVerdict struct {
	Kind   string
	Status string // "" = never uploaded
	Reason string
}

// settledVerdicts returns every required document's current verdict, or nil
// while an uploaded document still waits for one.
//
// It locks the profile row first. Two officers deciding the last two
// documents at the same moment both reach this point; the lock makes the
// second wait until the first commits, so exactly one of them sees the set
// complete and sends the notice.
func settledVerdicts(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]docVerdict, error) {
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM ta_profiles WHERE user_id = $1 FOR UPDATE`, userID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT kind, status::text, COALESCE(reject_reason, '')
		  FROM ta_documents
		 WHERE user_id = $1 AND kind = ANY($2) AND superseded_at IS NULL`,
		userID, requiredDocKinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKind := map[string]docVerdict{}
	for rows.Next() {
		var v docVerdict
		if err := rows.Scan(&v.Kind, &v.Status, &v.Reason); err != nil {
			return nil, err
		}
		if v.Status == "pending" || v.Status == "submitted" {
			return nil, nil
		}
		byKind[v.Kind] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byKind) == 0 {
		return nil, nil
	}
	out := make([]docVerdict, 0, len(docReviewOrder))
	for _, k := range docReviewOrder {
		v, ok := byKind[k]
		if !ok {
			v = docVerdict{Kind: k}
		}
		out = append(out, v)
	}
	return out, nil
}

// notifyReviewResult sends the one notice for a settled review. A nil
// verdicts (review not finished yet) sends nothing.
func (s *DocsService) notifyReviewResult(ctx context.Context, userID uuid.UUID, verdicts []docVerdict, profileApproved bool) {
	if s.notify == nil || verdicts == nil {
		return
	}
	var passed, failed, missing int
	lines := make([]string, 0, len(verdicts))
	rows := make([][]string, 0, len(verdicts))
	for _, v := range verdicts {
		label := kindLabel(v.Kind)
		switch v.Status {
		case "approved":
			passed++
			lines = append(lines, label+": ผ่าน")
			rows = append(rows, []string{label, "ผ่าน", "-"})
		case "rejected", "needs_fix":
			failed++
			lines = append(lines, label+": ไม่ผ่าน เนื่องจาก "+v.Reason)
			rows = append(rows, []string{label, "ไม่ผ่าน", v.Reason})
		default:
			missing++
			lines = append(lines, label+": ยังไม่ได้ส่ง")
			rows = append(rows, []string{label, "ยังไม่ได้ส่ง", "กรุณาอัปโหลดในระบบ"})
		}
	}
	table := &MailTable{
		Title: "ผลการตรวจสอบเอกสาร",
		Head:  []string{"เอกสาร", "ผลการตรวจสอบ", "หมายเหตุ"},
		Rows:  rows,
	}

	if failed == 0 && missing == 0 {
		title := fmt.Sprintf("เอกสารผ่านการตรวจสอบครบทั้ง %d รายการ", passed)
		intro := "เจ้าหน้าที่ได้ตรวจสอบเอกสารประกอบการเบิกจ่ายของท่านแล้ว เอกสารผ่านการตรวจสอบครบทุกรายการ"
		if profileApproved {
			intro += " และข้อมูลส่วนตัวของท่านได้รับการอนุมัติแล้ว ท่านสามารถใช้งานเมนูอื่นของระบบได้ทันที"
		}
		s.notify.SendLaidOut(ctx, userID, title, intro+"\n"+numberedLines(lines),
			"/ta/documents", false, MailLayout{Intro: intro, Table: table})
		return
	}

	title := fmt.Sprintf("ผลการตรวจเอกสาร ผ่าน %d รายการ ต้องแก้ไข %d รายการ", passed, failed+missing)
	intro := fmt.Sprintf("เจ้าหน้าที่ได้ตรวจสอบเอกสารประกอบการเบิกจ่ายของท่านแล้ว ผ่านการตรวจสอบ %d รายการ ต้องแก้ไข %d รายการ ดังนี้",
		passed, failed+missing)
	after := "กรุณาแก้ไขเฉพาะรายการที่ไม่ผ่านและส่งใหม่อีกครั้งในระบบ รายการที่ผ่านแล้วไม่ต้องส่งใหม่"
	s.notify.SendLaidOut(ctx, userID, title, intro+"\n"+numberedLines(lines)+"\n\n"+after,
		"/ta/documents", true, MailLayout{
			Intro:       intro,
			Table:       table,
			After:       after,
			ButtonLabel: "แก้ไขเอกสาร",
		})
}
