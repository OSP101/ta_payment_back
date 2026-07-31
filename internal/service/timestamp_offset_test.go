package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// Regression guard for the truncated UTC-offset bug.
//
// Timestamps leave this system as pre-formatted strings built by TO_CHAR, and
// the templates used to end in `OF` or `TZ`. Under the session zone the app
// pins (Asia/Bangkok — see db.Connect) both of those render a whole-hour
// offset with no minutes: "2026-07-31T17:35:18+07".
//
// That is legal ISO 8601 but NOT legal RFC 3339, and JavaScript's Date parser
// implements the RFC 3339 subset — it requires "+07:00". So `new Date(iso)`
// returned an Invalid Date and every notification row rendered the literal
// text "Invalid Date". The failure is quiet on the JS side too, because
// toLocaleString on an Invalid Date returns that string instead of throwing,
// so the frontend's try/catch never fired.
//
// The templates now end in `TZH:TZM`, which always emits the minutes. These
// tests pin both halves of the guarantee: that the value a real query produces
// parses as RFC 3339, and that no one reintroduces a truncated template
// anywhere in the tree.

// TestNotifyList_TimestampsParseAsRFC3339 runs the actual notification query
// against a real database. A unit test over the format string alone would not
// catch this: the defect only appears once Postgres expands the template under
// the pinned session zone.
func TestNotifyList_TimestampsParseAsRFC3339(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()

	// mail.New on an empty config has no SMTP host, so delivery fails and Send
	// logs it — deliberately, since the in-app row is the source of truth and
	// is written before the mail attempt. That is exactly the path we want.
	notify := &NotifyService{pool: pool, mailer: mail.New(config.Config{})}

	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, is_active, profile_completed, study_level)
		 VALUES ($1, $2, 'Notify', 'Test', TRUE, TRUE, 'undergrad')`,
		userID, fmt.Sprintf("notify-%s@example.test", userID)); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	notify.Send(ctx, userID, "หัวข้อทดสอบ", "เนื้อหาทดสอบ", "/notifications")

	list, err := notify.List(ctx, userID, 10, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want exactly 1 in-app notification, got %d", len(list))
	}

	// The precise failure the user saw: this is what the browser does.
	assertRFC3339(t, "created_at", list[0].CreatedAt)

	// read_at travels the same template and is nullable, so prove both that it
	// stays NULL while unread and parses once set.
	if list[0].ReadAt != nil {
		t.Fatalf("a freshly created notification must be unread, got read_at=%q", *list[0].ReadAt)
	}
	if err := notify.MarkRead(ctx, userID, list[0].ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	read, err := notify.List(ctx, userID, 10, false)
	if err != nil {
		t.Fatalf("List after MarkRead: %v", err)
	}
	if read[0].ReadAt == nil {
		t.Fatal("read_at must be set after MarkRead")
	}
	assertRFC3339(t, "read_at", *read[0].ReadAt)
}

func assertRFC3339(t *testing.T, field, got string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("%s = %q does not parse as RFC 3339 (this is what makes JavaScript "+
			"report Invalid Date): %v", field, got, err)
	}
	// Asia/Bangkok is a fixed UTC+7 with no DST, so the offset is knowable.
	// Asserting it catches a template that parses but drops the zone entirely.
	if _, offset := ts.Zone(); offset != 7*60*60 {
		t.Errorf("%s = %q: want a +07:00 offset, got %+d seconds", field, got, offset)
	}
	if !strings.HasSuffix(got, "+07:00") {
		t.Errorf("%s = %q: want the offset spelled with minutes (+07:00)", field, got)
	}
}

// truncatedOffset matches a TO_CHAR datetime template whose offset element
// omits the minutes. `OF` and `TZ` both collapse to "+07" under a whole-hour
// zone, and `TZH` without a following `TZM` does the same.
//
// The needle is assembled at runtime so this file does not match itself.
var truncatedOffset = regexp.MustCompile(`HH24:MI:SS(` +
	strings.Join([]string{"OF", "TZ", "TZH"}, "|") + `)'`)

// TestNoTruncatedTimezoneOffsetInSQL scans the whole tree, not just notify.go.
// The original report was filed against /notifications, but the same truncated
// template was live on 20 other columns across 6 more files — document
// progress, export batches, submission periods, appointment rounds, teaching
// course exports and work-log edit batches. Fixing only the reported endpoint
// would have left the identical bug waiting on every other timestamp the API
// serves, so the guard is repo-wide by design.
func TestNoTruncatedTimezoneOffsetInSQL(t *testing.T) {
	root, err := filepath.Abs(repoRoot(t))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	self := offsetGuardSelf(t)

	var offenders []string
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and generated trees are not ours to police.
			if name := d.Name(); name == ".git" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || path == self {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if truncatedOffset.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	if len(offenders) > 0 {
		t.Errorf("TO_CHAR templates emit a UTC offset without minutes (\"+07\"), which "+
			"JavaScript's Date parser rejects — use HH24:MI:SSTZH:TZM instead:\n\t%s",
			strings.Join(offenders, "\n\t"))
	}
}

// offsetGuardSelf locates this file so the scan can skip it — the regex above
// would otherwise match the literals it is built from.
func offsetGuardSelf(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve this test's source path")
	}
	return file
}
