package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pdfgen"
	"ta-payment-back/internal/timeutil"
)

// CorrectTAIdentityInput is a staff correction of what a TA typed wrong on
// their profile form — the TA comes to the office, staff check the card and
// fix it here (requested 02/10/2026). Each field is optional; nil = unchanged.
type CorrectTAIdentityInput struct {
	StudentID  *string `json:"student_id,omitempty" validate:"omitempty,max=20"`
	NationalID *string `json:"national_id,omitempty" validate:"omitempty,max=20"`
	Prefix     *string `json:"prefix,omitempty" validate:"omitempty,max=20"`
	Reason     string  `json:"reason" validate:"required,max=500"`
}

// TAIdentity is what the correction form shows before editing. The citizen ID
// is never returned whole — staff compare the last 4 digits with the card.
type TAIdentity struct {
	StudentID      *string `json:"student_id"`
	Prefix         *string `json:"prefix"`
	CitizenIDLast4 *string `json:"citizen_id_last4"`
	HasProfile     bool    `json:"has_profile"`
	// Foreign: the stored number is the tax ID, not a citizen ID.
	Foreign bool `json:"foreign"`
}

func (s *DocsService) GetTAIdentity(ctx context.Context, userID uuid.UUID) (*TAIdentity, error) {
	out := &TAIdentity{}
	err := s.pool.QueryRow(ctx, `
		SELECT u.student_id, p.prefix, NULLIF(p.citizen_id_last4, ''), p.user_id IS NOT NULL,
		       u.nationality = 'foreign'
		  FROM users u LEFT JOIN ta_profiles p ON p.user_id = u.id
		 WHERE u.id = $1 AND u.deleted_at IS NULL`, userID,
	).Scan(&out.StudentID, &out.Prefix, &out.CitizenIDLast4, &out.HasProfile, &out.Foreign)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// CorrectTAIdentity fixes a TYPO in the TA's current details. It is not a
// level change: a new student id from moving up to โท/เอก goes through
// EnrollmentService.RecordTransition, which opens a new period. Here the
// active period keeps its identity and only its value is corrected, and so is
// every assignment snapshot taken from that period — the wrong id was never a
// fact worth preserving, and leaving the snapshots would keep printing it on
// every reprint of the documents.
//
// The citizen ID is re-encrypted exactly as the TA's own submission stores it
// (storeCitizenID). Neither the old nor the new number reaches the audit row:
// it records that the field changed, the last 4 digits, and the reason.
func (s *DocsService) CorrectTAIdentity(ctx context.Context, actor, userID uuid.UUID, in CorrectTAIdentityInput) error {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return Invalid("กรุณาระบุเหตุผลการแก้ไข")
	}
	if in.StudentID == nil && in.NationalID == nil && in.Prefix == nil {
		return Invalid("ไม่มีข้อมูลที่ต้องแก้ไข")
	}
	var sid, nid string
	if in.StudentID != nil {
		sid = strings.TrimSpace(*in.StudentID)
		if !studentIDPattern(sid) {
			return Invalid("รหัสนักศึกษาต้องอยู่ในรูปแบบ XXXXXXXXX-X")
		}
	}
	if in.NationalID != nil {
		foreign, err := userIsForeign(ctx, s.pool, userID)
		if err != nil {
			return err
		}
		if nid, err = validateIDNumber(*in.NationalID, foreign); err != nil {
			return err
		}
	}
	if in.Prefix != nil && !AllowedPrefixes[*in.Prefix] {
		return Invalid("คำนำหน้าต้องเป็น นาย, นาง หรือ นางสาว")
	}
	isTA, err := hasRole(ctx, s.pool, userID, "ta")
	if err != nil {
		return err
	}
	if !isTA {
		return Invalid("แก้ไขได้เฉพาะบัญชีผู้ช่วยสอน")
	}
	// Same reach as every other staff edit of an account (assertMayManage):
	// an account that also holds admin is admin's to change.
	if targetAdmin, err := hasRole(ctx, s.pool, userID, "admin"); err != nil {
		return err
	} else if targetAdmin {
		if actorAdmin, err := hasRole(ctx, s.pool, actor, "admin"); err != nil {
			return err
		} else if !actorAdmin {
			return Forbidden("เฉพาะผู้ดูแลระบบ (admin) เท่านั้นที่จัดการบัญชีผู้ดูแลระบบได้")
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var hasProfile bool
	var oldSID *string
	if err := tx.QueryRow(ctx, `
		SELECT u.student_id, EXISTS (SELECT 1 FROM ta_profiles p WHERE p.user_id = u.id)
		  FROM users u WHERE u.id = $1 AND u.deleted_at IS NULL FOR UPDATE`, userID,
	).Scan(&oldSID, &hasProfile); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if (in.NationalID != nil || in.Prefix != nil) && !hasProfile {
		return Invalid("ผู้ช่วยสอนยังไม่เคยส่งแบบฟอร์มข้อมูลส่วนตัว จึงยังไม่มีข้อมูลให้แก้ไข")
	}

	changed := map[string]any{"reason": in.Reason}
	if in.StudentID != nil && (oldSID == nil || *oldSID != sid) {
		var activeID *uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM ta_enrollments WHERE user_id = $1 AND ended_at IS NULL`, userID,
		).Scan(&activeID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE users SET student_id = $2, updated_at = NOW() WHERE id = $1`, userID, sid); err != nil {
			return err
		}
		snapshots := int64(0)
		if activeID != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE ta_enrollments SET student_id = $2 WHERE id = $1`, *activeID, sid); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx,
				`UPDATE ta_request_assignments SET student_id_snapshot = $2 WHERE enrollment_id = $1`, *activeID, sid)
			if err != nil {
				return err
			}
			snapshots = tag.RowsAffected()
		} else {
			// No period yet (never submitted a profile): open the first one,
			// the same row UpsertProfile would have created.
			if _, err := tx.Exec(ctx, `
				INSERT INTO ta_enrollments (user_id, student_id, study_level, created_by, note)
				SELECT id, $2, COALESCE(study_level, 'undergrad'), $3, $4 FROM users WHERE id = $1`,
				userID, sid, actor, "เจ้าหน้าที่บันทึก: "+in.Reason); err != nil {
				return err
			}
		}
		before := ""
		if oldSID != nil {
			before = *oldSID
		}
		changed["student_id"] = map[string]any{"before": before, "after": sid, "assignment_snapshots": snapshots}
	}
	if in.Prefix != nil {
		var old string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(prefix, '') FROM ta_profiles WHERE user_id = $1`, userID).Scan(&old); err != nil {
			return err
		}
		if old != *in.Prefix {
			if _, err := tx.Exec(ctx, `UPDATE ta_profiles SET prefix = $2 WHERE user_id = $1`, userID, *in.Prefix); err != nil {
				return err
			}
			changed["prefix"] = map[string]any{"before": old, "after": *in.Prefix}
		}
	}
	if in.NationalID != nil {
		if err := s.storeCitizenID(ctx, tx, userID, nid); err != nil {
			return err
		}
		changed["citizen_id_last4"] = nid[len(nid)-4:]
	}
	if len(changed) == 1 {
		return Invalid("ข้อมูลที่กรอกตรงกับข้อมูลเดิม ไม่มีอะไรเปลี่ยน")
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "ta_profile.identity_correct", Entity: "user", EntityID: userID.String(),
		After: changed, Note: in.Reason,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// The TA is told what changed and why — their own details were edited by
	// someone else. Fields are named, values are not, except the last four
	// digits of the citizen ID they can recognise. The date is in the title so
	// two corrections on different days never fold into one notice.
	if s.notify != nil {
		var what []string
		if v, ok := changed["student_id"].(map[string]any); ok {
			what = append(what, "รหัสนักศึกษาเป็น "+v["after"].(string))
		}
		if v, ok := changed["prefix"].(map[string]any); ok {
			what = append(what, "คำนำหน้าเป็น "+v["after"].(string))
		}
		if v, ok := changed["citizen_id_last4"].(string); ok {
			what = append(what, "เลขบัตรประจำตัวประชาชน (ลงท้าย "+v+")")
		}
		s.notify.Send(ctx, userID,
			"เจ้าหน้าที่แก้ไขข้อมูลส่วนตัวของท่าน วันที่ "+thaiDate(timeutil.Now()),
			"เจ้าหน้าที่ได้แก้ไข"+strings.Join(what, " และ")+" ตามที่ท่านแจ้ง เหตุผล "+in.Reason+
				" หากข้อมูลไม่ถูกต้อง กรุณาติดต่อเจ้าหน้าที่",
			"/ta/documents")
	}
	return nil
}

// RegenerateCreditorForm rebuilds the TA's signed creditor form with their
// CURRENT prefix, name and citizen ID, replaces the stored file, and returns
// it for staff to download on the spot. The bank details and the signature
// are not stored anywhere, so the form is not re-rendered from data: the
// TA's own PDF is the background and only the identity fields are repainted
// (pdfgen.PatchCreditor). The document keeps its row and review status — it
// is the same signed form with a staff-verified correction, not a new
// submission for review.
func (s *DocsService) RegenerateCreditorForm(ctx context.Context, actor, userID uuid.UUID, templatePath, fontDir string) ([]byte, string, error) {
	if s.pii == nil {
		return nil, "", errors.New("citizen id encryption is not configured (PII_ENC_KEY missing)")
	}
	var docID uuid.UUID
	var oldKey, filename string
	err := s.pool.QueryRow(ctx, `
		SELECT id, storage_key, filename FROM ta_documents
		 WHERE user_id = $1 AND kind = 'creditor_form' AND superseded_at IS NULL AND file_deleted_at IS NULL`,
		userID).Scan(&docID, &oldKey, &filename)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", Invalid("ผู้ช่วยสอนยังไม่มีแบบฟอร์มเจ้าหนี้ในระบบ ให้ผู้ช่วยสอนสร้างจากหน้าเอกสารก่อน")
	}
	if err != nil {
		return nil, "", err
	}
	rc, err := s.store.Open(oldKey)
	if err != nil {
		return nil, "", err
	}
	src, err := io.ReadAll(io.LimitReader(rc, maxDocBytes+1))
	rc.Close()
	if err != nil {
		return nil, "", err
	}

	var prefix, first, last string
	var sealed []byte
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(p.prefix,''), u.first_name, u.last_name, p.citizen_id_enc
		  FROM users u JOIN ta_profiles p ON p.user_id = u.id WHERE u.id = $1`, userID,
	).Scan(&prefix, &first, &last, &sealed); err != nil {
		return nil, "", err
	}
	if len(sealed) == 0 {
		return nil, "", Invalid("ยังไม่มีเลขบัตรประจำตัวประชาชนของผู้ช่วยสอนในระบบ")
	}
	nid, err := s.pii.Open(userID[:], sealed)
	if err != nil {
		return nil, "", err
	}
	body, err := pdfgen.PatchCreditor(pdfgen.CreditorPatchInput{
		SourcePDF: src, TemplatePath: templatePath, FontDir: fontDir,
		Prefix: prefix, FullName: first + " " + last, NationalID: string(nid),
	})
	if errors.Is(err, pdfgen.ErrNotGeneratedCreditorForm) {
		return nil, "", Invalid("แบบฟอร์มเจ้าหนี้ในระบบไม่ใช่ฉบับที่ระบบสร้าง จึงแก้ให้อัตโนมัติไม่ได้ ให้ผู้ช่วยสอนสร้างแบบฟอร์มใหม่ในหน้าเอกสารของตนเอง")
	}
	if err != nil {
		return nil, "", err
	}
	newKey, size, err := s.store.Save("ta_docs", filename, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "ta_profile.creditor_form_regenerate", Entity: "ta_document", EntityID: docID.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE ta_documents SET storage_key = $2, size_bytes = $3 WHERE id = $1`, docID, newKey, size)
			return err
		}); err != nil {
		_ = s.store.Delete(newKey)
		return nil, "", err
	}
	_ = s.store.Delete(oldKey)
	return body, filename, nil
}
