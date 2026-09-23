package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/mail"
)

// MailContact is the contact block at the foot of every notification e-mail,
// editable under ตั้งค่า > อีเมลแจ้งเตือน (table mail_settings, one row).
type MailContact struct {
	Heading string `json:"contact_heading"`
	Unit    string `json:"contact_unit"`
	// Detail is free lines under the unit: building/room, phone, e-mail.
	Detail string `json:"contact_detail"`
}

// defaultMailContact is used when the row is missing (a database the
// migration has not reached, or tests that build NotifyService by hand).
var defaultMailContact = MailContact{
	Heading: "หากมีข้อสงสัยเพิ่มเติม สามารถติดต่อได้ที่",
	Unit:    "เจ้าหน้าที่" + mailSystemName + " " + mailCollegeName,
}

// MailSettings is what the settings page reads.
type MailSettings struct {
	MailContact
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy *string    `json:"updated_by_name,omitempty"`
}

type MailSettingsService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

// loadMailContact reads the footer. Any failure falls back to the default so
// a settings problem can never stop a notification from going out.
func loadMailContact(ctx context.Context, q querier) MailContact {
	var c MailContact
	if err := q.QueryRow(ctx,
		`SELECT contact_heading, contact_unit, contact_detail FROM mail_settings WHERE id`).
		Scan(&c.Heading, &c.Unit, &c.Detail); err != nil {
		return defaultMailContact
	}
	return c
}

func (s *MailSettingsService) Get(ctx context.Context) (*MailSettings, error) {
	out := &MailSettings{}
	err := s.pool.QueryRow(ctx, `
		SELECT m.contact_heading, m.contact_unit, m.contact_detail, m.updated_at,
		       NULLIF(TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, '')), '')
		  FROM mail_settings m LEFT JOIN users u ON u.id = m.updated_by
		 WHERE m.id`).Scan(&out.Heading, &out.Unit, &out.Detail, &out.UpdatedAt, &out.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		out.MailContact = defaultMailContact
		return out, nil
	}
	return out, err
}

// normalizeMailContact trims and bounds what staff typed. Detail keeps its
// line breaks (one line per contact channel) but loses blank runs.
func normalizeMailContact(in MailContact) (MailContact, error) {
	out := MailContact{
		Heading: plainPunct(strings.TrimSpace(in.Heading)),
		Unit:    plainPunct(strings.TrimSpace(in.Unit)),
	}
	var lines []string
	for _, ln := range strings.Split(strings.ReplaceAll(in.Detail, "\r\n", "\n"), "\n") {
		if ln = plainPunct(strings.TrimSpace(ln)); ln != "" {
			lines = append(lines, ln)
		}
	}
	out.Detail = strings.Join(lines, "\n")

	switch {
	case out.Heading == "":
		return out, Invalid("กรุณาระบุข้อความหัวข้อการติดต่อ")
	case out.Unit == "":
		return out, Invalid("กรุณาระบุหน่วยงานที่ติดต่อ")
	case utf8.RuneCountInString(out.Heading) > 200:
		return out, Invalid("ข้อความหัวข้อยาวเกิน 200 ตัวอักษร")
	case utf8.RuneCountInString(out.Unit) > 300:
		return out, Invalid("ชื่อหน่วยงานยาวเกิน 300 ตัวอักษร")
	case len(lines) > 6:
		return out, Invalid("รายละเอียดการติดต่อได้ไม่เกิน 6 บรรทัด")
	case utf8.RuneCountInString(out.Detail) > 600:
		return out, Invalid("รายละเอียดการติดต่อยาวเกิน 600 ตัวอักษร")
	}
	return out, nil
}

func (s *MailSettingsService) Update(ctx context.Context, actor uuid.UUID, in MailContact) (*MailSettings, error) {
	c, err := normalizeMailContact(in)
	if err != nil {
		return nil, err
	}
	before := loadMailContact(ctx, s.pool)
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "mail_settings.update", Entity: "mail_settings", EntityID: "contact",
			Before: before, After: c},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO mail_settings (id, contact_heading, contact_unit, contact_detail, updated_at, updated_by)
				VALUES (TRUE, $1, $2, $3, NOW(), $4)
				ON CONFLICT (id) DO UPDATE SET
				  contact_heading = EXCLUDED.contact_heading,
				  contact_unit    = EXCLUDED.contact_unit,
				  contact_detail  = EXCLUDED.contact_detail,
				  updated_at      = NOW(),
				  updated_by      = EXCLUDED.updated_by`,
				c.Heading, c.Unit, c.Detail, actor)
			return err
		}); err != nil {
		return nil, err
	}
	return s.Get(ctx)
}

// Preview renders a sample notification with the given (possibly unsaved)
// contact block, for the settings page to show before staff save. The logo
// is inlined as a data URI because a browser cannot resolve the cid: link
// that the real e-mail uses.
func (s *MailSettingsService) Preview(in MailContact) (string, error) {
	c, err := normalizeMailContact(in)
	if err != nil {
		return "", err
	}
	html := renderMailHTML(mailContent{
		Title:     "ตัวอย่างอีเมลแจ้งเตือน",
		Body:      "บันทึกเวลาปฏิบัติงานรายวิชา CP353004 การพัฒนาซอฟต์แวร์ ประจำเดือนกันยายน 2569 ของท่านได้รับการอนุมัติจากอาจารย์ผู้สอนแล้ว",
		Link:      "https://tas.coco.kku.ac.th/ta",
		Recipient: recipientName("นางสาว", "สมหญิง", "รักเรียน"),
		Closing:   closingInform,
		Contact:   c,
	})
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString(mail.LogoPNG())
	return strings.ReplaceAll(html, "cid:"+mail.LogoCID, logo), nil
}
