package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

type Announcement struct {
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Audience    []string   `json:"audience"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

type AnnounceService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

func (s *AnnounceService) List(ctx context.Context, roleFilter string) ([]Announcement, error) {
	q := `SELECT id, title, body, audience, published_at FROM announcements`
	args := []any{}
	if roleFilter != "" {
		q += ` WHERE $1 = ANY(audience) AND published_at IS NOT NULL`
		args = append(args, roleFilter)
	}
	q += ` ORDER BY COALESCE(published_at, created_at) DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Audience, &a.PublishedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *AnnounceService) Upsert(ctx context.Context, actor uuid.UUID, in Announcement) (uuid.UUID, error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		_, err := s.pool.Exec(ctx,
			`INSERT INTO announcements (id, title, body, audience, published_at, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			in.ID, in.Title, in.Body, in.Audience, in.PublishedAt, actor)
		if err != nil {
			return uuid.Nil, err
		}
	} else {
		_, err := s.pool.Exec(ctx,
			`UPDATE announcements SET title=$2, body=$3, audience=$4, published_at=$5, updated_at=NOW() WHERE id=$1`,
			in.ID, in.Title, in.Body, in.Audience, in.PublishedAt)
		if err != nil {
			return uuid.Nil, err
		}
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "announce.upsert", Entity: "announcement", EntityID: in.ID.String(), After: in})
	return in.ID, nil
}
