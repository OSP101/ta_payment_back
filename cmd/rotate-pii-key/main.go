// Command rotate-pii-key re-encrypts every ta_profiles.citizen_id_enc value
// under a new PII_ENC_KEY.
//
// This exists because internal/pii.Cipher only ever holds ONE key — Open()
// simply fails against ciphertext sealed under a different one. Swapping
// PII_ENC_KEY in production without running this first does not "rotate" the
// key, it silently breaks every TA's transfer-cover document generation the
// next time RevealCitizenID is called, with no error until an officer notices
// a form comes out wrong. citizen_id_key_version exists on the table (see
// migration 0076) specifically so this tool has somewhere to record which key
// encrypted a row — but nothing ever populated it beyond the constant 1 until
// this command.
//
// Usage:
//
//	# 1. Dry run first — decrypts every row with OLD_PII_ENC_KEY and reports
//	#    success/failure, writes nothing. Confirms the old key is right
//	#    before anything is touched.
//	OLD_PII_ENC_KEY=... NEW_PII_ENC_KEY=... DATABASE_URL=... \
//	  go run ./cmd/rotate-pii-key
//
//	# 2. Apply — decrypts with the old key, re-encrypts with the new one,
//	#    writes every row in ONE transaction (all-or-nothing).
//	OLD_PII_ENC_KEY=... NEW_PII_ENC_KEY=... DATABASE_URL=... \
//	  go run ./cmd/rotate-pii-key -apply -new-version=2
//
//	# 3. Only once every row is confirmed rotated (check the row count this
//	#    tool reports against `SELECT count(*) FROM ta_profiles WHERE
//	#    citizen_id_key_version=2`), update PII_ENC_KEY in the running
//	#    service's environment to NEW_PII_ENC_KEY's value and restart it.
//	#    Doing this BEFORE the rotation finishes would make the live service
//	#    unable to read the rows this tool has not gotten to yet.
//
// -new-version must be higher than every version currently on the table —
// the tool refuses to run otherwise, so a mistyped or reused version number
// cannot silently mix two keys' rows together under one label.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/google/uuid"

	"ta-payment-back/internal/db"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/storage"
)

type row struct {
	userID  uuid.UUID
	sealed  []byte
	version int
}

func main() {
	apply := flag.Bool("apply", false, "write the re-encrypted values (default: dry run — decrypt-verify only, no writes)")
	newVersion := flag.Int("new-version", 0, "key version to record for rows re-encrypted under NEW_PII_ENC_KEY; required with -apply, must exceed every version already on the table")
	flag.Parse()

	oldKey, err := storage.ParseKeyFromBase64(mustEnv("OLD_PII_ENC_KEY"))
	if err != nil {
		log.Fatalf("OLD_PII_ENC_KEY: %v", err)
	}
	newKey, err := storage.ParseKeyFromBase64(mustEnv("NEW_PII_ENC_KEY"))
	if err != nil {
		log.Fatalf("NEW_PII_ENC_KEY: %v", err)
	}
	oldCipher, err := pii.New(oldKey)
	if err != nil {
		log.Fatalf("old cipher: %v", err)
	}
	newCipher, err := pii.New(newKey)
	if err != nil {
		log.Fatalf("new cipher: %v", err)
	}

	if *apply && *newVersion <= 0 {
		log.Fatal("-new-version is required (and must be > 0) with -apply")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	if *apply {
		var maxVersion int
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(MAX(citizen_id_key_version), 0) FROM ta_profiles WHERE citizen_id_enc IS NOT NULL`,
		).Scan(&maxVersion); err != nil {
			log.Fatalf("read current max version: %v", err)
		}
		if *newVersion <= maxVersion {
			log.Fatalf("-new-version=%d must be greater than the highest version already on the table (%d)",
				*newVersion, maxVersion)
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT user_id, citizen_id_enc, COALESCE(citizen_id_key_version, 0)
		 FROM ta_profiles WHERE citizen_id_enc IS NOT NULL`)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.userID, &r.sealed, &r.version); err != nil {
			rows.Close()
			log.Fatalf("scan: %v", err)
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Fatalf("query: %v", err)
	}

	fmt.Printf("%d row(s) with a citizen id on file\n", len(todo))

	// Decrypt-verify every row with the OLD key BEFORE writing anything —
	// AAD-bound to the row's own user_id (see internal/service/citizen_id.go's
	// storeCitizenID), so this also confirms no row was ever copied between
	// users. A single bad row aborts the whole run: partial re-encryption
	// would leave the table split across two keys with no record of which
	// rows got done, which is worse than doing nothing.
	reenc := make([][]byte, len(todo))
	for i, r := range todo {
		plain, err := oldCipher.Open(r.userID[:], r.sealed)
		if err != nil {
			log.Fatalf("decrypt failed for user %s (row %d/%d) — aborting, nothing written: %v",
				r.userID, i+1, len(todo), err)
		}
		sealed, err := newCipher.Seal(r.userID[:], plain)
		// Overwrite the plaintext buffer now that we're done with it — best
		// effort only (Go's GC can still have moved/copied it), but there is
		// no reason to let it sit in memory a moment longer than needed.
		for j := range plain {
			plain[j] = 0
		}
		if err != nil {
			log.Fatalf("re-encrypt failed for user %s (row %d/%d) — aborting, nothing written: %v",
				r.userID, i+1, len(todo), err)
		}
		reenc[i] = sealed
	}
	fmt.Printf("decrypted and re-encrypted all %d row(s) successfully\n", len(todo))

	if !*apply {
		fmt.Println("dry run — nothing written. Re-run with -apply -new-version=N once this looks right.")
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for i, r := range todo {
		if _, err := tx.Exec(ctx,
			`UPDATE ta_profiles SET citizen_id_enc = $1, citizen_id_key_version = $2 WHERE user_id = $3`,
			reenc[i], *newVersion, r.userID); err != nil {
			log.Fatalf("update failed for user %s (row %d/%d) — rolling back, nothing written: %v",
				r.userID, i+1, len(todo), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Printf("done — %d row(s) now under key version %d. "+
		"Update PII_ENC_KEY to the new key's value and restart the service.\n", len(todo), *newVersion)
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}
