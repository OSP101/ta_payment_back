// Command rotate-pii-key re-encrypts every ta_profiles.citizen_id_enc and
// ta_profiles.payee_enc value under a new PII_ENC_KEY.
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

// sealedColumn is one PII_ENC_KEY-encrypted column on ta_profiles. Both are
// rotated in the same run and the same transaction, so the table is never left
// with the two columns under different keys. Column names are literals, never
// input, so interpolating them into SQL is safe.
type sealedColumn struct {
	name, version string
	aad           func(uuid.UUID) []byte
}

var sealedColumns = []sealedColumn{
	// AAD = user_id (internal/service/citizen_id.go storeCitizenID).
	{"citizen_id_enc", "citizen_id_key_version", func(id uuid.UUID) []byte { return append([]byte{}, id[:]...) }},
	// AAD = user_id || "payee" (internal/service/payee.go payeeAAD).
	{"payee_enc", "payee_key_version", func(id uuid.UUID) []byte {
		return append(append([]byte{}, id[:]...), "payee"...)
	}},
}

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
		for _, col := range sealedColumns {
			var maxVersion int
			if err := pool.QueryRow(ctx, fmt.Sprintf(
				`SELECT COALESCE(MAX(%s), 0) FROM ta_profiles WHERE %s IS NOT NULL`, col.version, col.name),
			).Scan(&maxVersion); err != nil {
				log.Fatalf("read current max %s: %v", col.version, err)
			}
			if *newVersion <= maxVersion {
				log.Fatalf("-new-version=%d must be greater than the highest %s already on the table (%d)",
					*newVersion, col.version, maxVersion)
			}
		}
	}

	type work struct {
		col   sealedColumn
		rows  []row
		reenc [][]byte
	}
	var jobs []work
	for _, col := range sealedColumns {
		rows, err := pool.Query(ctx, fmt.Sprintf(
			`SELECT user_id, %s, COALESCE(%s, 0) FROM ta_profiles WHERE %s IS NOT NULL`,
			col.name, col.version, col.name))
		if err != nil {
			log.Fatalf("query %s: %v", col.name, err)
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
			log.Fatalf("query %s: %v", col.name, err)
		}
		fmt.Printf("%s: %d row(s) on file\n", col.name, len(todo))

		// Decrypt-verify every row with the OLD key BEFORE writing anything —
		// AAD-bound to the row's own user_id (see storeCitizenID / storePayee),
		// so this also confirms no row was ever copied between users. A single
		// bad row aborts the whole run: partial re-encryption would leave the
		// table split across two keys with no record of which rows got done,
		// which is worse than doing nothing.
		reenc := make([][]byte, len(todo))
		for i, r := range todo {
			plain, err := oldCipher.Open(col.aad(r.userID), r.sealed)
			if err != nil {
				log.Fatalf("%s: decrypt failed for user %s (row %d/%d) — aborting, nothing written: %v",
					col.name, r.userID, i+1, len(todo), err)
			}
			sealed, err := newCipher.Seal(col.aad(r.userID), plain)
			// Overwrite the plaintext buffer now that we're done with it — best
			// effort only (Go's GC can still have moved/copied it), but there is
			// no reason to let it sit in memory a moment longer than needed.
			for j := range plain {
				plain[j] = 0
			}
			if err != nil {
				log.Fatalf("%s: re-encrypt failed for user %s (row %d/%d) — aborting, nothing written: %v",
					col.name, r.userID, i+1, len(todo), err)
			}
			reenc[i] = sealed
		}
		jobs = append(jobs, work{col: col, rows: todo, reenc: reenc})
	}
	fmt.Println("decrypted and re-encrypted every row successfully")

	if !*apply {
		fmt.Println("dry run — nothing written. Re-run with -apply -new-version=N once this looks right.")
		return
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	total := 0
	for _, j := range jobs {
		for i, r := range j.rows {
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				`UPDATE ta_profiles SET %s = $1, %s = $2 WHERE user_id = $3`, j.col.name, j.col.version),
				j.reenc[i], *newVersion, r.userID); err != nil {
				log.Fatalf("%s: update failed for user %s (row %d/%d) — rolling back, nothing written: %v",
					j.col.name, r.userID, i+1, len(j.rows), err)
			}
		}
		total += len(j.rows)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}
	fmt.Printf("done — %d value(s) now under key version %d. "+
		"Update PII_ENC_KEY to the new key's value and restart the service.\n", total, *newVersion)
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}
