// data_export.go answers the PDPA "what do you have on me" question: everything
// a user is entitled to see about themselves, assembled into one bundle by
// ExportMyData. It deliberately reads straight off the tables it needs rather
// than depending on DocsService/SessionService — this is a read-only fact
// aggregator, not a place that should grow cross-service coupling.
package service

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type MyDataExport struct {
	Profile  MyDataProfile      `json:"profile"`
	Bank     *MyDataBank        `json:"ta_profile,omitempty"`
	Consent  *MyDataConsent     `json:"pdpa_consent,omitempty"`
	Sessions []MyDataSession    `json:"sessions"`
	Docs     []MyDataDoc        `json:"documents"`
	Activity []MyDataAuditEntry `json:"recent_activity"`
}

type MyDataProfile struct {
	Email       string   `json:"email"`
	Title       *string  `json:"title,omitempty"`
	FirstName   string   `json:"first_name"`
	LastName    string   `json:"last_name"`
	Phone       *string  `json:"phone,omitempty"`
	StudentID   *string  `json:"student_id,omitempty"`
	Department  *string  `json:"department,omitempty"`
	Roles       []string `json:"roles"`
	TOTPEnabled bool     `json:"totp_enabled"`
}

// MyDataBank never carries the full citizen ID — only the last 4 digits
// already shown elsewhere in the app. The full number is a separate,
// deliberately-friction-ful action: see DocsService.RevealCitizenID and the
// password-gated POST /me/citizen-id/reveal handler.
type MyDataBank struct {
	CitizenIDLast4 *string `json:"citizen_id_last4,omitempty"`
	Status         string  `json:"status"`
}

type MyDataConsent struct {
	Version     int16     `json:"version"`
	ConsentedAt time.Time `json:"consented_at"`
}

type MyDataSession struct {
	ID             uuid.UUID  `json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	LastActivityAt time.Time  `json:"last_activity_at"`
	IP             *string    `json:"ip,omitempty"`
	UserAgent      *string    `json:"user_agent,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	RevokeReason   *string    `json:"revoke_reason,omitempty"`
}

type MyDataDoc struct {
	Kind       string    `json:"kind"`
	Filename   string    `json:"filename"`
	Status     string    `json:"status"`
	UploadedAt time.Time `json:"uploaded_at"`
}

type MyDataAuditEntry struct {
	At       time.Time `json:"at"`
	Action   string    `json:"action"`
	Entity   string    `json:"entity"`
	EntityID *string   `json:"entity_id,omitempty"`
	IP       *string   `json:"ip,omitempty"`
}

// ExportMyData assembles everything the system holds on userID: identity,
// the encrypted citizen-ID's last 4 digits only, session/login history,
// document status, PDPA consent record, and their own recent audit trail
// (actions THEY took — auth.login, document uploads, etc; not actions taken
// about them by staff, which live in a separate part of audit_logs keyed by
// entity_id instead of actor_id).
func (s *UserService) ExportMyData(ctx context.Context, userID uuid.UUID) (*MyDataExport, error) {
	out := &MyDataExport{Sessions: []MyDataSession{}, Docs: []MyDataDoc{}, Activity: []MyDataAuditEntry{}}

	if err := s.pool.QueryRow(ctx,
		`SELECT email, title, first_name, last_name, phone, student_id, department, totp_enabled_at IS NOT NULL
		 FROM users WHERE id = $1`, userID).Scan(
		&out.Profile.Email, &out.Profile.Title, &out.Profile.FirstName, &out.Profile.LastName,
		&out.Profile.Phone, &out.Profile.StudentID, &out.Profile.Department, &out.Profile.TOTPEnabled,
	); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `SELECT role::text FROM user_roles WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err == nil {
			out.Profile.Roles = append(out.Profile.Roles, r)
		}
	}
	rows.Close()

	// ta_profiles.status is NOT NULL, so a scan error here only ever means "no
	// row yet" (a TA who hasn't opened the profile form) — nothing to export.
	var bank MyDataBank
	if err := s.pool.QueryRow(ctx,
		`SELECT status::text, citizen_id_last4 FROM ta_profiles WHERE user_id = $1`, userID,
	).Scan(&bank.Status, &bank.CitizenIDLast4); err == nil {
		out.Bank = &bank
	}

	var consent MyDataConsent
	if err := s.pool.QueryRow(ctx,
		`SELECT version, consented_at FROM pdpa_consents WHERE user_id = $1 ORDER BY consented_at DESC LIMIT 1`,
		userID,
	).Scan(&consent.Version, &consent.ConsentedAt); err == nil {
		out.Consent = &consent
	}

	sessRows, err := s.pool.Query(ctx,
		`SELECT id, created_at, last_activity_at, ip, user_agent, revoked_at, revoke_reason
		 FROM sessions WHERE user_id = $1 ORDER BY created_at DESC LIMIT 10`, userID)
	if err != nil {
		return nil, err
	}
	for sessRows.Next() {
		var sess MyDataSession
		if err := sessRows.Scan(&sess.ID, &sess.CreatedAt, &sess.LastActivityAt,
			&sess.IP, &sess.UserAgent, &sess.RevokedAt, &sess.RevokeReason); err != nil {
			sessRows.Close()
			return nil, err
		}
		out.Sessions = append(out.Sessions, sess)
	}
	sessRows.Close()

	docRows, err := s.pool.Query(ctx,
		`SELECT kind, filename, status::text, uploaded_at
		 FROM ta_documents WHERE user_id = $1 AND superseded_at IS NULL
		 ORDER BY uploaded_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	for docRows.Next() {
		var d MyDataDoc
		if err := docRows.Scan(&d.Kind, &d.Filename, &d.Status, &d.UploadedAt); err != nil {
			docRows.Close()
			return nil, err
		}
		out.Docs = append(out.Docs, d)
	}
	docRows.Close()

	actRows, err := s.pool.Query(ctx,
		`SELECT at, action, entity, entity_id, ip::text
		 FROM audit_logs WHERE actor_id = $1 ORDER BY at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	for actRows.Next() {
		var e MyDataAuditEntry
		if err := actRows.Scan(&e.At, &e.Action, &e.Entity, &e.EntityID, &e.IP); err != nil {
			actRows.Close()
			return nil, err
		}
		out.Activity = append(out.Activity, e)
	}
	actRows.Close()

	return out, nil
}
