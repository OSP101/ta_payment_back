package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// Backgrounds for the cover maker. The picture itself lives in storage under
// "announcements/…" so it is served by the same ServeImage route as covers.

// Cover maker canvas, and so the minimum a background must cover. 16:9.
const (
	BackgroundMinWidth  = 1600
	BackgroundMinHeight = 900
	// How far from exact 16:9 a background may be. 1% lets 1672×941 (the
	// faculty's own template) through while refusing a 4:3 photo, which would
	// lose a sixth of itself to the crop.
	backgroundAspectSlack = 0.01
)

// AnnouncementBackground is one uploaded background.
type AnnouncementBackground struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	TextTone  string    `json:"text_tone"`
	CreatedAt time.Time `json:"created_at"`
}

// CheckBackgroundSize says why an image cannot be a background, or nil.
func CheckBackgroundSize(w, h int) error {
	if w < BackgroundMinWidth || h < BackgroundMinHeight {
		return Invalid("รูปพื้นหลังต้องมีขนาดอย่างน้อย 1600×900 พิกเซล (รูปนี้ " + strconv.Itoa(w) + "×" + strconv.Itoa(h) + ")")
	}
	ratio := float64(w) / float64(h)
	want := 16.0 / 9.0
	if d := (ratio - want) / want; d > backgroundAspectSlack || d < -backgroundAspectSlack {
		return Invalid("รูปพื้นหลังต้องเป็นสัดส่วน 16:9 เช่น 1600×900 หรือ 1920×1080 (รูปนี้ " + strconv.Itoa(w) + "×" + strconv.Itoa(h) + ")")
	}
	return nil
}

func (s *AnnounceService) ListBackgrounds(ctx context.Context) ([]AnnouncementBackground, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, storage_key, width, height, text_tone, created_at
		  FROM announcement_backgrounds ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnouncementBackground{}
	for rows.Next() {
		var b AnnouncementBackground
		var key string
		if err := rows.Scan(&b.ID, &b.Name, &key, &b.Width, &b.Height, &b.TextTone, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.URL = "/api/v1/announcements/images/" + key
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddBackground records an already-stored, already-checked picture.
func (s *AnnounceService) AddBackground(ctx context.Context, actor uuid.UUID, name, key string, w, h int, tone string) (*AnnouncementBackground, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, Invalid("กรุณาตั้งชื่อพื้นหลัง")
	}
	if utf8.RuneCountInString(name) > 100 {
		return nil, Invalid("ชื่อพื้นหลังยาวเกิน 100 ตัวอักษร")
	}
	if tone != "light" {
		tone = "dark"
	}
	if err := CheckBackgroundSize(w, h); err != nil {
		return nil, err
	}
	b := &AnnouncementBackground{ID: uuid.New(), Name: name, Width: w, Height: h, TextTone: tone,
		URL: "/api/v1/announcements/images/" + key}
	err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "announce.background_add", Entity: "announcement_background",
			EntityID: b.ID.String(), After: map[string]any{"name": name, "width": w, "height": h}},
		func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO announcement_backgrounds (id, name, storage_key, width, height, text_tone, created_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`,
				b.ID, name, key, w, h, tone, actor).Scan(&b.CreatedAt)
		})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// DeleteBackground removes a background and returns its storage key so the
// caller can remove the file. Covers already made from it are separate files
// and are not affected.
func (s *AnnounceService) DeleteBackground(ctx context.Context, actor, id uuid.UUID) (string, error) {
	var key string
	err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "announce.background_delete", Entity: "announcement_background", EntityID: id.String()},
		func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `DELETE FROM announcement_backgrounds WHERE id=$1 RETURNING storage_key`, id).Scan(&key)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		})
	return key, err
}
