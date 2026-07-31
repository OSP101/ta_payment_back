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

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	dailyCheck := time.NewTicker(24 * time.Hour)
	defer dailyCheck.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		case <-dailyCheck.C:
			s.dailyClose(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
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
}

func (s *Scheduler) dailyClose(ctx context.Context) {
	n, err := s.svc.SubmissionPeriods.AutoCloseExpired(ctx)
	if err != nil {
		log.Printf("scheduler: auto_close err=%v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: auto-closed %d expired periods", n)
	}
}
