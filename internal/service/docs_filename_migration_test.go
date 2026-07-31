package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// Migration 0050 reimplements taFileStem in SQL, because a migration cannot
// call Go. Duplicated logic drifts silently, and the symptom here would be
// half the officer's folder named one way and half the other — the exact mess
// 0050 exists to clean up.
//
// So this runs the real migration file against rows built for the purpose and
// compares what SQL produced against what the Go helper produces. Re-running
// the file is safe: it creates its helper functions, updates only rows still
// carrying the old name, and drops the helpers again.
func TestMigration0050_MatchesGoFilenameHelper(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	sqlPath := filepath.Join(repoRoot(t), "migrations",
		"0050_rename_creditor_form_files.up.sql")
	body, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	cases := []struct {
		name                   string
		studentID, first, last string
	}{
		{"plain", "653020123-4", "สมชาย", "ใจดี"},
		{"no student id", "", "สมชาย", "ใจดี"},
		{"spaces in name", "653020123-4", "สม ชาย", "ใจ ดี"},
		{"slash in name", "653020123-4", "สมชาย", "ใจดี/ทดสอบ"},
		{"latin name", "653020999-1", "Somchai", "Jaidee"},
	}

	type want struct {
		docID    uuid.UUID
		expected string
	}
	wants := make([]want, 0, len(cases))

	for _, c := range cases {
		userID := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, student_id, is_active)
			 VALUES ($1, $2, $3, $4, NULLIF($5,''), TRUE)`,
			userID, "mig0050-"+userID.String()+"@example.test",
			c.first, c.last, c.studentID); err != nil {
			t.Fatalf("%s: insert user: %v", c.name, err)
		}

		// The filename exactly as the pre-0050 Go code wrote it.
		oldName := "creditor_form_" + replaceSpaces(c.first+"_"+c.last) + ".pdf"
		docID := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO ta_documents
			  (id, user_id, kind, filename, mime, size_bytes, storage_key, status)
			VALUES ($1,$2,'creditor_form',$3,'application/pdf',1,$4,'submitted')`,
			docID, userID, oldName, "key/"+docID.String()); err != nil {
			t.Fatalf("%s: insert doc: %v", c.name, err)
		}
		wants = append(wants, want{
			docID:    docID,
			expected: taFileStem(c.studentID, c.first, c.last) + ".pdf",
		})
	}

	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("run migration 0050: %v", err)
	}

	for i, w := range wants {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT filename FROM ta_documents WHERE id = $1`, w.docID).Scan(&got); err != nil {
			t.Fatalf("%s: read back: %v", cases[i].name, err)
		}
		if got != w.expected {
			t.Errorf("%s: SQL produced %q, Go taFileStem produces %q — the migration has drifted from internal/service/docs.go",
				cases[i].name, got, w.expected)
		}
	}
}

// A file the TA named themselves is not ours to rewrite, even when it happens
// to start with the same prefix. Without the exact-match guard this row would
// be silently renamed.
func TestMigration0050_LeavesHandUploadedNamesAlone(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, student_id, is_active)
		 VALUES ($1, $2, 'สมชาย', 'ใจดี', '653020123-4', TRUE)`,
		userID, "mig0050-keep-"+userID.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	const handName = "creditor_form_ที่ผมกรอกเอง.pdf"
	docID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO ta_documents
		  (id, user_id, kind, filename, mime, size_bytes, storage_key, status)
		VALUES ($1,$2,'creditor_form',$3,'application/pdf',1,$4,'submitted')`,
		docID, userID, handName, "key/"+docID.String()); err != nil {
		t.Fatalf("insert doc: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(repoRoot(t), "migrations",
		"0050_rename_creditor_form_files.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("run migration 0050: %v", err)
	}

	var got string
	if err := pool.QueryRow(ctx,
		`SELECT filename FROM ta_documents WHERE id = $1`, docID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != handName {
		t.Fatalf("hand-uploaded filename was rewritten to %q; it must stay %q", got, handName)
	}
}

// replaceSpaces mirrors the pre-0050 builder: spaces only, nothing else.
func replaceSpaces(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r == ' ' {
			out[i] = '_'
		}
	}
	return string(out)
}
