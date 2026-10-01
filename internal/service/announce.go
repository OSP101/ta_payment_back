package service

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"

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
	ID            uuid.UUID  `json:"id"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Category      string     `json:"category"`
	Audience      []string   `json:"audience"`
	Pinned        bool       `json:"pinned"`
	CoverImageKey *string    `json:"cover_image_key,omitempty"`
	CoverImageURL *string    `json:"cover_image_url,omitempty"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	AnnouncedAt   *time.Time `json:"announced_at,omitempty"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	// IsPublic opens the announcement to readers with no account, through the
	// share link. Publishing state still applies: a public draft is not visible.
	IsPublic        bool                     `json:"is_public"`
	TargetCourseIDs []uuid.UUID              `json:"target_course_ids"`
	TargetUserIDs   []uuid.UUID              `json:"target_user_ids"`
	TargetFilters   []string                 `json:"target_filters"`
	TargetTermID    *uuid.UUID               `json:"target_term_id,omitempty"`
	Attachments     []AnnouncementAttachment `json:"attachments,omitempty"`
	// AudienceCount is how many people the announcement actually reached.
	// Filled from the materialised ledger; 0 before publishing. The three
	// counts beside it break that number down for the staff list, so a failed
	// email is visible without opening every announcement.
	AudienceCount int `json:"audience_count"`
	FailedCount   int `json:"failed_count,omitempty"`
	PendingCount  int `json:"pending_count,omitempty"`
	ReadCount     int `json:"read_count,omitempty"`
	// RemindedAt is when unread recipients were last nudged.
	RemindedAt *time.Time `json:"reminded_at,omitempty"`
	// TargetCourses / TargetUsers carry the names behind the target ids, so the
	// composer can show "CP363205 …" instead of a UUID. Filled by Get only.
	TargetCourses []TargetCourse   `json:"target_courses,omitempty"`
	TargetUsers   []AudienceMember `json:"target_users,omitempty"`
	// Excerpt is the body with its markup taken off, cut to a length that fits
	// a link-preview card. Filled by PublicGet, because the page that needs it
	// is rendered for crawlers that cannot run the React renderer. Derived from
	// Body by the same stripper the mail preview uses, so a shared card and an
	// email can never describe the announcement differently.
	Excerpt string `json:"excerpt,omitempty"`
	// Recipients is the extra-email ledger. Populated by Get only — the feed
	// has no use for it and it would be the largest field in every list row.
	Recipients []AnnouncementRecipient `json:"recipients,omitempty"`
	// Derived state — computed in Go so the FE doesn't reimplement timing rules.
	Status string `json:"status,omitempty"` // draft | scheduled | live | expired
}

// AnnouncementRecipient is one address on the extra-email list, with what
// happened to it. Staff read this to answer "did it actually go out?".
type AnnouncementRecipient struct {
	Email  string     `json:"email"`
	Name   string     `json:"name,omitempty"` // when picked from the user list
	UserID *uuid.UUID `json:"user_id,omitempty"`
	Status string     `json:"status"` // pending | sent | skipped | failed
	SentAt *time.Time `json:"sent_at,omitempty"`
	ReadAt *time.Time `json:"read_at,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// TargetCourse names one course a rule aims at.
type TargetCourse struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	Name string    `json:"name"`
}

type AnnounceService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
}

