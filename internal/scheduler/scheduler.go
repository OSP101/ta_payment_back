// Package scheduler owns the background timers that fire off submission
// reminders and auto-close expired periods. It runs as a goroutine inside the
// main API process — the plan called for a separate cmd/scheduler binary but
// starting it inline saves an extra service in docker-compose. If a future
// deployment needs to scale the reminder worker separately, this file is small
// enough to lift into cmd/scheduler/main.go with only a Container factory
// change.
package scheduler

import (
	"context"
	"log"
	"time"

	"ta-payment-back/internal/handler"
	"ta-payment-back/internal/service"
)

type Scheduler struct {
	svc *service.Container
}

func New(svc *service.Container) *Scheduler {
	return &Scheduler{svc: svc}
}

// Start launches the background loop and returns immediately. The loop stops
// when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

// loop fires every hour: sweep due-window reminders, once-daily auto-close.
// A short 5s startup jitter prevents thundering-herd on cold-start.
func (s *Scheduler) loop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	// Do one immediate sweep so restarts don't miss a due window that only
	// has a few hours left; then settle into hourly cadence.
	s.tick(ctx)
	// เรียกทันทีตอนเริ่ม ด้วยเหตุผลเดียวกับ s.tick ข้างบน: ticker 24 ชม.
	// เดิมยิงครั้งแรกที่ชั่วโมงที่ 24 และรีเซ็ตทุกครั้งที่โปรเซสรีสตาร์ท ⇒
	// deployment ที่ deploy ถี่กว่าวันละครั้งจะไม่เคยรัน dailyClose เลยแม้แต่
	// ครั้งเดียว (นโยบายเก็บ audit 5 ปี, การล้าง session/challenge, และการตั้ง
	// is_closed ทั้งหมดอยู่ในนี้) — shouldRunDaily เช็ค DB เอง จึงเรียกได้ที่นี่
	// อย่างปลอดภัยแม้ dailyClose เพิ่งรันไปเมื่อไม่กี่นาทีก่อนรีสตาร์ท
	if s.shouldRunDaily(ctx) {
		s.dailyClose(ctx)
	}

	// เก็บวันที่รันล่าสุดใน DB แล้วเช็คทุกชั่วโมงว่าวันนี้รันไปหรือยัง —
	// ทนต่อการรีสตาร์ทได้จริง ต่างจาก ticker แบบเดิมที่นับจากเวลา boot
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
			if s.shouldRunDaily(ctx) {
				s.dailyClose(ctx)
			}
		}
	}
}

