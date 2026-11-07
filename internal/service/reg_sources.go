package service

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"ta-payment-back/internal/regkku"
)

const regHealthTTL = 30 * time.Second

// Sources probes from the server that will actually perform the fetch. A
// short shared cache coalesces repeated modal opens across staff members.
func (s *RegEnrolmentService) Sources(ctx context.Context, actor uuid.UUID) ([]regkku.Availability, error) {
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	return s.checkSources(ctx), nil
}

func (s *RegEnrolmentService) checkSources(ctx context.Context) []regkku.Availability {
	s.mu.Lock()
	active := ""
	for _, job := range s.jobs {
		if job.Status == "running" {
			active = job.Source
			break
		}
	}
	s.mu.Unlock()
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if len(s.health) == 2 && time.Now().Before(s.healthUntil) {
		copy := append([]regkku.Availability(nil), s.health...)
		for i := range copy {
			if copy[i].Source == active {
				copy[i] = regInUse(active)
			}
		}
		return copy
	}
	out := make([]regkku.Availability, 2)
	probes := []struct {
		source string
		run    func(context.Context) error
	}{{regkku.SourceNew, s.reg.Probe}, {regkku.SourceLegacy, s.legacy.Probe}}
	var wg sync.WaitGroup
	for i, p := range probes {
		if p.source == active {
			out[i] = regInUse(p.source)
			continue
		}
		wg.Add(1)
		go func(i int, source string, probe func(context.Context) error) {
			defer wg.Done()
			bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			out[i] = regkku.DescribeAccess(source, probe(bounded))
		}(i, p.source, p.run)
	}
	wg.Wait()
	s.health, s.healthUntil = out, time.Now().Add(regHealthTTL)
	return append([]regkku.Availability(nil), out...)
}

func (s *RegEnrolmentService) rememberSourceProblem(problem regkku.Availability) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	for i := range s.health {
		if s.health[i].Source == problem.Source {
			s.health[i] = problem
		}
	}
	// Keep the probe cache short; the provider's cooldown preserves Retry-After.
	s.healthUntil = time.Now().Add(regHealthTTL)
}

func (s *RegEnrolmentService) sourceAllowed(source string) error {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if time.Now().Before(s.healthUntil) {
		for _, health := range s.health {
			if health.Source == source && !health.Available && health.State != "in_use" {
				return Conflict(health.Message + " · " + health.Action)
			}
		}
	}
	return nil
}

func regInUse(source string) regkku.Availability {
	return regkku.Availability{Source: source, State: "in_use", CheckedAt: time.Now(), Message: "กำลังดึงข้อมูลจากเว็บไซต์นี้", Action: "รอให้งานเดิมเสร็จ หรือหยุดงานเดิมก่อนเริ่มใหม่"}
}