// ListFilter narrows the announcement list for a caller. Non-staff callers
// must supply RoleFilter; staff may pass IncludeAll to see drafts+scheduled+expired.
type ListFilter struct {
	RoleFilter string
	IncludeAll bool
	// ViewerID is who is asking. Targeting is per-person, so the feed reads the
	// same materialised ledger the email went to — one source, so a reader can
	// never be mailed about something their feed refuses to show.
	ViewerID uuid.UUID
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
	                       announced_at, created_at, updated_at, is_public,
	                       target_course_ids, target_user_ids, target_filters, target_term_id,
	                       reminded_at
	                FROM announcements`)
	args := []any{}
	where := []string{}

	if !f.IncludeAll && f.ViewerID != uuid.Nil {
		args = append(args, f.ViewerID)
		where = append(where,
			`EXISTS (SELECT 1 FROM announcement_recipients r
			          WHERE r.announcement_id = announcements.id AND r.user_id = $1)`)
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
			&a.AnnouncedAt, &a.CreatedAt, &a.UpdatedAt, &a.IsPublic,
			&a.TargetCourseIDs, &a.TargetUserIDs, &a.TargetFilters, &a.TargetTermID,
			&a.RemindedAt,
		); err != nil {
			return nil, err
		}
		a.CoverImageURL = coverURL(a.CoverImageKey)
		a.Status = deriveStatus(a.PublishedAt, a.ExpiresAt)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(out))
	for _, a := range out {
		ids = append(ids, a.ID)
	}
	if byID, err := s.loadAttachmentsFor(ctx, ids); err == nil {
		for i := range out {
			out[i].Attachments = byID[out[i].ID]
		}
	}
	if f.IncludeAll {
		if err := s.fillDeliveryCounts(ctx, out, ids); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillDeliveryCounts adds the ledger totals to the staff list in one query.
func (s *AnnounceService) fillDeliveryCounts(ctx context.Context, out []Announcement, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT announcement_id,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'failed'),
		       COUNT(*) FILTER (WHERE status = 'pending'),
		       COUNT(*) FILTER (WHERE read_at IS NOT NULL)
		  FROM announcement_recipients
		 WHERE announcement_id = ANY($1) AND user_id IS NOT NULL
		 GROUP BY announcement_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	type counts struct{ all, failed, pending, read int }
	byID := map[uuid.UUID]counts{}
	for rows.Next() {
		var id uuid.UUID
		var c counts
		if err := rows.Scan(&id, &c.all, &c.failed, &c.pending, &c.read); err != nil {
			return err
		}
		byID[id] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range out {
		c := byID[out[i].ID]
		out[i].AudienceCount, out[i].FailedCount = c.all, c.failed
		out[i].PendingCount, out[i].ReadCount = c.pending, c.read
	}
	return nil
}

// Get returns a single announcement regardless of publish state. Visibility
// checks are the caller's job — this is used both by the composer and by
// public feed detail pages, and both call sites already gate access.
func (s *AnnounceService) Get(ctx context.Context, id uuid.UUID) (*Announcement, error) {
	var a Announcement
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, body, category, audience, pinned,
		       cover_image_key, published_at, expires_at,
		       announced_at, created_at, updated_at, is_public,
		       target_course_ids, target_user_ids, target_filters, target_term_id,
		       reminded_at
		FROM announcements WHERE id = $1
	`, id).Scan(
		&a.ID, &a.Title, &a.Body, &a.Category, &a.Audience, &a.Pinned,
		&a.CoverImageKey, &a.PublishedAt, &a.ExpiresAt,
		&a.AnnouncedAt, &a.CreatedAt, &a.UpdatedAt, &a.IsPublic,
		&a.TargetCourseIDs, &a.TargetUserIDs, &a.TargetFilters, &a.TargetTermID,
		&a.RemindedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CoverImageURL = coverURL(a.CoverImageKey)
	a.Status = deriveStatus(a.PublishedAt, a.ExpiresAt)
	if rec, err := s.loadRecipients(ctx, id); err == nil {
		a.Recipients = rec
		a.AudienceCount = len(rec)
		for _, r := range rec {
			switch r.Status {
			case "failed":
				a.FailedCount++
			case "pending":
				a.PendingCount++
			}
			if r.ReadAt != nil {
				a.ReadCount++
			}
		}
	}
	a.TargetCourses, a.TargetUsers = s.targetNames(ctx, a.TargetCourseIDs, a.TargetUserIDs)
	if att, err := s.loadAttachments(ctx, id); err == nil {
		a.Attachments = att
	}
	return &a, nil
}

