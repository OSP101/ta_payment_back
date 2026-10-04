package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
)

// SendCredentials mails a new (or freshly reset) account its sign-in details:
// the address and the temporary password the officer is looking at in the
// one-time panel. Asked for by staff on 05/10/2026 so they no longer copy the
// password into a chat by hand.
//
// The server never keeps the plaintext, so the caller sends it back and it is
// checked against the stored hash. Together with must_change_password this
// means the endpoint can only ever deliver the account's OWN, still-unused
// temporary password to the account's OWN address: it cannot be used to mail
// arbitrary text, and once the person has set a password of their own it
// refuses.
//
// The password goes in the e-mail only. The notifications row the mailer
// writes carries the plain body, which never contains it.
func (s *UserService) SendCredentials(ctx context.Context, actor, id uuid.UUID, password string) error {
	if password == "" {
		return Invalid("ไม่พบรหัสผ่านชั่วคราวที่จะส่ง")
	}
	var (
		email   string
		hash    *string
		mustChg bool
		isTA    bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.email, u.password_hash, u.must_change_password,
		       EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'ta')
		  FROM users u
		 WHERE u.id = $1 AND u.is_active AND u.deleted_at IS NULL`, id).
		Scan(&email, &hash, &mustChg, &isTA)
	if errors.Is(err, pgx.ErrNoRows) {
		return &UserError{Status: 404, Msg: "ไม่พบบัญชีผู้ใช้ หรือบัญชีถูกปิดใช้งานแล้ว"}
	}
	if err != nil {
		return err
	}
	if !mustChg || hash == nil || !auth.CheckPassword(*hash, password) {
		return Conflict("รหัสผ่านชั่วคราวนี้ใช้ไม่ได้แล้ว (ผู้ใช้อาจตั้งรหัสผ่านใหม่ไปแล้ว หรือมีการรีเซ็ตรหัสผ่านอีกครั้ง) กรุณารีเซ็ตรหัสผ่านแล้วส่งใหม่")
	}

	title, body, layout := credentialsMailContent(email, password, isTA)
	if err := s.notify.SendLaidOutEmailOnly(ctx, id, title, body, "/login", true, layout); err != nil {
		return &UserError{Status: 502, Msg: "ส่งอีเมลไม่สำเร็จ กรุณาลองอีกครั้ง หรือคัดลอกรหัสผ่านส่งให้ผู้ใช้เอง"}
	}
	return s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "user.send_credentials",
		Entity: "user", EntityID: id.String(), After: map[string]string{"email": email}})
}

// credentialsMailContent is the sign-in letter. body is what the notifications
// table keeps, so it must never carry the password; the layout does.
func credentialsMailContent(email, password string, isTA bool) (title, body string, layout MailLayout) {
	title = "ข้อมูลสำหรับเข้าสู่ระบบ " + mailSystemName
	body = "เจ้าหน้าที่ได้ส่งข้อมูลสำหรับเข้าสู่ระบบครั้งแรกให้ทางอีเมล " + email +
		" แล้ว เมื่อเข้าสู่ระบบ ระบบจะให้ตั้งรหัสผ่านใหม่ทันที"
	layout = MailLayout{
		Intro: "บัญชีผู้ใช้งานของท่านในระบบเบิกจ่ายค่าตอบแทนผู้ช่วยสอนพร้อมใช้งานแล้ว " +
			"ท่านสามารถเข้าสู่ระบบครั้งแรกได้ด้วยข้อมูลดังต่อไปนี้",
		Facts:     []MailFact{{Label: "ชื่อผู้ใช้ (อีเมล)", Value: email}},
		Highlight: &MailFact{Label: "รหัสผ่านชั่วคราว", Value: password},
		After: "เมื่อเข้าสู่ระบบครั้งแรก ระบบจะให้ท่านตั้งรหัสผ่านใหม่ทันที รหัสผ่านชั่วคราวนี้จะใช้ไม่ได้อีกหลังจากนั้น\n" +
			"กรุณาเก็บรักษารหัสผ่านไว้เป็นความลับ และไม่ส่งต่ออีเมลฉบับนี้ให้ผู้อื่น " +
			"หากท่านไม่ได้เป็นผู้ขอใช้งานระบบ กรุณาแจ้งเจ้าหน้าที่",
		ButtonLabel: "เข้าสู่ระบบ",
	}
	if isTA {
		layout.Guides = []MailGuide{guideTAStart, guideTAAll}
	}
	return title, body, layout
}
