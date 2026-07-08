package audit

import (
	"context"
	"encoding/json"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	ActorID   *uuid.UUID
	ActorRole string
	Action    string
	Entity    string
	EntityID  string
	IP        string
	UserAgent string
	Before    any
	After     any
	Note      string
}

type Auditor struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Auditor { return &Auditor{pool: pool} }

func (a *Auditor) Log(ctx context.Context, e Entry) {
	var before, after []byte
	if e.Before != nil {
		before, _ = json.Marshal(e.Before)
	}
	if e.After != nil {
		after, _ = json.Marshal(e.After)
	}
	var ip *string
	if e.IP != "" {
		ip = &e.IP
	}
	var role *string
	if e.ActorRole != "" {
		role = &e.ActorRole
	}
	_, err := a.pool.Exec(ctx,
		`INSERT INTO audit_logs (actor_id, actor_role, action, entity, entity_id, ip, user_agent, before, after, note)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.ActorID, role, e.Action, e.Entity, nilIfEmpty(e.EntityID), ip, nilIfEmpty(e.UserAgent), before, after, nilIfEmpty(e.Note))
	if err != nil {
		log.Printf("audit: %v", err)
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
