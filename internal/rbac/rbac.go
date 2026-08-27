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
	// RoleExecutive is synthetic: it is derived from holding an active
	// admin_officers row, not a role_code enum value (ALTER TYPE ... ADD
	// VALUE cannot be used in the migration transaction that adds it — see
	// migration 0041/0068) and not a column on users any more (see migration
	// 0100). The management team are lecturers appointed to an administrative
	// seat via /settings/admin-officers; the role grants ONLY the read-only
	// budget-analytics endpoints.
	RoleExecutive = "executive"
)

type RBAC struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *RBAC { return &RBAC{pool: pool} }

func (r *RBAC) Roles(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT role::text FROM user_roles WHERE user_id = $1
		 UNION ALL
		 SELECT 'executive' FROM admin_officers WHERE user_id = $1 AND is_active
		 ORDER BY 1`, userID)
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
