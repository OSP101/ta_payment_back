package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/timeutil"
)

// PublicNoticeService feeds the cards on the login page. The page is
// anonymous, so everything here is college-wide and names no one: a term, a
// deadline, and how far the term's paper bundle has travelled. Per-document and
// per-person status stays behind the login (the /ta home cards).
//
// Both cards are "only when there is something to say": no live request window
// means no window card, and a document bundle that staff have not yet moved
// off stage 0 (or finished long ago) means no document card.
type PublicNoticeService struct {
	pool *pgxpool.Pool
	docs *DocumentProgressService

	mu       sync.Mutex
	cached   *PublicNotices
	cachedAt time.Time
}

// publicNoticeTTL keeps a burst of anonymous page loads from each running the
// window and fiscal-round queries; a deadline card that is 20 s stale is fine.
const publicNoticeTTL = 20 * time.Second

// docDoneVisibleFor is how long a bundle that reached the last stage stays on
// the login page before the card steps aside.
const docDoneVisibleFor = 14 * 24 * time.Hour

// stageTotal is the number of stages after "not started" (see
// DocumentProgressBoard's STAGES on the front end).
const stageTotal = 5

var publicStageLabels = [stageTotal + 1]string{
	"",
	"ผู้ช่วยสอนเซ็นครบ",
	"อาจารย์เซ็นครบ",
	"ผู้รับรองเซ็นครบ",
	"ส่งการเงินแล้ว",
	"คณบดีลงนามแล้ว",
}

// PublicRequestWindow is the TA-request deadline card.
type PublicRequestWindow struct {
	TermLabel string `json:"term_label"`
	OpensAt   string `json:"opens_at"`
	ClosesAt  string `json:"closes_at"`
	// DaysLeft is whole Bangkok calendar days until the closing date: 0 on
	// the closing day itself.
	DaysLeft int `json:"days_left"`
	// ElapsedPct is how much of the window has gone by, 0..100, for the bar.
	ElapsedPct int `json:"elapsed_pct"`
}

// PublicDocProgress is one document bundle's journey (a term, or one fiscal
// round of it).
type PublicDocProgress struct {
	TermLabel  string `json:"term_label"`
	RoundLabel string `json:"round_label,omitempty"`
	Stage      int    `json:"stage"` // 1..5
	StageTotal int    `json:"stage_total"`
	StageLabel string `json:"stage_label"`
	UpdatedAt  string `json:"updated_at"`
}

// PublicNotices is the whole payload. Either part may be absent.
type PublicNotices struct {
	RequestWindow *PublicRequestWindow `json:"request_window,omitempty"`
	Documents     []PublicDocProgress  `json:"documents,omitempty"`
}

func (n *PublicNotices) empty() bool {
	return n.RequestWindow == nil && len(n.Documents) == 0
}

// Get returns the current notices, from a short cache when it is fresh.
func (s *PublicNoticeService) Get(ctx context.Context) (*PublicNotices, error) {
	s.mu.Lock()
	if s.cached != nil && time.Since(s.cachedAt) < publicNoticeTTL {
		out := s.cached
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	out, err := s.compute(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = out, time.Now()
	s.mu.Unlock()
	return out, nil
}

func (s *PublicNoticeService) compute(ctx context.Context, now time.Time) (*PublicNotices, error) {
	out := &PublicNotices{}

	w, err := s.requestWindow(ctx, now)
	if err != nil {
		return nil, err
	}
	out.RequestWindow = w

	docs, err := s.documents(ctx, now)
	if err != nil {
		return nil, err
	}
	out.Documents = docs
	return out, nil
}

// requestWindow answers for the active term only, with the same window rule
// the lecturer e-mails use: the latest-closing window, switched on, between
// its opening and closing times.
func (s *PublicNoticeService) requestWindow(ctx context.Context, now time.Time) (*PublicRequestWindow, error) {
	var (
		year, sem        int
		opensAt, closeAt time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.academic_year, t.semester, w.opens_at, w.closes_at
		  FROM academic_terms t
		  JOIN (`+noticeWindowsSQL+`) w ON w.term_id = t.id
		 WHERE t.is_active
		   AND w.is_open AND w.opens_at <= $1 AND w.closes_at > $1
		 ORDER BY w.closes_at
		 LIMIT 1`, now).Scan(&year, &sem, &opensAt, &closeAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	day := func(t time.Time) time.Time {
		b := t.In(timeutil.Bangkok)
		return time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, timeutil.Bangkok)
	}
	daysLeft := int(day(closeAt).Sub(day(now)).Hours()/24 + 0.5)
	if daysLeft < 0 {
		daysLeft = 0
	}
	pct := 100
	if span := closeAt.Sub(opensAt); span > 0 {
		pct = int(now.Sub(opensAt) * 100 / span)
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return &PublicRequestWindow{
		TermLabel:  fmt.Sprintf("%d/%d", year, sem),
		OpensAt:    opensAt.In(timeutil.Bangkok).Format(time.RFC3339),
		ClosesAt:   closeAt.In(timeutil.Bangkok).Format(time.RFC3339),
		DaysLeft:   daysLeft,
		ElapsedPct: pct,
	}, nil
}

// documents lists the active term's bundles that staff have started moving
// (stage 1+) and that are not long finished.
func (s *PublicNoticeService) documents(ctx context.Context, now time.Time) ([]PublicDocProgress, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.academic_year, t.semester, dp.fiscal_round, dp.stage, dp.updated_at
		  FROM document_progress dp
		  JOIN academic_terms t ON t.id = dp.term_id
		 WHERE t.is_active AND dp.stage >= 1
		   AND (dp.stage < $1 OR dp.updated_at > $2)
		 ORDER BY dp.fiscal_round`, stageTotal, now.Add(-docDoneVisibleFor))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		termID     uuid.UUID
		year, sem  int
		round, stg int
		updated    time.Time
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.termID, &r.year, &r.sem, &r.round, &r.stg, &r.updated); err != nil {
			return nil, err
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, nil
	}

	// "รอบ" wording only when the term really has two rounds, same as the
	// staff board; a single-round term never shows it.
	var fr *fiscalRounds
	if len(got) > 1 || got[0].round == 2 {
		fr, err = s.docs.resolveFiscalRounds(ctx, got[0].termID)
		if err != nil {
			return nil, err
		}
	}

	out := make([]PublicDocProgress, 0, len(got))
	for _, r := range got {
		d := PublicDocProgress{
			TermLabel:  fmt.Sprintf("%d/%d", r.year, r.sem),
			Stage:      r.stg,
			StageTotal: stageTotal,
			StageLabel: publicStageLabels[r.stg],
			UpdatedAt:  r.updated.In(timeutil.Bangkok).Format(time.RFC3339),
		}
		if fr != nil && fr.twoRounds {
			d.RoundLabel = fr.round1Label
			if r.round == 2 {
				d.RoundLabel = fr.round2Label
			}
		}
		out = append(out, d)
	}
	return out, nil
}
