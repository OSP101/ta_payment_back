package service

// announce_media.go: the files an announcement carries, and the rule for who
// may fetch them.
//
// The second half matters more than it looks. An announcement opened for public
// sharing is read by people with no account, so its pictures have to be
// fetchable without one — but ONLY those. MediaIsPublic is the single place
// that decides, and it answers by key, because that is all a fetch request has.

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AnnouncementAttachment is one file on an announcement.
type AnnouncementAttachment struct {
	ID         uuid.UUID `json:"id"`
	Kind       string    `json:"kind"` // image | video | file
	StorageKey string    `json:"storage_key"`
	URL        string    `json:"url"`
	Filename   string    `json:"filename"`
	Mime       string    `json:"mime"`
	SizeBytes  int64     `json:"size_bytes"`
}

// AttachmentInput is what the composer submits: keys it already uploaded, in
// the order it wants them shown.
type AttachmentInput struct {
	Kind       string `json:"kind"`
	StorageKey string `json:"storage_key"`
	Filename   string `json:"filename"`
	Mime       string `json:"mime"`
	SizeBytes  int64  `json:"size_bytes"`
}

var validAttachmentKinds = map[string]bool{"image": true, "video": true, "file": true}

// maxAttachments bounds one announcement. Not a technical limit — a notice
// with fifty files is a folder, and the feed cannot render it usefully.
const maxAttachments = 20

// AttachmentURL is where a client fetches the bytes. The public route serves
// the same keys and applies its own visibility check, so one URL works for
// both readers and anonymous visitors.
func AttachmentURL(key string) string {
	return "/api/v1/announcements/media/" + key
}

// saveAttachments replaces the attachment list for an announcement.
//
// Replacement rather than merge: the composer owns the whole list including
// its order, and a merge would make removing a file impossible to express.
// Rows are deleted and rewritten inside the caller's transaction, so a failed
// save cannot leave a half-updated gallery.
func (s *AnnounceService) saveAttachments(
	ctx context.Context, tx pgx.Tx, id uuid.UUID, in []AttachmentInput,
) error {
	if len(in) > maxAttachments {
		return Invalid("แนบไฟล์ได้ไม่เกิน 20 ไฟล์ต่อหนึ่งประกาศ")
	}
	for _, a := range in {
		if !validAttachmentKinds[a.Kind] {
			return Invalid("ประเภทไฟล์แนบไม่ถูกต้อง")
		}
		// The key is what the serve route will open, so it must be one this
		// service produced. Anything else is an attempt to read the store.
		if !strings.HasPrefix(a.StorageKey, "announcements/") || strings.Contains(a.StorageKey, "..") {
			return Invalid("ไฟล์แนบไม่ถูกต้อง")
		}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM announcement_attachments WHERE announcement_id = $1`, id); err != nil {
		return err
	}
	for i, a := range in {
		if _, err := tx.Exec(ctx, `
			INSERT INTO announcement_attachments
			  (announcement_id, kind, storage_key, filename, mime, size_bytes, sort_order)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, a.Kind, a.StorageKey, truncateRunes(a.Filename, 200), a.Mime, a.SizeBytes, i); err != nil {
			return err
		}
	}
	return nil
}

// loadAttachments reads one announcement's files in composer order.
func (s *AnnounceService) loadAttachments(ctx context.Context, id uuid.UUID) ([]AnnouncementAttachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, storage_key, filename, mime, size_bytes
		  FROM announcement_attachments
		 WHERE announcement_id = $1
		 ORDER BY sort_order, created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnouncementAttachment{}
	for rows.Next() {
		var a AnnouncementAttachment
		if err := rows.Scan(&a.ID, &a.Kind, &a.StorageKey, &a.Filename, &a.Mime, &a.SizeBytes); err != nil {
			return nil, err
		}
		a.URL = AttachmentURL(a.StorageKey)
		out = append(out, a)
	}
	return out, rows.Err()
}

// loadAttachmentsFor fetches attachments for many announcements at once, so a
// feed of twenty posts costs one query rather than twenty.
func (s *AnnounceService) loadAttachmentsFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]AnnouncementAttachment, error) {
	out := map[uuid.UUID][]AnnouncementAttachment{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT announcement_id, id, kind, storage_key, filename, mime, size_bytes
		  FROM announcement_attachments
		 WHERE announcement_id = ANY($1)
		 ORDER BY sort_order, created_at`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var annID uuid.UUID
		var a AnnouncementAttachment
		if err := rows.Scan(&annID, &a.ID, &a.Kind, &a.StorageKey, &a.Filename, &a.Mime, &a.SizeBytes); err != nil {
			return nil, err
		}
		a.URL = AttachmentURL(a.StorageKey)
		out[annID] = append(out[annID], a)
	}
	return out, rows.Err()
}

// MediaIsPublic reports whether a stored key may be served without an account.
//
// True only when the key belongs to an announcement that staff opened for
// public sharing AND that is live right now — the same test PublicGet applies
// to the text. Expiring or un-publishing a notice therefore takes its pictures
// down with it, which is what "ถอนประกาศ" has to mean.
func (s *AnnounceService) MediaIsPublic(ctx context.Context, key string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM announcements a
			 WHERE a.is_public
			   AND a.published_at IS NOT NULL AND a.published_at <= NOW()
			   AND (a.expires_at IS NULL OR a.expires_at > NOW())
			   AND (
			     a.cover_image_key = $1
			     OR EXISTS (SELECT 1 FROM announcement_attachments t
			                 WHERE t.announcement_id = a.id AND t.storage_key = $1)
			   )
		)`, key).Scan(&ok)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return ok, nil
}
