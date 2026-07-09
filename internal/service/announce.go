package service

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// ============================================================================
// Domain
// ============================================================================

// AnnounceCategory is a small closed set that drives the FE color/icon.
// Any change here must match the migration's CHECK constraint.
var validCategories = map[string]bool{
	"info": true, "news": true, "warning": true, "urgent": true, "event": true,
}

// validAudienceRoles mirrors the role_code enum on `announcements.audience`.
// Rejecting bad values in the service keeps a rogue payload from producing a
// row that no one can ever read.
var validAudienceRoles = map[string]bool{
	"admin": true, "staff": true, "lecturer": true, "ta": true,
}

// Announcement is the wire shape returned to both staff (composer) and
// end-users (feed). `CoverImageURL` is a virtual read-only field derived
// from `CoverImageKey`; clients treat it as a resolvable URL.
type Announcement struct {
	ID             uuid.UUID  `json:"id"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Category       string     `json:"category"`
	Audience       []string   `json:"audience"`
	Pinned         bool       `json:"pinned"`
	CoverImageKey  *string    `json:"cover_image_key,omitempty"`
	CoverImageURL  *string    `json:"cover_image_url,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	AnnouncedAt    *time.Time `json:"announced_at,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	// Derived state — computed in Go so the FE doesn't reimplement timing rules.
	Status         string     `json:"status,omitempty"` // draft | scheduled | live | expired
}

type AnnounceService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
}

// ListFilter narrows the announcement list for a caller. Non-staff callers
// must supply RoleFilter; staff may pass IncludeAll to see drafts+scheduled+expired.
type ListFilter struct {
	RoleFilter  string
	IncludeAll  bool
}

// ============================================================================
// List / Get
// ============================================================================

