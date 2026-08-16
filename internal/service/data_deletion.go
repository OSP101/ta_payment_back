// data_deletion.go implements the PDPA "erase my data" request workflow — see
// migration 0093 and docs/SECURITY.md's data-deletion section. A TA submits a
// request; an admin reviews it. Approval always deactivates the account and
// scrubs everything with no retention basis (2FA, avatar, sessions, document
// blobs). Whether the citizen ID and users.deleted_at ALSO get cleared
// depends on HasPaymentHistory — see ReviewDeletion's own comment for why
// that split exists, not something this file decides unilaterally.
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/storage"
)

type DataDeletionService struct {
	pool     *pgxpool.Pool
	aud      *audit.Auditor
	docs     *DocsService
	users    *UserService
	sessions *SessionService
	notify   *NotifyService
	store    storage.Store
}

type DataDeletionRequest struct {
	ID          uuid.UUID  `json:"id"`
	UserID      uuid.UUID  `json:"user_id"`
	Reason      *string    `json:"reason,omitempty"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy  *uuid.UUID `json:"reviewed_by,omitempty"`
	ReviewNote  *string    `json:"review_note,omitempty"`
	ExecutedAt  *time.Time `json:"executed_at,omitempty"`
}

// DataDeletionRequestForReview is what the staff queue shows — the request
// plus enough of the requester's identity to review it without a second
// round trip, and the payment-history hint that decides which erasure branch
// approval will take.
type DataDeletionRequestForReview struct {
	DataDeletionRequest
	RequesterEmail    string `json:"requester_email"`
	RequesterName     string `json:"requester_name"`
	HasPaymentHistory bool   `json:"has_payment_history"`
}

// RequestDeletion records a TA's ask to have their data erased. The partial
// unique index ix_data_deletion_requests_one_pending (migration 0093) makes a
// second submission while one is already pending fail as a plain 23505
// unique-violation — internal/handler/middleware.go's ErrorHandler already
// turns that into a friendly Thai 409 with no special-casing needed here.
func (s *DataDeletionService) RequestDeletion(ctx context.Context, userID uuid.UUID, reason string) error {
	var reasonArg any
	if r := strings.TrimSpace(reason); r != "" {
		reasonArg = r
	}
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO data_deletion_requests (user_id, reason) VALUES ($1,$2) RETURNING id`,
		userID, reasonArg).Scan(&id); err != nil {
		return err
	}
	return s.aud.Log(ctx, audit.Entry{
		ActorID: &userID, Action: "ta_deletion_request.submit",
		Entity: "data_deletion_request", EntityID: id.String(), Note: reason,
	})
}

// MyDeletionRequest returns the TA's own most recent request (any status), or
// nil if they have never submitted one — lets /account/my-data show a live
// status instead of the submit form once a request exists.
func (s *DataDeletionService) MyDeletionRequest(ctx context.Context, userID uuid.UUID) (*DataDeletionRequest, error) {
	var r DataDeletionRequest
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, reason, status, requested_at, reviewed_at, reviewed_by, review_note, executed_at
		 FROM data_deletion_requests WHERE user_id = $1 ORDER BY requested_at DESC LIMIT 1`, userID,
	).Scan(&r.ID, &r.UserID, &r.Reason, &r.Status, &r.RequestedAt,
		&r.ReviewedAt, &r.ReviewedBy, &r.ReviewNote, &r.ExecutedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// HasPaymentHistory reports whether userID has ever had an approved worklog
// or an appointment-order line — the two places actual payment eligibility is
// recorded in this schema (there is no separate "paid" status; 'approved' on
// work_logs is the terminal payment-eligible state, per internal/service/
// work_log.go). ReviewDeletion uses this to decide how much can be erased.
func (s *DataDeletionService) HasPaymentHistory(ctx context.Context, userID uuid.UUID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT
			EXISTS(
				SELECT 1 FROM work_logs w
				JOIN ta_request_assignments a ON a.id = w.assignment_id
				WHERE a.ta_id = $1 AND w.status = 'approved'
			)
			OR EXISTS(
				SELECT 1 FROM appointment_order_items WHERE ta_id = $1
			)`, userID).Scan(&ok)
	return ok, err
}

