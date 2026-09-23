package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Helpers for the wording of notifications. The notices are written as short
// Thai official letters (the e-mail adds the salutation and closing, see
// notify.go), so dates are spelled out, times use the official "10.30 น."
// form, and people are named with the title the system holds for them.

// thaiLongDateISO renders "2026-09-23" (a date or the date part of a
// timestamp) as "23 กันยายน 2569". Anything unparseable is returned unchanged.
func thaiLongDateISO(s string) string {
	if len(s) < 10 {
		return s
	}
	d, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return s
	}
	return fmt.Sprintf("%d %s %d", d.Day(), thaiMonthNames[d.Month()], d.Year()+543)
}

// thaiClock renders "13:05" or "13:05:00" as "13.05", the form Thai official
// documents use before "น.".
func thaiClock(s string) string {
	if len(s) >= 5 && s[2] == ':' {
		return s[:2] + "." + s[3:5]
	}
	return s
}

// thaiTimeRange renders a start/end pair as "เวลา 09.00 ถึง 12.00 น.".
func thaiTimeRange(start, end string) string {
	return "เวลา " + thaiClock(start) + " ถึง " + thaiClock(end) + " น."
}

// personNameSQL is the display name with its title: a TA's prefix from their
// profile (นาย, นางสาว) or else the account's academic title (ผศ. ดร.),
// written onto the first name as Thai documents do. Needs users aliased u and
// ta_profiles LEFT JOINed as tp.
const personNameSQL = `TRIM(COALESCE(NULLIF(tp.prefix, ''), NULLIF(u.title, ''), '') || u.first_name || ' ' || u.last_name)`

// personName returns a user's name with title, or "" when the user is gone.
func personName(ctx context.Context, q querier, id uuid.UUID) string {
	var name string
	if err := q.QueryRow(ctx, `
		SELECT `+personNameSQL+`
		  FROM users u LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		 WHERE u.id = $1`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}

// courseLabelOf returns "CP353004 การพัฒนาซอฟต์แวร์" for a teaching course.
func courseLabelOf(ctx context.Context, q querier, tcID uuid.UUID) string {
	var code, name string
	_ = q.QueryRow(ctx, `SELECT code, COALESCE(name_th, '') FROM teaching_courses WHERE id = $1`, tcID).
		Scan(&code, &name)
	return strings.TrimSpace(code + " " + name)
}

// numberedLines renders items as "1. ...\n2. ..." for a notice body.
func numberedLines(items []string) string {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, it)
	}
	return b.String()
}

// thaiBaht renders a whole-baht amount with thousands separators: 12,345.
func thaiBaht(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
