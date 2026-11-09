package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

func TestConcurrentFirstLoginsLeaveOneSession(t *testing.T) {
	f := newAnnFixture(t)
	user := f.user("ta", "session-test")
	svc := &SessionService{pool: f.svc.pool}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	errors := make(chan error, 8)
	for i := 0; i < cap(errors); i++ {
		go func() {
			<-start
			_, err := svc.CreateAndSupersede(ctx, user, time.Hour, "127.0.0.1", "test")
			errors <- err
		}()
	}
	close(start)
	for i := 0; i < cap(errors); i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.svc.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id=$1 AND revoked_at IS NULL`, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active sessions=%d, want 1", count)
	}
}

func TestRoleChangeAndSessionRevocationCommitTogether(t *testing.T) {
	f := newAnnFixture(t)
	admin, user := f.user("admin", "admin"), f.user("staff", "target")
	sessions := &SessionService{pool: f.svc.pool}
	users := &UserService{pool: f.svc.pool, aud: audit.New(f.svc.pool)}
	sid, err := sessions.CreateAndSupersede(f.ctx, user, time.Hour, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{"ta"}
	// Fail the audit AFTER both writes; neither may survive that failure.
	f.exec(`ALTER TABLE audit_logs ADD CONSTRAINT fail_role_audit CHECK(action <> 'user.update')`)
	if _, err := users.Update(f.ctx, admin, user, UpdateUserInput{Roles: &roles}); err == nil {
		t.Fatal("expected audit failure")
	}
	var role string
	var revoked bool
	if err := f.svc.pool.QueryRow(f.ctx, `SELECT role::text FROM user_roles WHERE user_id=$1`, user).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.pool.QueryRow(f.ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE id=$1`, sid).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if role != "staff" || revoked {
		t.Fatal("failed role change partially committed")
	}
	f.exec(`ALTER TABLE audit_logs DROP CONSTRAINT fail_role_audit`)
	if _, err := users.Update(f.ctx, admin, user, UpdateUserInput{Roles: &roles}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.pool.QueryRow(f.ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE id=$1`, sid).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("role change did not revoke session: %v", err)
	}
}

func TestCreateUserAuditDoesNotStoreSuppliedPassword(t *testing.T) {
	f := newAnnFixture(t)
	actor := f.user("admin", "creator")
	users := &UserService{pool: f.svc.pool, aud: audit.New(f.svc.pool)}
	password := "Audit-only!73_river_Q9"
	created, err := users.Create(f.ctx, actor, CreateUserInput{
		Email: uuid.NewString()+"@example.test", FirstName: "Audit", LastName: "Test",
		Roles: []string{"ta"}, Password: &password,
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.svc.pool.QueryRow(f.ctx, `SELECT after FROM audit_logs WHERE action='user.create' AND entity_id=$1`, created.User.ID.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if _, exists := record["password"]; exists || strings.Contains(string(raw), password) {
		t.Fatal("creation audit retained the supplied credential")
	}
	if record["email"] != created.User.Email || record["roles"] == nil {
		t.Fatal("creation audit lost identity or roles")
	}
}