// ListDeletionRequests is the staff review queue. status filters to one
// state ("pending", "approved", "rejected"); empty returns all.
func (s *DataDeletionService) ListDeletionRequests(ctx context.Context, status string) ([]DataDeletionRequestForReview, error) {
	q := `
		SELECT r.id, r.user_id, r.reason, r.status, r.requested_at,
		       r.reviewed_at, r.reviewed_by, r.review_note, r.executed_at,
		       u.email, COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,''),
		       EXISTS(
		           SELECT 1 FROM work_logs w
		           JOIN ta_request_assignments a ON a.id = w.assignment_id
		           WHERE a.ta_id = r.user_id AND w.status = 'approved'
		       ) OR EXISTS(
		           SELECT 1 FROM appointment_order_items WHERE ta_id = r.user_id
		       )
		FROM data_deletion_requests r
		JOIN users u ON u.id = r.user_id`
	args := []any{}
	if status != "" {
		q += ` WHERE r.status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY r.requested_at DESC`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DataDeletionRequestForReview{}
	for rows.Next() {
		var r DataDeletionRequestForReview
		if err := rows.Scan(&r.ID, &r.UserID, &r.Reason, &r.Status, &r.RequestedAt,
			&r.ReviewedAt, &r.ReviewedBy, &r.ReviewNote, &r.ExecutedAt,
			&r.RequesterEmail, &r.RequesterName, &r.HasPaymentHistory); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ReviewDeletion is the admin decision on one request.
//
// Reject: requires a note (same "reason required on reject" rule
// DocsService.ReviewProfile already applies), just marks the request and
// notifies the TA — nothing about their data changes.
//
// Approve, in ONE transaction:
//  1. re-check HasPaymentHistory (the hint shown to staff could be stale by
//     the time they click approve)
//  2. deactivate the account and clear the avatar column (blob deleted from
//     storage AFTER commit, same rule SetAvatar's own doc comment states —
//     a failed update must never leave the row pointing at a deleted file)
//  3. clear the 2FA secret/recovery codes — identical SQL to MFAService's
//     AdminReset, just from this code path instead of that one
//  4. ONLY if no payment history: also clear ta_profiles' citizen ID columns
//     and set users.deleted_at — the first real writer of that column (every
//     read in this codebase already filters WHERE deleted_at IS NULL, see
//     Get/List/FindByEmail and AccountGuard, but nothing has set it before)
//  5. write both "ta_deletion_request.approve" and a separate
//     "user.pdpa_erasure" audit row whose Note states which branch ran — the
//     durable, staff-visible proof of exactly what was and wasn't erased
//
// Session revocation, document-blob scrubbing, and the outcome notification
// happen AFTER commit as best-effort follow-ups — same "physical side effects
// happen after the row commits" convention as the avatar blob delete.
func (s *DataDeletionService) ReviewDeletion(ctx context.Context, actor, requestID uuid.UUID, approve bool, note string) error {
	var userID uuid.UUID
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT user_id, status FROM data_deletion_requests WHERE id = $1`, requestID,
	).Scan(&userID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status != "pending" {
		return Conflict("คำขอนี้ถูกดำเนินการไปแล้ว")
	}

	if !approve {
		if strings.TrimSpace(note) == "" {
			return Invalid("กรุณาระบุเหตุผลที่ปฏิเสธคำขอ")
		}
		if err := writeAudited(ctx, s.pool, s.aud,
			audit.Entry{ActorID: &actor, Action: "ta_deletion_request.reject",
				Entity: "data_deletion_request", EntityID: requestID.String(), Note: note},
			func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`UPDATE data_deletion_requests
					 SET status='rejected', reviewed_at=NOW(), reviewed_by=$2, review_note=$3
					 WHERE id=$1`, requestID, actor, note)
				return err
			}); err != nil {
			return err
		}
		if s.notify != nil {
			s.notify.Send(ctx, userID, "คำขอลบข้อมูลถูกปฏิเสธ",
				fmt.Sprintf("เหตุผล: %s", note), "/account/my-data")
		}
		return nil
	}

	hasPayment, err := s.HasPaymentHistory(ctx, userID)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var avatarKey *string
	if err := tx.QueryRow(ctx, `SELECT avatar_key FROM users WHERE id=$1`, userID).Scan(&avatarKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE users SET
			is_active = FALSE,
			avatar_key = NULL, avatar_updated_at = NULL,
			totp_secret_enc = NULL, totp_pending_secret_enc = NULL,
			totp_enabled_at = NULL, totp_last_step = NULL, totp_key_version = NULL,
			updated_at = NOW()
		WHERE id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}

	branch := "partial: payment history retained"
	if !hasPayment {
		if _, err := tx.Exec(ctx, `
			UPDATE ta_profiles SET citizen_id_enc = NULL, citizen_id_last4 = NULL, citizen_id_key_version = NULL
			WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET deleted_at = NOW() WHERE id = $1`, userID); err != nil {
			return err
		}
		branch = "full: no payment history, citizen ID cleared"
	}

	if _, err := tx.Exec(ctx, `
		UPDATE data_deletion_requests
		SET status='approved', reviewed_at=NOW(), reviewed_by=$2, review_note=$3, executed_at=NOW()
		WHERE id=$1`, requestID, actor, note); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "ta_deletion_request.approve",
		Entity: "data_deletion_request", EntityID: requestID.String(), Note: note,
	}); err != nil {
		return err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "user.pdpa_erasure", Entity: "user", EntityID: userID.String(), Note: branch,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if s.sessions != nil {
		if err := s.sessions.RevokeAllForUser(ctx, userID, "data_deletion_approved"); err != nil {
			log.Printf("data_deletion: revoke sessions for %s failed: %v", userID, err)
		}
	}
	if s.docs != nil {
		if err := s.docs.ScrubUserDocuments(ctx, actor, userID); err != nil {
			log.Printf("data_deletion: scrub documents for %s failed: %v", userID, err)
		}
	}
	if avatarKey != nil && *avatarKey != "" && s.store != nil {
		if err := s.store.Delete(*avatarKey); err != nil && !os.IsNotExist(err) {
			log.Printf("data_deletion: delete avatar %s failed: %v", *avatarKey, err)
		}
	}
	if s.notify != nil {
		msg := "บัญชีของคุณถูกปิดใช้งานและข้อมูลส่วนบุคคลที่ไม่จำเป็นถูกลบแล้วตามคำขอ"
		if hasPayment {
			msg += " ข้อมูลที่เกี่ยวข้องกับการเบิกจ่าย (เลขบัตรประชาชน ประวัติชั่วโมงสอน) ยังคงถูกเก็บไว้ตามข้อบังคับทางบัญชี/ภาษี"
		} else {
			msg += " รวมถึงเลขบัตรประชาชนที่เคยจัดเก็บไว้"
		}
		s.notify.Send(ctx, userID, "คำขอลบข้อมูลได้รับการอนุมัติ", msg, "/account/my-data")
	}
	return nil
}
