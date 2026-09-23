package service

import (
	"strconv"
	"strings"
	"time"

	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/timeutil"
)

// notify_mail.go lays a notification out as an e-mail.
//
// The layout follows the university's own notices (KKU Digital Academic
// Document): a solid header in the unit's colour carrying its emblem and
// name, "เรียน <name>" in the brand colour, the letter body, then optional
// blocks: an info box with a coloured left edge, a dashed highlight for the
// one value that matters, a titled table, and a button. A grey footer says
// whom to contact. Colours and names are the College of Computing's.
//
// Table layout and inline styles only, since that is all mail clients
// reliably honour. Every value is HTML-escaped here.

// Brand colours, from the site's design tokens (ta_payment_front
// app/globals.css --brand / --brand-hover / --brand-soft).
const (
	mailBrand     = "#0776BC"
	mailBrandDark = "#065f97"
	mailBrandSoft = "#f3f8fc"
	mailInk       = "#333333"
	mailMuted     = "#6b7280"
	mailBorder    = "#e5e7eb"
	mailGrey      = "#f8f9fa"
	mailFont      = "font-family:Sarabun,'Leelawadee UI',Tahoma,Arial,sans-serif"
)

const (
	mailUnitTH      = "วิทยาลัยการคอมพิวเตอร์"
	mailUnitEN      = "College of Computing, Khon Kaen University"
	mailSystemName  = "ระบบเบิกจ่ายค่าตอบแทนผู้ช่วยสอน (COCO TAS)"
	mailCollegeName = "วิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น"
	mailButtonLabel = "เข้าสู่ระบบเพื่อดูรายละเอียด"
	mailAutoNotice  = "อีเมลฉบับนี้ส่งโดยระบบอัตโนมัติ กรุณาอย่าตอบกลับอีเมลฉบับนี้"
)

// mailNow is the clock the copyright year reads; a variable so tests can pin it.
var mailNow = time.Now

// mailCopyright is the line under the footer, worded as the site's own
// footer is ("© 2026 College of Computing, Khon Kaen University", see
// ta_payment_front app/login/LoginForm.tsx). English only, by request.
func mailCopyright() string {
	return "© " + strconv.Itoa(mailNow().In(timeutil.Bangkok).Year()) + " " + mailUnitEN + ". All rights reserved."
}

// MailFact is one "label : value" line.
type MailFact struct{ Label, Value string }

// MailTable is a small table, e.g. a lecturer's courses.
type MailTable struct {
	Title string
	Head  []string
	Rows  [][]string
	// Align per column ("left" when empty or shorter than Head).
	Align []string
}

// MailLayout is the structured part of an e-mail. The zero value is a plain
// letter: the notification body under the salutation.
type MailLayout struct {
	// Intro replaces the notification body as the letter's opening. Empty
	// means "use the body".
	Intro     string
	Facts     []MailFact
	Highlight *MailFact
	Table     *MailTable
	// After is text below the table, before the closing.
	After       string
	ButtonLabel string
}

// mailContent is everything one notification e-mail shows.
type mailContent struct {
	Title, Body, Link string
	Recipient         string // "ผศ. ดร.สมชาย ใจดี" or "คุณสมชาย ใจดี"
	Closing           string
	Layout            MailLayout
	// Contact is the footer block; zero value means defaultMailContact.
	Contact MailContact
}

func (m mailContent) contact() MailContact {
	if m.Contact.Heading == "" && m.Contact.Unit == "" {
		return defaultMailContact
	}
	return m.Contact
}

// recipientName is how the letter addresses its reader: the title the system
// holds for them (TA prefix, else the account's academic title) written
// straight onto the first name, as Thai letters do; "คุณ" when there is none.
func recipientName(prefix, first, last string) string {
	name := strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
	if name == "" {
		return "ผู้ใช้งานระบบ"
	}
	if p := strings.TrimSpace(prefix); p != "" {
		return p + name
	}
	return "คุณ" + name
}

