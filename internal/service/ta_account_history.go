package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
)

// MarkReturning updates only the verified identity's seniority. It never changes
// credentials, first-term stamps, enrollment, requests, or the delivery manifest.
func (s *TAAccountBatch) MarkReturning(ctx context.Context, actor uuid.UUID, batch string, in []TAAccountInput, apply bool) ([]TAAccountRecord, error) {
	if err := validateTAAccountInputs(in); err != nil {
		return nil, err
	}
	for _, r := range in {
		if r.Seniority != "returning" {
			return nil, errors.New("mark-returning requires explicit returning history on every input row")
		}
	}
	if err := s.CheckActor(ctx, actor); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	result := make([]TAAccountRecord, 0, len(in))
	for _, r := range in {
		rec := TAAccountRecord{TAAccountInput: r, Status: "not_found"}
		query := `SELECT u.id,u.email,COALESCE(u.student_id,''),u.is_active,EXISTS(SELECT 1 FROM user_roles ur WHERE ur.user_id=u.id AND ur.role='ta'),u.ta_seniority_override FROM users u WHERE u.deleted_at IS NULL AND (lower(u.email)=lower($1) OR regexp_replace(COALESCE(u.student_id,''),'[^0-9]','','g')=replace($2,'-','')) ORDER BY u.id`
		if apply {
			query += " FOR UPDATE OF u"
		}
		rows, err := tx.Query(ctx, query, r.Email, r.StudentID)
		if err != nil {
			return nil, err
		}
		count := 0
		var id uuid.UUID
		var email, sid string
		var active, isTA bool
		var override *string
		for rows.Next() {
			count++
			if err = rows.Scan(&id, &email, &sid, &active, &isTA, &override); err != nil {
				rows.Close()
				return nil, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case count == 0:
			rec.Detail = "account not found; no account created"
		case count != 1 || !strings.EqualFold(email, r.Email) || strings.ReplaceAll(sid, "-", "") != strings.ReplaceAll(r.StudentID, "-", "") || !active || !isTA:
			rec.Status = "conflict"
			rec.Detail = "student ID/email must identify one active TA account"
		case override != nil && *override == "new":
			rec.Status = "conflict"
			rec.Detail = "explicit new override requires staff review"
		case override != nil && *override == "returning":
			rec.Status = "already_returning"
			rec.UserID = id
		default:
			rec.Status = "ready_returning"
			rec.UserID = id
			if apply {
				if _, err = tx.Exec(ctx, `UPDATE users SET ta_seniority_override='returning',updated_at=NOW() WHERE id=$1`, id); err != nil {
					return nil, err
				}
				if err = s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "user.ta_history.import", Entity: "user", EntityID: id.String(), Before: map[string]any{"ta_seniority_override": override}, After: map[string]any{"ta_seniority_override": "returning", "batch": batch, "history_source": r.HistorySource, "student_id": r.StudentID}, Note: "verified legacy TA history; no request or mail"}); err != nil {
					return nil, err
				}
				rec.Status = "marked_returning"
			}
		}
		result = append(result, rec)
	}
	if apply {
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
	}
	return result, nil
}
