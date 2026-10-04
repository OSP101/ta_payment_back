package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The lecturer hears about a submission once (05/10/2026, migration 0150).
//
// Since 0149 each TA on a submission is a request of their own and is decided
// alone, and the lecturer was sent one notice per TA as each verdict landed:
// "ดูเยอะมาก กลายเป็นดูน่ารำคาญ". The verdicts of one submission (batch_id) are
// now held and sent together:
//
//   - as soon as every TA on it is decided, or
//   - 24 hours after it was sent, as a summary of who passed, who did not and
//     why, and who is still waiting; anyone decided after that is told in a
//     follow-up as their verdict lands.
//
// TAs are still told their own verdict straight away; only the lecturer waits.
// Requests from before 0149 (no batch_id) keep the old one-notice rule.
const lecturerDigestWait = 24 * time.Hour

// lecturerAwaitsDigest reports whether the lecturer's notice for this request
// is to be held for the submission's digest rather than sent now.
func (s *TARequestService) lecturerAwaitsDigest(ctx context.Context, reqID uuid.UUID) bool {
	var held bool
	if err := s.pool.QueryRow(ctx, `
		SELECT batch_id IS NOT NULL AND lecturer_notified_at IS NULL
		FROM ta_requests WHERE id = $1`, reqID).Scan(&held); err != nil {
		return false
	}
	return held
}

// holdLecturerNote keeps clash lines for the digest, where they are read next
// to the TA's verdict.
func (s *TARequestService) holdLecturerNote(ctx context.Context, reqID uuid.UUID, note string) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE ta_requests SET lecturer_note = CONCAT_WS(' ', NULLIF(lecturer_note, ''), $2::text)
		WHERE id = $1`, reqID, note); err != nil {
		log.Printf("ta_request %s: hold lecturer note: %v", reqID, err)
	}
}

// flushLecturerDigestFor sends the digest of the request's submission if it is
// due. Best-effort, like every notice.
func (s *TARequestService) flushLecturerDigestFor(ctx context.Context, reqID uuid.UUID) {
	var batchID *uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT batch_id FROM ta_requests WHERE id = $1`, reqID).Scan(&batchID); err != nil || batchID == nil {
		return
	}
	if _, err := s.flushLecturerDigest(ctx, *batchID, time.Now()); err != nil {
		log.Printf("ta_request batch %s: lecturer digest: %v", *batchID, err)
	}
}

type digestRow struct {
	id          uuid.UUID
	status      string
	notified    bool
	reason      string
	note        string
	taName      string
	submittedAt time.Time
	courseID    uuid.UUID
	lecturerID  uuid.UUID
}

// flushLecturerDigest sends one notice for every verdict of the submission
// the lecturer has not been told yet, when the rule above says it is time.
// Reports whether a notice went out.
func (s *TARequestService) flushLecturerDigest(ctx context.Context, batchID uuid.UUID, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// FOR UPDATE: the request being decided, the TA's timetable save and the
	// hourly sweep may all flush the same submission at once; only one sends.
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.status::text, r.lecturer_notified_at IS NOT NULL,
		       COALESCE(r.reject_reason, ''), COALESCE(r.lecturer_note, ''),
		       COALESCE((SELECT string_agg(DISTINCT u.first_name || ' ' || u.last_name, ', ')
		                   FROM ta_request_assignments a JOIN users u ON u.id = a.ta_id
		                  WHERE a.request_id = r.id), ''),
		       r.submitted_at, r.teaching_course_id, r.lecturer_id
		FROM ta_requests r
		WHERE r.batch_id = $1
		ORDER BY r.created_at, r.id
		FOR UPDATE OF r`, batchID)
	if err != nil {
		return false, err
	}
	var all []digestRow
	for rows.Next() {
		var d digestRow
		var submitted *time.Time
		if err := rows.Scan(&d.id, &d.status, &d.notified, &d.reason, &d.note, &d.taName,
			&submitted, &d.courseID, &d.lecturerID); err != nil {
			rows.Close()
			return false, err
		}
		if submitted != nil {
			d.submittedAt = *submitted
		}
		all = append(all, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	var fresh, pending []digestRow
	toldBefore := false
	var sent time.Time
	for _, d := range all {
		if !d.submittedAt.IsZero() && (sent.IsZero() || d.submittedAt.Before(sent)) {
			sent = d.submittedAt
		}
		switch d.status {
		case "submitted":
			pending = append(pending, d)
		case "approved", "rejected":
			if d.notified {
				toldBefore = true
			} else {
				fresh = append(fresh, d)
			}
		}
		// A cancelled request was the lecturer's own doing; nothing to tell.
	}
	if len(fresh) == 0 {
		return false, nil
	}
	timedOut := !sent.IsZero() && now.Sub(sent) >= lecturerDigestWait
	if len(pending) > 0 && !toldBefore && !timedOut {
		return false, nil // the rest may still land within the 24 hours
	}

	ids := make([]uuid.UUID, len(fresh))
	for i, d := range fresh {
		ids[i] = d.id
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ta_requests SET lecturer_notified_at = NOW(), lecturer_note = NULL
		WHERE id = ANY($1)`, ids); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	if s.notify == nil {
		return true, nil
	}
	title, body := s.lecturerDigestText(ctx, fresh, pending, toldBefore, sent)
	s.notify.Send(ctx, fresh[0].lecturerID, title, body, "/lecturer")
	return true, nil
}