// plainPunct swaps the typographic marks that read as machine-written in a
// Thai official notice (em/en dashes, middle dots, bullets, arrows, curly
// quotes) for plain ones. Most message text is written without them already;
// this catches what arrives inside dynamic values such as reasons.
func plainPunct(s string) string {
	r := strings.NewReplacer(
		" — ", " ", "—", " ", " – ", " ", "–", "-",
		" · ", " ", "·", " ", "• ", "", "•", "",
		" → ", " ถึง ", "→", " ถึง ",
		"“", "\"", "”", "\"", "‘", "'", "’", "'", "…", "...",
	)
	s = r.Replace(s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

func mailEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
}

// isListLine reports whether a line is a numbered item ("1. ...").
func isListLine(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' '
}

// mailParagraphs turns plain text into letter paragraphs: a blank line
// separates paragraphs, and a paragraph's first line is indented as in a Thai
// letter. Numbered lines become hanging-indent items. Explicit markup rather
// than white-space:pre-wrap, which Outlook ignores.
func mailParagraphs(text string) string {
	var b strings.Builder
	for _, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		lines := strings.Split(strings.TrimSpace(para), "\n")
		if len(lines) == 1 && lines[0] == "" {
			continue
		}
		var prose []string
		flush := func() {
			if len(prose) == 0 {
				return
			}
			b.WriteString(`<p style="margin:0 0 14px;text-indent:2em">` + mailEsc(strings.Join(prose, " ")) + `</p>`)
			prose = nil
		}
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if isListLine(ln) {
				flush()
				dot := strings.Index(ln, ". ")
				b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 6px 2em"><tr>` +
					`<td style="vertical-align:top;padding-right:8px;white-space:nowrap">` + mailEsc(ln[:dot+1]) + `</td>` +
					`<td style="vertical-align:top">` + mailEsc(ln[dot+2:]) + `</td></tr></table>`)
				continue
			}
			prose = append(prose, ln)
		}
		flush()
	}
	return b.String()
}

