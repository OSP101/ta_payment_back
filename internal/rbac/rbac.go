package rbac

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	RoleAdmin    = "admin"
	RoleStaff    = "staff"
	RoleLecturer = "lecturer"
	RoleTA       = "ta"
)

type RBAC struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *RBAC { return &RBAC{pool: pool} }

func (r *RBAC) Roles(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT role::text FROM user_roles WHERE user_id = $1 ORDER BY role`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func Has(roles []string, want ...string) bool {
	set := map[string]bool{}
	for _, r := range roles {
		set[r] = true
	}
	for _, w := range want {
		if set[w] {
			return true
		}
	}
	return false
}