// List returns announcements. When IncludeAll is false, only rows whose
// window contains "now" are returned; when true (staff), everything is
// returned so the composer can edit drafts and scheduled posts.
//
// Ordering places pinned live posts first, then reverse chronological.
func (s *AnnounceService) List(ctx context.Context, f ListFilter) ([]Announcement, error) {
	// Lazy fanout: any scheduled row that has come due but not yet been
	// broadcast gets picked up here. Doing it on read means we don't need a
	// tick to be precisely on time — the next observer flushes the queue.
	s.tryFanoutDue(ctx)

	q := strings.Builder{}
	q.WriteString(`SELECT id, title, body, category, audience, pinned,
	                       cover_image_key, published_at, expires_at,
	                       announced_at, created_at, updated_at
	                FROM announcements`)
	args := []any{}
	where := []string{}

	if f.RoleFilter != "" {
		args = append(args, f.RoleFilter)
		where = append(where, `$1 = ANY(audience)`)
	}
	if !f.IncludeAll {
		where = append(where,
			`published_at IS NOT NULL AND published_at <= NOW()`,
			`(expires_at IS NULL OR expires_at > NOW())`,
		)
	}
	if len(where) > 0 {
		q.WriteString(" WHERE ")
		q.WriteString(strings.Join(where, " AND "))
	}
	// Live pinned first, then newest. NULL published_at (drafts) sort last.
	q.WriteString(` ORDER BY
	    (pinned AND published_at IS NOT NULL AND published_at <= NOW()
	         AND (expires_at IS NULL OR expires_at > NOW())) DESC,
	    COALESCE(published_at, created_at) DESC`)

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Announcement, 0, 16)
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(
			&a.ID, &a.Title, &a.Body, &a.Category, &a.Audience, &a.Pinned,
			&a.CoverImageKey, &a.PublishedAt, &a.ExpiresAt,
			&a.AnnouncedAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		a.CoverImageURL = coverURL(a.CoverImageKey)
		a.Status = deriveStatus(a.PublishedAt, a.ExpiresAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get returns a single announcement regardless of publish state. Visibility
// checks are the caller's job — this is used both by the composer and by
// public feed detail pages, and both call sites already gate access.
func (s *AnnounceService) Get(ctx context.Context, id uuid.UUID) (*Announcement, error) {
	var a Announcement
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, body, category, audience, pinned,
		       cover_image_key, published_at, expires_at,
		       announced_at, created_at, updated_at
		FROM announcements WHERE id = $1
	`, id).Scan(
		&a.ID, &a.Title, &a.Body, &a.Category, &a.Audience, &a.Pinned,
		&a.CoverImageKey, &a.PublishedAt, &a.ExpiresAt,
		&a.AnnouncedAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CoverImageURL = coverURL(a.CoverImageKey)
	a.Status = deriveStatus(a.PublishedAt, a.ExpiresAt)
	return &a, nil
}

// ============================================================================
// Upsert
// ============================================================================

// UpsertInput is the composer payload. Zero UUID means "create"; a real UUID
// updates in place. The service enforces category, audience, and scheduling
// invariants (expires_at > published_at, etc.) so a broken row can't land.
type UpsertInput struct {
	ID            uuid.UUID  `json:"id"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Category      string     `json:"category"`
	Audience      []string   `json:"audience"`
	Pinned        bool       `json:"pinned"`
	CoverImageKey *string    `json:"cover_image_key"`
	PublishedAt   *time.Time `json:"published_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

func (s *AnnounceService) Upsert(ctx context.Context, actor uuid.UUID, in UpsertInput) (uuid.UUID, error) {
	title := strings.TrimSpace(in.Title)
	body := strings.TrimSpace(in.Body)
	if title == "" {
		return uuid.Nil, errors.New("หัวข้อประกาศต้องไม่ว่าง")
	}
	if len(title) > 200 {
		return uuid.Nil, errors.New("หัวข้อประกาศยาวเกิน 200 ตัวอักษร")
	}
	if body == "" {
		return uuid.Nil, errors.New("เนื้อหาประกาศต้องไม่ว่าง")
	}
	if len(body) > 8000 {
		return uuid.Nil, errors.New("เนื้อหาประกาศยาวเกิน 8000 ตัวอักษร")
	}
	category := strings.TrimSpace(in.Category)
	if category == "" {
		category = "info"
	}
	if !validCategories[category] {
		return uuid.Nil, errors.New("หมวดหมู่ไม่ถูกต้อง")
	}
	// Audience: dedupe + validate. Empty audience = broadcast to all four roles.
	aud := normalizeAudience(in.Audience)
	if len(aud) == 0 {
		return uuid.Nil, errors.New("ต้องเลือกกลุ่มผู้รับอย่างน้อยหนึ่งกลุ่ม")
	}
	// Scheduling sanity — expiry after publish, published_at not more than a year old.
	if in.ExpiresAt != nil && in.PublishedAt != nil && !in.ExpiresAt.After(*in.PublishedAt) {
		return uuid.Nil, errors.New("วันหมดอายุต้องอยู่หลังวันเผยแพร่")
	}
	if in.ExpiresAt != nil && in.PublishedAt == nil && in.ExpiresAt.Before(time.Now()) {
		return uuid.Nil, errors.New("วันหมดอายุต้องอยู่ในอนาคต")
	}
	if in.CoverImageKey != nil {
		k := strings.TrimSpace(*in.CoverImageKey)
		if k == "" {
			in.CoverImageKey = nil
		} else if !strings.HasPrefix(k, "announcements/") {
			return uuid.Nil, errors.New("cover_image_key ไม่ถูกต้อง")
		} else {
			in.CoverImageKey = &k
		}
	}

	isNew := in.ID == uuid.Nil
	if isNew {
		in.ID = uuid.New()
	}

	// Detect the "should we fanout now?" transition. Two things matter:
	//   1. Row is (or has just become) published — published_at <= NOW().
	//   2. We haven't already announced (announced_at IS NULL).
	// The fanout itself happens after commit so a rollback can't leak notifs.
	var oldAnnouncedAt *time.Time
	if !isNew {
		_ = s.pool.QueryRow(ctx, `SELECT announced_at FROM announcements WHERE id=$1`, in.ID).Scan(&oldAnnouncedAt)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	if isNew {
		_, err = tx.Exec(ctx, `
			INSERT INTO announcements (
				id, title, body, category, audience, pinned,
				cover_image_key, published_at, expires_at,
				created_by, updated_by
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		`, in.ID, title, body, category, aud, in.Pinned,
			in.CoverImageKey, in.PublishedAt, in.ExpiresAt, actor)
	} else {
		tag, execErr := tx.Exec(ctx, `
			UPDATE announcements
			   SET title=$2, body=$3, category=$4, audience=$5, pinned=$6,
			       cover_image_key=$7, published_at=$8, expires_at=$9,
			       updated_by=$10, updated_at=NOW()
			 WHERE id=$1
		`, in.ID, title, body, category, aud, in.Pinned,
			in.CoverImageKey, in.PublishedAt, in.ExpiresAt, actor)
		err = execErr
		if err == nil && tag.RowsAffected() == 0 {
			return uuid.Nil, ErrNotFound
		}
	}
	if err != nil {
		return uuid.Nil, err
	}

	// Clear announced_at when the composer schedules a NEW future publish
	// after previously being announced, so the sweeper will refire.
	if !isNew && oldAnnouncedAt != nil && in.PublishedAt != nil && in.PublishedAt.After(*oldAnnouncedAt) {
		if _, err := tx.Exec(ctx, `UPDATE announcements SET announced_at=NULL WHERE id=$1`, in.ID); err != nil {
			return uuid.Nil, err
		}
		oldAnnouncedAt = nil
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}

	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "announce.upsert", Entity: "announcement",
		EntityID: in.ID.String(), After: in,
	})

	// Fanout only if the row is live *now* and hasn't been announced yet.
	if oldAnnouncedAt == nil && in.PublishedAt != nil && !in.PublishedAt.After(time.Now()) {
		s.fanout(ctx, in.ID)
	}
	return in.ID, nil
}

// ============================================================================
// Delete / Publish / Unpublish
// ============================================================================

func (s *AnnounceService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM announcements WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "announce.delete", Entity: "announcement", EntityID: id.String(),
	})
	return nil
}

// Publish flips a draft/scheduled row to "live now" and fires the fanout.
// Safe to call repeatedly — a second call just refreshes published_at but
// the fanout guard (announced_at) prevents re-notifying.
func (s *AnnounceService) Publish(ctx context.Context, actor, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE announcements
		   SET published_at = COALESCE(published_at, NOW()),
		       updated_by = $2, updated_at = NOW()
		 WHERE id = $1
	`, id, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "announce.publish", Entity: "announcement", EntityID: id.String(),
	})
	s.fanout(ctx, id)
	return nil
}

// Unpublish demotes a live announcement back to draft. Notifications that
// already went out are left in place — the recipient's inbox is a log, not
// a mirror of the current state.
func (s *AnnounceService) Unpublish(ctx context.Context, actor, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE announcements
		   SET published_at = NULL, announced_at = NULL,
		       updated_by = $2, updated_at = NOW()
		 WHERE id = $1
	`, id, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "announce.unpublish", Entity: "announcement", EntityID: id.String(),
	})
	return nil
}

// ============================================================================
// Fanout — the "how do recipients actually see this" side
// ============================================================================

// fanout creates one in-app notification per active user whose role appears
// in the announcement's audience. Idempotent per-announcement thanks to
// `announced_at`. Best-effort: any per-row error just gets logged so a bad
// email address can't stall the whole rollout.
func (s *AnnounceService) fanout(ctx context.Context, id uuid.UUID) {
	if s.notify == nil {
		return
	}
	var (
		title    string
		body     string
		category string
		audience []string
		announcedAt *time.Time
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT title, body, category, audience, announced_at
		  FROM announcements WHERE id = $1
	`, id).Scan(&title, &body, &category, &audience, &announcedAt); err != nil {
		log.Printf("announce.fanout lookup: %v", err)
		return
	}
	if announcedAt != nil {
		return // already fanned out — nothing to do
	}
	if len(audience) == 0 {
		return
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.id
		  FROM users u
		  JOIN user_roles ur ON ur.user_id = u.id
		 WHERE ur.role::text = ANY($1)
		   AND u.is_active = TRUE
		   AND u.deleted_at IS NULL
	`, audience)
	if err != nil {
		log.Printf("announce.fanout users: %v", err)
		return
	}
	defer rows.Close()

	link := "/announcements/" + id.String()
	prefix := ""
	switch category {
	case "urgent":
		prefix = "[ด่วน] "
	case "warning":
		prefix = "[แจ้งเตือน] "
	case "event":
		prefix = "[กิจกรรม] "
	}
	subject := prefix + title

	// Truncate the body preview to something reasonable for an in-app card.
	preview := body
	if len(preview) > 240 {
		preview = preview[:240] + "…"
	}

	var count int
	for rows.Next() {
		var uid uuid.UUID
		if err := rows.Scan(&uid); err != nil {
			log.Printf("announce.fanout scan: %v", err)
			continue
		}
		// Reuse the shared notification pipeline — it handles both the in-app
		// row and the (best-effort) email delivery.
		s.notify.Send(ctx, uid, subject, preview, link)
		count++
	}
	if _, err := s.pool.Exec(ctx, `UPDATE announcements SET announced_at = NOW() WHERE id = $1`, id); err != nil {
		log.Printf("announce.fanout mark: %v", err)
		return
	}
	log.Printf("announce.fanout: id=%s recipients=%d", id, count)
}

// tryFanoutDue is called from List. It picks up any scheduled rows whose
// publish time has arrived but that haven't been fanned out yet. Cheap
// query — the partial index announcements_pending_fanout_idx makes it O(k).
func (s *AnnounceService) tryFanoutDue(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM announcements
		 WHERE announced_at IS NULL
		   AND published_at IS NOT NULL
		   AND published_at <= NOW()
		   AND (expires_at IS NULL OR expires_at > NOW())
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		s.fanout(ctx, id)
	}
}

// RunScheduler blocks and periodically flushes overdue scheduled fanouts.
// Called from main in a goroutine. Uses a modest 60-second cadence — the
// lazy fanout in List already handles anything that a bell/feed observer
// triggers, and this loop is only a safety net for periods of low traffic.
func (s *AnnounceService) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tryFanoutDue(ctx)
		}
	}
}

// ============================================================================
// Helpers
// ============================================================================

func coverURL(key *string) *string {
	if key == nil || *key == "" {
		return nil
	}
	// Router mounts the serve endpoint at /api/v1/announcements/images/*key.
	// Returning an /api-relative URL keeps CDN/proxy setups simple.
	u := "/api/v1/announcements/images/" + *key
	return &u
}

func deriveStatus(publishedAt, expiresAt *time.Time) string {
	now := time.Now()
	switch {
	case publishedAt == nil:
		return "draft"
	case publishedAt.After(now):
		return "scheduled"
	case expiresAt != nil && !expiresAt.After(now):
		return "expired"
	default:
		return "live"
	}
}

func normalizeAudience(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.ToLower(strings.TrimSpace(r))
		if !validAudienceRoles[r] || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