// targetNames resolves the ids of a rule to what an officer calls them. A
// course or account that has since been deleted is simply absent — the composer
// then drops it from the rule rather than showing an id nobody can read.
func (s *AnnounceService) targetNames(ctx context.Context, courseIDs, userIDs []uuid.UUID) ([]TargetCourse, []AudienceMember) {
	courses := []TargetCourse{}
	if len(courseIDs) > 0 {
		rows, err := s.pool.Query(ctx, `
			SELECT id, code, name_th FROM teaching_courses WHERE id = ANY($1) ORDER BY code`, courseIDs)
		if err == nil {
			for rows.Next() {
				var c TargetCourse
				if rows.Scan(&c.ID, &c.Code, &c.Name) == nil {
					courses = append(courses, c)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				log.Printf("announce.targetNames courses: %v", err)
			}
		}
	}
	users := []AudienceMember{}
	if len(userIDs) > 0 {
		rows, err := s.pool.Query(ctx, `
			SELECT id, email, COALESCE(NULLIF(title,''), '') || first_name || ' ' || last_name
			  FROM users WHERE id = ANY($1) ORDER BY first_name, last_name`, userIDs)
		if err == nil {
			for rows.Next() {
				var u AudienceMember
				if rows.Scan(&u.ID, &u.Email, &u.Name) == nil {
					users = append(users, u)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				log.Printf("announce.targetNames users: %v", err)
			}
		}
	}
	return courses, users
}

// ============================================================================
// Upsert
// ============================================================================

// UpsertInput is the composer payload. Zero UUID means "create"; a real UUID
// updates in place. The service enforces category, audience, and scheduling
// invariants (expires_at > published_at, etc.) so a broken row can't land.
type UpsertInput struct {
	ID uuid.UUID `json:"id"`
	// Title/Body limits mirror the rune-count checks Upsert applies below
	// (200 / 8000) — the tag rejects an obviously-oversized payload before a
	// round trip; Upsert's own trimmed check still runs and stays authoritative.
	Title    string `json:"title" validate:"required,max=200"`
	Body     string `json:"body" validate:"required,max=8000"`
	Category string `json:"category" validate:"omitempty,oneof=info news warning urgent event"`
	// Empty audience deliberately means "everyone" (see normalizeAudience),
	// so this is not required — only non-empty values are constrained to the
	// four known roles.
	Audience      []string   `json:"audience" validate:"omitempty,dive,oneof=admin staff lecturer ta"`
	Pinned        bool       `json:"pinned"`
	CoverImageKey *string    `json:"cover_image_key" validate:"omitempty,startswith=announcements/"`
	PublishedAt   *time.Time `json:"published_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	// Pointers because this endpoint is a full-document upsert and several
	// callers send a document they rebuilt from a LIST row — which carries no
	// recipient ledger. A plain bool/slice would read as "clear it", so
	// pinning an announcement would silently switch off its share link and
	// delete everyone still queued for email. nil means "leave as it is".
	IsPublic *bool `json:"is_public"`
	// Targeting. Audience above is the role part; these narrow it further.
	// Recipients are always people who hold an account — an announcement is a
	// system notice, and a stranger's inbox is not somewhere it belongs.
	TargetCourseIDs *[]uuid.UUID `json:"target_course_ids"`
	TargetUserIDs   *[]uuid.UUID `json:"target_user_ids"`
	// TargetFilters is the same closed set validateRule checks (announce_target.go);
	// tagged here too so a bad filter name is rejected before Upsert runs.
	TargetFilters *[]string  `json:"target_filters" validate:"omitempty,dive,oneof=ta_missing_documents ta_missing_schedule ta_no_assignment lecturer_no_request lecturer_pending_worklog course_missing_schedule"`
	TargetTermID  *uuid.UUID `json:"target_term_id"`
	// Attachments replace the whole list when present; absent leaves it alone,
	// so a payload rebuilt from a list row cannot wipe the gallery. max=20
	// mirrors maxAttachments in announce_media.go.
	Attachments *[]AttachmentInput `json:"attachments" validate:"omitempty,max=20,dive"`
}

func (s *AnnounceService) Upsert(ctx context.Context, actor uuid.UUID, in UpsertInput) (uuid.UUID, error) {
	title := strings.TrimSpace(in.Title)
	body := strings.TrimSpace(in.Body)
	if title == "" {
		return uuid.Nil, errors.New("หัวข้อประกาศต้องไม่ว่าง")
	}
	if utf8.RuneCountInString(title) > 200 {
		return uuid.Nil, errors.New("หัวข้อประกาศยาวเกิน 200 ตัวอักษร")
	}
	if body == "" {
		return uuid.Nil, errors.New("เนื้อหาประกาศต้องไม่ว่าง")
	}
	if utf8.RuneCountInString(body) > 8000 {
		return uuid.Nil, errors.New("เนื้อหาประกาศยาวเกิน 8000 ตัวอักษร")
	}
	category := strings.TrimSpace(in.Category)
	if category == "" {
		category = "info"
	}
	if !validCategories[category] {
		return uuid.Nil, errors.New("หมวดหมู่ไม่ถูกต้อง")
	}
	// Audience: dedupe + validate. Roles may be empty: a rule can select by
	// course, by name, or by a condition alone. A rule with nothing in it selects
	// nobody, and is refused below the moment it would be published.
	aud := normalizeAudience(in.Audience)
	rule := AudienceRule{
		Roles:     aud,
		CourseIDs: derefSlice(in.TargetCourseIDs),
		UserIDs:   derefSlice(in.TargetUserIDs),
		Filters:   derefSlice(in.TargetFilters),
		TermID:    in.TargetTermID,
	}
	if err := validateRule(rule); err != nil {
		return uuid.Nil, err
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
		} else if !strings.HasPrefix(k, "announcements/") || strings.Contains(k, "..") {
			// ตรวจสองอย่างเหมือน saveAttachments (announce_media.go) — key
			// ชนิดเดียวกันต้องใช้กฎเดียวกัน · prefix อย่างเดียวไม่พอ เพราะ
			// "announcements/../.." ก็ผ่าน prefix
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
	var (
		oldAnnouncedAt, oldPublishedAt *time.Time
		oldRule                        AudienceRule
	)
	if !isNew {
		_ = s.pool.QueryRow(ctx, `SELECT announced_at, published_at FROM announcements WHERE id=$1`, in.ID).
			Scan(&oldAnnouncedAt, &oldPublishedAt)
		oldRule, _ = s.storedRule(ctx, s.pool, in.ID)
	}
	now := time.Now()
	// "Already out": published, and the first delivery has run.
	wasLive := oldAnnouncedAt != nil && oldPublishedAt != nil && !oldPublishedAt.After(now)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	if isNew {
		_, err = tx.Exec(ctx, `
			INSERT INTO announcements (
				id, title, body, category, audience, pinned,
				cover_image_key, published_at, expires_at, is_public,
				target_course_ids, target_user_ids, target_filters, target_term_id,
				created_by, updated_by
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15)
		`, in.ID, title, body, category, aud, in.Pinned,
			in.CoverImageKey, in.PublishedAt, in.ExpiresAt, in.IsPublic != nil && *in.IsPublic,
			nonNil(rule.CourseIDs), nonNil(rule.UserIDs), nonNil(rule.Filters), in.TargetTermID, actor)
	} else {
		tag, execErr := tx.Exec(ctx, `
			UPDATE announcements
			   SET title=$2, body=$3, category=$4, audience=$5, pinned=$6,
			       cover_image_key=$7, published_at=$8, expires_at=$9,
			       is_public=COALESCE($10, is_public),
			       target_course_ids=COALESCE($11, target_course_ids),
			       target_user_ids=COALESCE($12, target_user_ids),
			       target_filters=COALESCE($13, target_filters),
			       target_term_id=COALESCE($14, target_term_id),
			       updated_by=$15, updated_at=NOW()
			 WHERE id=$1
		`, in.ID, title, body, category, aud, in.Pinned,
			in.CoverImageKey, in.PublishedAt, in.ExpiresAt, in.IsPublic,
			in.TargetCourseIDs, in.TargetUserIDs, in.TargetFilters, in.TargetTermID, actor)
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
		wasLive = false
	}

	// Read the rule back as stored: a payload that left the target fields out
	// keeps the old ones, so the payload alone does not say who this is for.
	newRule, err := s.storedRule(ctx, tx, in.ID)
	if err != nil {
		return uuid.Nil, err
	}
	ruleChanged := isNew || !sameAudience(oldRule, newRule)
	// Publishing or scheduling to nobody is refused. Checked only when the
	// target is being decided — first publish, or a changed rule — so fixing a
	// typo in a notice whose condition has since emptied out is still allowed.
	if in.PublishedAt != nil && (!wasLive || ruleChanged) {
		if err := s.requireAudience(ctx, tx, in.ID, newRule); err != nil {
			return uuid.Nil, err
		}
	}

	if in.Attachments != nil {
		if err := s.saveAttachments(ctx, tx, in.ID, *in.Attachments); err != nil {
			return uuid.Nil, err
		}
	}

	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "announce.upsert", Entity: "announcement",
		EntityID: in.ID.String(), After: in,
	}); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}

	// What happens to the audience depends on what this save was:
	//
	//   going live            resolve the rule, freeze it, tell everyone
	//   live, rule changed    re-resolve: drop people no longer selected, tell
	//                         only the people newly selected
	//   live, rule untouched  nothing. Fixing a typo or pinning must not mail
	//                         anybody — it used to re-run the conditions on every
	//                         save and quietly email whoever newly matched.
	//
	// A row saved already past its expiry is not "going live": nobody could open
	// it, so nobody is told about it.
	expired := in.ExpiresAt != nil && !in.ExpiresAt.After(now)
	if in.PublishedAt != nil && !in.PublishedAt.After(now) && !(expired && !wasLive) {
		switch {
		case !wasLive:
			if err := s.syncAudience(ctx, in.ID); err != nil {
				log.Printf("announce.upsert sync %s: %v", in.ID, err)
			}
			s.fanout(ctx, in.ID)
		case ruleChanged:
			if err := s.syncAudience(ctx, in.ID); err != nil {
				log.Printf("announce.upsert sync %s: %v", in.ID, err)
			}
			if _, err := s.Deliver(ctx, in.ID); err != nil {
				log.Printf("announce.upsert deliver %s: %v", in.ID, err)
			}
		}
	}
	return in.ID, nil
}

// ============================================================================
// Delete / Publish / Unpublish
// ============================================================================

func (s *AnnounceService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "announce.delete", Entity: "announcement", EntityID: id.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM announcements WHERE id=$1`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		})
}