func renderMailHTML(m mailContent) string {
	L := m.Layout
	intro := m.Body
	if L.Intro != "" {
		intro = L.Intro
	}
	button := L.ButtonLabel
	if button == "" {
		button = mailButtonLabel
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="th"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + mailEsc(m.Title) + `</title></head>`)
	b.WriteString(`<body style="margin:0;padding:0;background:#ffffff">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#ffffff"><tr><td align="center" style="padding:24px 12px">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:600px;` + mailFont + `;color:` + mailInk + `">`)

	// Header: emblem and unit name on the brand colour.
	b.WriteString(`<tr><td align="center" style="background:` + mailBrand + `;border-radius:8px 8px 0 0;padding:32px 24px 28px">`)
	b.WriteString(`<img src="cid:` + mail.LogoCID + `" width="56" height="56" alt="" style="display:block;border:0;width:56px;height:56px;margin:0 auto 14px">`)
	b.WriteString(`<div style="color:#ffffff;font-size:24px;font-weight:bold;line-height:1.4">` + mailUnitTH + `</div>`)
	b.WriteString(`<div style="color:#ffffff;font-size:15px;line-height:1.6;opacity:0.9">` + mailUnitEN + `</div>`)
	b.WriteString(`</td></tr>`)

	// Letter.
	b.WriteString(`<tr><td style="padding:32px 30px 8px;font-size:15px;line-height:1.8">`)
	b.WriteString(`<div style="margin:0 0 20px;font-size:18px;font-weight:bold;color:` + mailBrandDark + `">เรียน ` + mailEsc(m.Recipient) + `</div>`)
	b.WriteString(mailParagraphs(intro))

	if len(L.Facts) > 0 {
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin:10px 0 24px"><tr><td style="background:` + mailBrandSoft + `;border-left:4px solid ` + mailBrand + `;border-radius:0 6px 6px 0;padding:14px 20px">`)
		for _, f := range L.Facts {
			b.WriteString(`<div style="margin:6px 0"><b>` + mailEsc(f.Label) + ` :</b> ` + mailEsc(f.Value) + `</div>`)
		}
		b.WriteString(`</td></tr></table>`)
	}

	if L.Highlight != nil {
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin:0 0 28px"><tr><td align="center" style="border:1px dashed ` + mailBrand + `;border-radius:6px;background:` + mailBrandSoft + `;padding:20px 16px">`)
		b.WriteString(`<div style="font-weight:bold">` + mailEsc(L.Highlight.Label) + `</div>`)
		b.WriteString(`<div style="margin-top:6px;font-size:22px;font-weight:bold;color:` + mailBrandDark + `;line-height:1.5">` + mailEsc(L.Highlight.Value) + `</div>`)
		b.WriteString(`</td></tr></table>`)
	}

	if t := L.Table; t != nil && len(t.Rows) > 0 {
		if t.Title != "" {
			b.WriteString(`<div style="margin:0 0 12px;border-left:3px solid ` + mailBrand + `;padding-left:10px;font-size:16px;font-weight:bold;color:` + mailBrandDark + `;line-height:1.5">` + mailEsc(t.Title) + `</div>`)
		}
		align := func(i int) string {
			if i < len(t.Align) && t.Align[i] != "" {
				return t.Align[i]
			}
			return "left"
		}
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="border:1px solid ` + mailBorder + `;border-collapse:collapse;margin:0 0 24px;font-size:14px">`)
		b.WriteString(`<tr>`)
		for i, h := range t.Head {
			b.WriteString(`<th align="` + align(i) + `" style="background:#f3f4f6;padding:10px 14px;border-bottom:1px solid ` + mailBorder + `;font-weight:bold;white-space:nowrap">` + mailEsc(h) + `</th>`)
		}
		b.WriteString(`</tr>`)
		for _, row := range t.Rows {
			b.WriteString(`<tr>`)
			for i, cell := range row {
				b.WriteString(`<td align="` + align(i) + `" style="padding:12px 14px;border-bottom:1px solid ` + mailBorder + `;vertical-align:top">` + mailEsc(cell) + `</td>`)
			}
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</table>`)
	}

	if L.After != "" {
		b.WriteString(mailParagraphs(L.After))
	}
	b.WriteString(`<p style="margin:0 0 8px;text-indent:2em">` + mailEsc(m.Closing) + `</p>`)
	b.WriteString(`</td></tr>`)

	// Button, then the address written out for when it does not work.
	if m.Link != "" {
		b.WriteString(`<tr><td align="center" style="padding:16px 30px 8px">`)
		b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0"><tr><td style="background:` + mailBrandDark + `;border-radius:6px">`)
		b.WriteString(`<a href="` + mailEsc(m.Link) + `" style="display:inline-block;padding:12px 28px;color:#ffffff;text-decoration:none;font-size:15px;font-weight:bold;` + mailFont + `">` + mailEsc(button) + `</a>`)
		b.WriteString(`</td></tr></table></td></tr>`)
		b.WriteString(`<tr><td style="padding:16px 30px 28px">`)
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td style="background:` + mailGrey + `;border:1px solid #eef0f2;border-radius:6px;padding:16px 20px;font-size:14px;line-height:1.7">`)
		b.WriteString(`<div>หากกดปุ่มไม่ได้ สามารถเข้าสู่ระบบได้ที่</div>`)
		b.WriteString(`<a href="` + mailEsc(m.Link) + `" style="color:` + mailBrand + `;word-break:break-all">` + mailEsc(m.Link) + `</a>`)
		b.WriteString(`</td></tr></table></td></tr>`)
	} else {
		b.WriteString(`<tr><td style="padding:0 0 20px"></td></tr>`)
	}

	// Footer: who sent this and whom to ask.
	b.WriteString(`<tr><td style="background:` + mailGrey + `;border-top:1px solid #eef0f2;border-radius:0 0 8px 8px;padding:22px 30px;font-size:13px;line-height:1.7;color:#4b5563">`)
	c := m.contact()
	b.WriteString(`<div style="font-weight:bold;color:` + mailBrandDark + `;font-size:14px">` + mailEsc(c.Heading) + `</div>`)
	b.WriteString(`<div>` + mailEsc(c.Unit) + `</div>`)
	if c.Detail != "" {
		b.WriteString(`<div>` + strings.ReplaceAll(mailEsc(c.Detail), "\n", "<br>") + `</div>`)
	}
	b.WriteString(`<div style="margin-top:8px;color:` + mailMuted + `">` + mailAutoNotice + `</div>`)
	b.WriteString(`</td></tr>`)

	// Copyright, outside the card.
	b.WriteString(`<tr><td align="center" style="padding:18px 30px 0;font-size:12px;line-height:1.7;color:#9ca3af">` +
		mailEsc(mailCopyright()) + `</td></tr>`)

	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}

// renderMailText is the plain-text alternative of the same letter.
func renderMailText(m mailContent) string {
	L := m.Layout
	intro := m.Body
	if L.Intro != "" {
		intro = L.Intro
	}
	var b strings.Builder
	b.WriteString(mailUnitTH + "\n" + mailUnitEN + "\n\n")
	b.WriteString(m.Title + "\n\n")
	b.WriteString("เรียน " + m.Recipient + "\n\n")
	b.WriteString(strings.TrimSpace(intro) + "\n\n")
	for _, f := range L.Facts {
		b.WriteString(f.Label + " : " + f.Value + "\n")
	}
	if len(L.Facts) > 0 {
		b.WriteString("\n")
	}
	if L.Highlight != nil {
		b.WriteString(L.Highlight.Label + " " + L.Highlight.Value + "\n\n")
	}
	if t := L.Table; t != nil && len(t.Rows) > 0 {
		if t.Title != "" {
			b.WriteString(t.Title + "\n")
		}
		for i, row := range t.Rows {
			b.WriteString(strconv.Itoa(i+1) + ". " + strings.Join(row, " ") + "\n")
		}
		b.WriteString("\n")
	}
	if L.After != "" {
		b.WriteString(strings.TrimSpace(L.After) + "\n\n")
	}
	b.WriteString(m.Closing + "\n\n")
	if m.Link != "" {
		label := L.ButtonLabel
		if label == "" {
			label = mailButtonLabel
		}
		b.WriteString(label + ": " + m.Link + "\n\n")
	}
	c := m.contact()
	b.WriteString(c.Heading + "\n" + c.Unit + "\n")
	if c.Detail != "" {
		b.WriteString(c.Detail + "\n")
	}
	b.WriteString(mailAutoNotice + "\n\n")
	b.WriteString(mailCopyright() + "\n")
	return b.String()
}