// shouldRunDaily reports whether dailyClose has not yet run today (Bangkok
// calendar day — the process and DB both run TZ=Asia/Bangkok, see
// deploy/docker-compose.yml). Checking the DB rather than an in-memory
// timestamp is what survives a restart: an in-memory "last ran at" resets to
// zero on every boot, which is the exact bug this replaces.
func (s *Scheduler) shouldRunDaily(ctx context.Context) bool {
	var upToDate bool
	err := s.svc.Pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM scheduler_daily_close_state WHERE last_run_at::date = CURRENT_DATE
		)`).Scan(&upToDate)
	if err != nil {
		log.Printf("scheduler: shouldRunDaily check failed, skipping this hour: %v", err)
		return false
	}
	return !upToDate
}

func (s *Scheduler) tick(ctx context.Context) {
	// Drop expired read-audit throttle keys. Cheap and unconditional: the map
	// holds one key per (viewer, audited screen, subject) seen in the last
	// hour, and without a sweep a long-lived process keeps every one of them
	// for the life of the process. See handler/audit_read.go.
	handler.SweepReadAudit(time.Now())

	// AUTH-01: unknownAttempts is keyed by an open key space (any email
	// string anyone submits), unlike loginAttempts which is bounded by real
	// user count — see login_gate.go.
	service.SweepLoginGate(time.Now())

	// PDPA-01: retry document/avatar deletion for any approved erasure
	// request whose files did not finish deleting the first time. Hourly,
	// not daily — a scrub failure left sitting is exactly the state a PDPA
	// erasure guarantee is not supposed to have.
	if n, err := s.svc.DataDeletion.SweepPendingScrubs(ctx); err != nil {
		log.Printf("scheduler: sweep_pending_scrubs err=%v", err)
	} else if n > 0 {
		log.Printf("scheduler: completed %d pending PDPA scrub(s)", n)
	}

	// OPS-03: zipTokens is keyed by a fresh random token every mint, unlike
	// loginAttempts/pwAttempts which are bounded by real user count — an
	// unconsumed token (staff clicked "prepare download" then navigated
	// away, or the download failed) sits in the map for the life of the
	// process without this.
	s.svc.Docs.SweepZipTokens(time.Now())

	// Safety net for the deferred TA-request decision. The primary trigger runs
	// when a TA saves their timetable; this catches anything that trigger
	// missed (a failed call, a crash mid-save, a timetable written by staff
	// through another path). Without it a request could rest in 'submitted'
	// forever, holding the course quota its assignments reserved.
	//
	// Runs before the reminder sweep because a request finalised here may
	// change who is due to submit work logs this month.
	if n, err := s.svc.TARequest.SweepPendingRequests(ctx); err != nil {
		log.Printf("scheduler: ta_request_sweep err=%v", err)
	} else if n > 0 {
		log.Printf("scheduler: finalised %d pending TA request(s)", n)
	}

	n, err := s.svc.SubmissionPeriods.SweepReminders(ctx)
	if err != nil {
		log.Printf("scheduler: sweep_reminders err=%v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: sent %d submission reminders", n)
	}

	// TDBM safety net: the webhook (POST /tdbm-webhook) is the fast path, but
	// TDBM gives no delivery guarantee for it (see
	// docs/TDBM-API-requirements.md's open questions on retry policy) — a
	// missed ping would otherwise go unnoticed until someone thinks to check.
	// Hourly bounds how stale we can get without one; SyncAll itself already
	// no-ops cleanly when no term is active.
	if _, err := s.svc.TDBM.SyncAll(ctx, "scheduler"); err != nil {
		log.Printf("scheduler: tdbm_sync err=%v", err)
	}
}

func (s *Scheduler) dailyClose(ctx context.Context) {
	// Age audit rows out at five years, archiving each batch to the document
	// store and verifying it before anything is deleted. Daily and batched: a
	// table that has never been purged catches up over several nights rather
	// than trying to move years of rows in one pass, and nothing is waiting on
	// it. An error here deletes nothing — see PurgeExpiredAudit's ordering.
	if r, err := s.svc.Audit.PurgeExpiredAudit(ctx, s.svc.Auditor); err != nil {
		log.Printf("scheduler: audit_purge err=%v", err)
	} else if r.Deleted > 0 {
		log.Printf("scheduler: archived and purged %d audit row(s) up to %s (sha256 %s)",
			r.Deleted, r.CoversTo.Format("2006-01-02"), r.SHA256[:12])
	}

	n, err := s.svc.SubmissionPeriods.AutoCloseExpired(ctx)
	if err != nil {
		log.Printf("scheduler: auto_close err=%v", err)
	} else if n > 0 {
		log.Printf("scheduler: auto-closed %d expired periods", n)
	}

	// Session rows are cheap individually but unbounded over time (every
	// login writes one) — see SessionService.Cleanup. Daily cadence is fine:
	// nothing reads a session after it is a week past revoked/expired, so
	// there is no freshness requirement pushing this onto the hourly tick.
	if n, err := s.svc.Sessions.Cleanup(ctx); err != nil {
		log.Printf("scheduler: session_cleanup err=%v", err)
	} else if n > 0 {
		log.Printf("scheduler: cleaned up %d expired session(s)", n)
	}

	// Same shape as session cleanup: mfa_challenges rows are cheap
	// individually but unbounded over time — every step-1 login for a 2FA
	// account writes one, whether or not step 2 ever completes.
	if n, err := s.svc.MFA.CleanupChallenges(ctx); err != nil {
		log.Printf("scheduler: mfa_challenge_cleanup err=%v", err)
	} else if n > 0 {
		log.Printf("scheduler: cleaned up %d expired mfa challenge(s)", n)
	}

	// Recorded LAST, unconditionally: even a run where every step above
	// logged an error still counts as "attempted today" — shouldRunDaily's
	// job is to guarantee at least one attempt per day, not to guarantee
	// every step succeeded (each step already logs its own failure).
	// Retrying the same day's work every hour on a transient error would
	// just repeat whatever already failed.
	if _, err := s.svc.Pool.Exec(ctx, `
		INSERT INTO scheduler_daily_close_state (id, last_run_at) VALUES (TRUE, NOW())
		ON CONFLICT (id) DO UPDATE SET last_run_at = NOW()`); err != nil {
		log.Printf("scheduler: recording dailyClose run failed: %v", err)
	}
}