// Publish makes a draft or scheduled row live NOW and fires the fanout.
// Safe to call on a row that is already live — published_at is left alone and
// the fanout guard (announced_at) prevents re-notifying.
//
// Two things are refused rather than quietly "succeeding":
//   - an expired row: its expiry is still in the past, so it would stay hidden
//     while the page reported "เผยแพร่ประกาศแล้ว";
//   - a rule that reaches nobody.
func (s *AnnounceService) Publish(ctx context.Context, actor, id uuid.UUID) error {
	var expiresAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT expires_at FROM announcements WHERE id=$1`, id).Scan(&expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return Invalid("ประกาศนี้หมดอายุแล้ว กรุณาแก้ไขวันหมดอายุก่อนเผยแพร่อีกครั้ง")
	}
	rule, err := s.storedRule(ctx, s.pool, id)
	if err != nil {
		return err
	}
	if err := s.requireAudience(ctx, s.pool, id, rule); err != nil {
		return err
	}
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "announce.publish", Entity: "announcement", EntityID: id.String()},
		func(tx pgx.Tx) error {
			// A scheduled row keeps a FUTURE published_at; COALESCE alone left it
			// scheduled, so "publish now" on it did nothing.
			tag, err := tx.Exec(ctx, `
				UPDATE announcements
				   SET published_at = CASE WHEN published_at IS NULL OR published_at > NOW()
				                           THEN NOW() ELSE published_at END,
				       updated_by = $2, updated_at = NOW()
				 WHERE id = $1
			`, id, actor)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		}); err != nil {
		return err
	}
	// Resolve and freeze the audience first — the fanout and the email both
	// read that ledger, so nothing can be delivered before it exists. Skipped
	// for a row that is already out: its audience is frozen.
	var announcedAt *time.Time
	_ = s.pool.QueryRow(ctx, `SELECT announced_at FROM announcements WHERE id=$1`, id).Scan(&announcedAt)
	if announcedAt == nil {
		if err := s.syncAudience(ctx, id); err != nil {
			log.Printf("announce.publish sync %s: %v", id, err)
		}
	}
	// After the commit on purpose: the fanout sends mail and notifications, and
	// those cannot be rolled back if the transaction later fails.
	s.fanout(ctx, id)
	return nil
}

// Unpublish demotes a live announcement back to draft. Notifications that
// already went out are left in place — the recipient's inbox is a log, not
// a mirror of the current state.
func (s *AnnounceService) Unpublish(ctx context.Context, actor, id uuid.UUID) error {
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "announce.unpublish", Entity: "announcement", EntityID: id.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
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
			return nil
		})
}

// ============================================================================
// Fanout — the "how do recipients actually see this" side
// ============================================================================

// fanout resolves the audience and delivers to it. Idempotent per announcement
// via `announced_at`; per person via the ledger's status.
func (s *AnnounceService) fanout(ctx context.Context, id uuid.UUID) {
	if s.notify == nil {
		return
	}
	var announcedAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT announced_at FROM announcements WHERE id = $1`, id).Scan(&announcedAt); err != nil {
		log.Printf("announce.fanout lookup: %v", err)
		return
	}
	if announcedAt != nil {
		return // already announced; a later Deliver still picks up new people
	}
	if _, err := s.Deliver(ctx, id); err != nil {
		log.Printf("announce.fanout deliver %s: %v", id, err)
		return
	}
	if _, err := s.pool.Exec(ctx, `UPDATE announcements SET announced_at = NOW() WHERE id = $1`, id); err != nil {
		log.Printf("announce.fanout mark: %v", err)
	}
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
		if err := rows.Scan(&id); err != nil {
			log.Printf("announce.fanout scan: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		log.Printf("announce.fanout rows: %v", err)
	}
	for _, id := range ids {
		if err := s.syncAudience(ctx, id); err != nil {
			log.Printf("announce.sweep sync %s: %v", id, err)
		}
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
