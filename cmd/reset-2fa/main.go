// Command reset-2fa is the break-glass path for two-factor authentication:
// clears a single user's TOTP secret, pending secret, and every recovery
// code, directly against the database, with no login and no app process
// involved.
//
// This exists because admin/staff/executive 2FA is mandatory (see migration
// 0091 and internal/handler/middleware.go's AccountGuard), and
// POST /users/:id/2fa/reset is deliberately admin-only and requires the
// ACTING admin's own password (see MFAService.AdminReset's doc comment on
// why staff cannot be trusted with it). Put those two facts together and a
// deployment with exactly one admin, whose phone and recovery codes are both
// gone, has no in-app way back in — every screen 403s to /setup-2fa, and
// there is no admin left who can call the reset endpoint on their behalf.
// This tool is the only remaining door, which is why it deliberately does
// NOT go through the API, the audit log, or any in-app authorization check:
// by the time someone needs it, none of those are reachable. Whoever holds
// DATABASE_URL already holds the keys to the whole system regardless.
//
// Usage:
//
//	# 1. Dry run first (default) — reports the account's current 2FA state,
//	#    writes nothing.
//	DATABASE_URL=... go run ./cmd/reset-2fa -email=admin@example.com
//
//	# 2. Apply — clears totp_secret_enc, totp_pending_secret_enc,
//	#    totp_enabled_at, totp_last_step, totp_key_version, and deletes every
//	#    row in mfa_recovery_codes for this user, in one transaction.
//	DATABASE_URL=... go run ./cmd/reset-2fa -email=admin@example.com -apply
//
// After running: the account can log in with just its password and will be
// routed straight to /setup-2fa to enrol again, same as any other admin/
// staff account with no TOTP on file. Every existing session for the
// account is left untouched — this tool does not require the app to be
// running, so it cannot call SessionService.RevokeAllForUser. Log the
// account out from the users screen afterward if that matters.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/db"
)

func main() {
	email := flag.String("email", "", "email of the account to clear 2FA for (required)")
	apply := flag.Bool("apply", false, "actually clear the account's 2FA (default: dry run — report only)")
	flag.Parse()

	if *email == "" {
		log.Fatal("-email is required")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	var id uuid.UUID
	var enabled bool
	var codeCount int
	err = pool.QueryRow(ctx, `
		SELECT u.id, u.totp_enabled_at IS NOT NULL,
		       (SELECT COUNT(*) FROM mfa_recovery_codes r WHERE r.user_id = u.id AND r.used_at IS NULL)
		FROM users u WHERE u.email = $1 AND u.deleted_at IS NULL`,
		*email).Scan(&id, &enabled, &codeCount)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Fatalf("no account found for %s", *email)
	}
	if err != nil {
		log.Fatalf("lookup: %v", err)
	}

	fmt.Printf("account: %s (id %s)\n", *email, id)
	fmt.Printf("2FA enabled: %v\n", enabled)
	fmt.Printf("unused recovery codes: %d\n", codeCount)

	if !enabled && codeCount == 0 {
		fmt.Println("nothing to clear — this account already has no 2FA on file.")
		return
	}

	if !*apply {
		fmt.Println("dry run — nothing written. Re-run with -apply to clear this account's 2FA.")
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET totp_secret_enc = NULL, totp_pending_secret_enc = NULL,
		    totp_enabled_at = NULL, totp_last_step = NULL, totp_key_version = NULL
		WHERE id = $1`, id); err != nil {
		log.Fatalf("clear users row: %v", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, id)
	if err != nil {
		log.Fatalf("delete recovery codes: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Printf("done — cleared 2FA for %s, deleted %d recovery code row(s). "+
		"The account can now log in with just its password and will be routed to /setup-2fa.\n",
		*email, tag.RowsAffected())
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}