func (s *TARequestService) lecturerDigestText(ctx context.Context, fresh, pending []digestRow, followUp bool, sent time.Time) (string, string) {
	code, nameTH := s.courseLabel(ctx, fresh[0].courseID)
	label := strings.TrimSpace(code + " " + nameTH)
	names := make([]string, len(fresh))
	for i, d := range fresh {
		names[i] = d.taName
	}
	// The TAs are in the title: unread notices fold by (title, link), and a
	// second submission for the course would otherwise overwrite this one.
	who := " (" + strings.Join(names, ", ") + ")"

	withNote := func(line, note string) string {
		if note == "" {
			return line
		}
		return line + " " + note
	}

	// One TA and nothing else to say: the plain notice it always was.
	if len(fresh) == 1 && len(pending) == 0 && !followUp {
		d := fresh[0]
		if d.status == "approved" {
			return "คำขอผู้ช่วยสอนได้รับการอนุมัติ " + code + who,
				withNote(fmt.Sprintf("ตามที่ท่านได้ยื่นคำขอผู้ช่วยสอนสำหรับรายวิชา %s นั้น ระบบได้ตรวจสอบคุณสมบัติและเงื่อนไขแล้ว คำขอแต่งตั้ง %s เป็นผู้ช่วยสอนได้รับการอนุมัติเรียบร้อยแล้ว", label, d.taName), d.note)
		}
		return "คำขอผู้ช่วยสอนไม่ผ่านการอนุมัติ " + code + who,
			withNote(fmt.Sprintf("ตามที่ท่านได้ยื่นคำขอผู้ช่วยสอนสำหรับรายวิชา %s นั้น ระบบได้ตรวจสอบแล้ว คำขอแต่งตั้ง %s เป็นผู้ช่วยสอนไม่ผ่านการอนุมัติ เนื่องจาก %s", label, d.taName, d.reason), d.note)
	}

	var approved, rejected, waiting []string
	for _, d := range fresh {
		if d.status == "approved" {
			approved = append(approved, withNote(d.taName, d.note))
		} else {
			rejected = append(rejected, withNote(d.taName+" เนื่องจาก "+d.reason, d.note))
		}
	}
	for _, d := range pending {
		waiting = append(waiting, d.taName+" รอผู้ช่วยสอนบันทึกตารางเรียน")
	}

	when := ""
	if !sent.IsZero() {
		when = " เมื่อวันที่ " + thaiLongDateTime(sent)
	}
	var b strings.Builder
	title := "ผลการพิจารณาคำขอผู้ช่วยสอน " + code + who
	switch {
	case followUp:
		title = "ผลการพิจารณาคำขอผู้ช่วยสอนเพิ่มเติม " + code + who
		fmt.Fprintf(&b, "ตามที่ท่านได้ยื่นคำขอผู้ช่วยสอนสำหรับรายวิชา %s%s และระบบได้แจ้งผลการพิจารณาบางส่วนไปแล้วนั้น บัดนี้ระบบได้พิจารณาผู้ช่วยสอนเพิ่มเติมแล้ว ผลการพิจารณามีดังนี้", label, when)
	case len(pending) > 0:
		fmt.Fprintf(&b, "ตามที่ท่านได้ยื่นคำขอผู้ช่วยสอนสำหรับรายวิชา %s%s นั้น ครบ 24 ชั่วโมงแล้วแต่ยังพิจารณาไม่ครบทุกคน ระบบจึงขอสรุปผลการพิจารณาเท่าที่ทราบในขณะนี้ ดังนี้", label, when)
	default:
		fmt.Fprintf(&b, "ตามที่ท่านได้ยื่นคำขอผู้ช่วยสอนสำหรับรายวิชา %s%s นั้น ระบบได้ตรวจสอบคุณสมบัติและเงื่อนไขของผู้ช่วยสอนเป็นรายบุคคลครบทุกคนแล้ว ผลการพิจารณามีดังนี้", label, when)
	}
	section := func(head string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n\n%s %d คน\n%s", head, len(lines), numberedLines(lines))
	}
	section("ได้รับการอนุมัติ", approved)
	section("ไม่ผ่านการอนุมัติ", rejected)
	section("อยู่ระหว่างพิจารณา", waiting)
	if len(waiting) > 0 {
		b.WriteString("\n\nระบบจะพิจารณาผู้ช่วยสอนที่รอตารางเรียนให้อัตโนมัติเมื่อบันทึกตารางเรียนแล้ว และจะแจ้งผลให้ท่านทราบอีกครั้ง")
	}
	return title, b.String()
}

// SweepLecturerDigests sends every digest that has come due: the 24-hour
// summaries, and submissions whose last TA was decided by a path that did not
// flush (a cancel, a crash between the decision and the notice).
func (s *TARequestService) SweepLecturerDigests(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT batch_id FROM ta_requests
		WHERE batch_id IS NOT NULL AND lecturer_notified_at IS NULL
		  AND status IN ('approved', 'rejected')`)
	if err != nil {
		return 0, err
	}
	var batches []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		batches = append(batches, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	sent := 0
	for _, id := range batches {
		ok, err := s.flushLecturerDigest(ctx, id, now)
		if err != nil {
			log.Printf("ta_request batch %s: lecturer digest: %v", id, err)
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}
