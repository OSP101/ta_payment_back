package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// seedAudit writes rows straight through the auditor, so the test exercises the
// same columns the product writes rather than a hand-built INSERT that could
// drift from it.
func seedAudit(t *testing.T, f *fixture, entries ...audit.Entry) {
	t.Helper()
	a := audit.New(f.Pool)
	for _, e := range entries {
		if err := a.Log(f.ctx, e); err != nil {
			t.Fatalf("seed audit %s: %v", e.Action, err)
		}
	}
}

func auditSvc(f *fixture) *AuditService { return &AuditService{pool: f.Pool} }

// The question the old handler could not answer at all: "show me everything
// this one request did". It read the newest 200 rows and filtered in the
// browser, so a row outside that window was unreachable however it was phrased.
func TestListAudit_FindsEverythingOneRequestDid(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)

	target := uuid.New()
	other := uuid.New()
	ctxA := audit.WithRequest(f.ctx, audit.RequestInfo{RequestID: target, IP: "203.0.113.7", ActorRole: "staff"})
	ctxB := audit.WithRequest(f.ctx, audit.RequestInfo{RequestID: other, IP: "198.51.100.2", ActorRole: "ta"})

	a := audit.New(f.Pool)
	for _, act := range []string{"probe.one", "probe.two"} {
		if err := a.Log(ctxA, audit.Entry{Action: act, Entity: "thing", EntityID: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Log(ctxB, audit.Entry{Action: "probe.other", Entity: "thing", EntityID: "y"}); err != nil {
		t.Fatal(err)
	}

	rows, total, err := svc.ListAudit(f.ctx, AuditQuery{RequestID: &target})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("got %d rows (total %d), want 2 — the request's own actions", len(rows), total)
	}
	for _, r := range rows {
		if r.Action == "probe.other" {
			t.Error("a different request's row came back under this request id")
		}
	}
}

// A window is applied whether or not one was asked for: an unbounded query is a
// full scan of a table that only grows, and the page count behind it needs a
// bounded set to mean anything.
func TestListAudit_DefaultsToARecentWindow(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f, audit.Entry{Action: "probe.recent", Entity: "thing", EntityID: "x"})
	// Backdate a second row past the default window. INSERT directly: `at`
	// defaults to NOW() and the table is append-only, so it cannot be moved
	// afterwards.
	f.exec(`INSERT INTO audit_logs (at, action, entity, entity_id)
	        VALUES (NOW() - INTERVAL '30 days', 'probe.ancient', 'thing', 'x')`)

	rows, _, err := svc.ListAudit(f.ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Action == "probe.ancient" {
			t.Error("a 30-day-old row came back from a default query; the window is not applied")
		}
	}

	// …and widening the dates reaches it, so the window is a default and not a
	// ceiling.
	rows, _, err = svc.ListAudit(f.ctx, AuditQuery{From: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.Action == "probe.ancient" {
			found = true
		}
	}
	if !found {
		t.Error("an explicit 90-day range still could not reach the old row")
	}
}

// Action matches by prefix, so "auth." reads as the whole sign-in family — the
// way these names were already grouped by their dots.
func TestListAudit_ActionMatchesTheFamilyByPrefix(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "auth.login", Entity: "user", EntityID: "a"},
		audit.Entry{Action: "auth.login_failed", Entity: "user", EntityID: "a"},
		audit.Entry{Action: "worklog.approve", Entity: "work_log", EntityID: "b"},
	)
	_, total, err := svc.ListAudit(f.ctx, AuditQuery{Action: "auth."})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("action=auth. matched %d rows, want 2 (the family, not one exact name)", total)
	}
}

// Typed text is text, not a pattern.
//
// The probe is a BARE "%" on purpose. A term like "100%" cannot tell the two
// behaviours apart — unescaped it becomes LIKE '%100%%', which still requires
// "100" to be there — whereas a lone "%" becomes LIKE '%%%', which matches
// every row in the table. That is the real failure: the search box silently
// stops filtering and the investigator reads the whole trail as if it were
// their result set.
func TestListAudit_SearchDoesNotTreatUserInputAsWildcards(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "probe.pct", Entity: "thing", EntityID: "x", Note: "raised to 100% of cap"},
		audit.Entry{Action: "probe.plain", Entity: "thing", EntityID: "y", Note: "nothing special"},
	)
	_, total, err := svc.ListAudit(f.ctx, AuditQuery{Q: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("a search for %q matched %d rows, want the 1 row containing that "+
			"character — a bare wildcard means the box stopped filtering", "%", total)
	}

	// The underscore is the other wildcard, and the one nobody remembers.
	seedAudit(t, f, audit.Entry{Action: "probe.us", Entity: "thing", EntityID: "z", Note: "a_b"})
	if _, total, err = svc.ListAudit(f.ctx, AuditQuery{Q: "a_b"}); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("a search for %q matched %d rows, want 1", "a_b", total)
	}
}

// An IP typed as "203.0.113.7" has to match the stored inet, which compares
// equal only to the same value carrying its /32.
func TestListAudit_MatchesAnAddressAsTyped(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f, audit.Entry{Action: "probe.ip", Entity: "thing", EntityID: "x", IP: "203.0.113.7"})

	_, total, err := svc.ListAudit(f.ctx, AuditQuery{IP: "203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("ip=203.0.113.7 matched %d rows, want 1", total)
	}
}

// The screen pages through the result; the total counts every match, not the
// page. Getting this wrong shows "1 of 1 page" over a set of hundreds.
func TestListAudit_TotalCountsEveryMatchNotThePage(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	for i := 0; i < 5; i++ {
		seedAudit(t, f, audit.Entry{Action: "probe.page", Entity: "thing", EntityID: "x"})
	}
	rows, total, err := svc.ListAudit(f.ctx, AuditQuery{Action: "probe.page", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("page held %d rows, want the requested 2", len(rows))
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 — the count is of matches, not of the page", total)
	}

	second, _, err := svc.ListAudit(f.ctx, AuditQuery{Action: "probe.page", Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID == rows[0].ID {
		t.Error("offset did not move the window; every page would show the same rows")
	}
}

// The filter offers what the table actually holds, so a newly audited action
// appears without anyone editing a list in the frontend.
func TestListAuditActions_ReturnsWhatTheTableHolds(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "zzz.last", Entity: "thing", EntityID: "x"},
		audit.Entry{Action: "aaa.first", Entity: "thing", EntityID: "x"},
		audit.Entry{Action: "aaa.first", Entity: "thing", EntityID: "y"},
	)
	got, err := svc.ListAuditActions(f.ctx)
	if err != nil {
		t.Fatalf("ListAuditActions: %v", err)
	}
	seen := map[string]int{}
	for _, a := range got {
		seen[a]++
	}
	if seen["aaa.first"] != 1 {
		t.Errorf("aaa.first appeared %d times, want exactly 1 — the list is distinct", seen["aaa.first"])
	}
	if seen["zzz.last"] != 1 {
		t.Error("zzz.last missing from the action list")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("actions are not sorted: %q before %q", got[i-1], got[i])
		}
	}
}

// The overview strip answers "is anything wrong?" before a single row is read,
// and the number that separates a bad Monday from an attack is the count of
// ADDRESSES behind the failures — eight tries from one machine is a mistyped
// password, eight from eight machines is not.
func TestSummarizeAudit_CountsAddressesBehindEachAction(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		seedAudit(t, f, audit.Entry{Action: "auth.login_failed", Entity: "user", EntityID: "a", IP: ip})
	}
	// Two more from an address already counted: more attempts, same machine.
	for i := 0; i < 2; i++ {
		seedAudit(t, f, audit.Entry{Action: "auth.login_failed", Entity: "user", EntityID: "a", IP: "203.0.113.1"})
	}
	seedAudit(t, f, audit.Entry{Action: "worklog.approve", Entity: "assignment", EntityID: "b", IP: "203.0.113.1"})

	sum, err := svc.SummarizeAudit(f.ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SummarizeAudit: %v", err)
	}
	byAction := map[string]AuditActionCount{}
	for _, a := range sum.Actions {
		byAction[a.Action] = a
	}
	got := byAction["auth.login_failed"]
	if got.Count != 5 {
		t.Errorf("login_failed count = %d, want 5", got.Count)
	}
	if got.DistinctIPs != 3 {
		t.Errorf("login_failed distinct_ips = %d, want 3 — this is the number that "+
			"tells a mistyped password from a spray", got.DistinctIPs)
	}
	if byAction["worklog.approve"].Count != 1 {
		t.Error("an unrelated action is missing from the summary")
	}
}

// The person filter is built from who was ACTIVE in the window, not from the
// account roster — reading the roster is itself an audited disclosure, so
// sourcing it that way would file a "read everyone's details" row every time
// somebody opened the audit screen.
func TestSummarizeAudit_ListsWhoWasActiveNotEveryAccount(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	busy := f.insertUser("staff", "busybee")
	f.exec(`UPDATE users SET first_name='ขยัน', last_name='ทำงาน' WHERE id=$1`, busy)
	// A second account that exists but did nothing in the window.
	f.insertUser("staff", "idle")

	for i := 0; i < 3; i++ {
		seedAudit(t, f, audit.Entry{ActorID: &busy, ActorRole: "staff",
			Action: "probe.busy", Entity: "thing", EntityID: "x"})
	}

	sum, err := svc.SummarizeAudit(f.ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range sum.Actors {
		if a.ID == busy {
			found = true
			if a.Name != "ขยัน ทำงาน" {
				t.Errorf("actor name = %q, want the readable name", a.Name)
			}
			if a.Count != 3 {
				t.Errorf("actor count = %d, want 3", a.Count)
			}
			if a.Role != "staff" {
				t.Errorf("actor role = %q, want staff", a.Role)
			}
		}
	}
	if !found {
		t.Fatal("the active account is missing from the filter list")
	}
	for _, a := range sum.Actors {
		if a.Count == 0 {
			t.Errorf("%s did nothing in the window but is offered as a filter", a.Name)
		}
	}
}

// The screen showed a truncated uuid for the person being acted on while
// showing the actor's real name two columns away, which made "who looked at
// whose record" unreadable — the exact question these rows exist to answer.
func TestListAudit_ResolvesTheSubjectToAName(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	subject := f.insertUser("ta", "subjectperson")
	f.exec(`UPDATE users SET first_name='ถูก', last_name='ตรวจสอบ' WHERE id=$1`, subject)
	seedAudit(t, f, audit.Entry{Action: "user.record.view", Entity: "user", EntityID: subject.String()})

	rows, _, err := svc.ListAudit(f.ctx, AuditQuery{Action: "user.record.view"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].SubjectName != "ถูก ตรวจสอบ" {
		t.Errorf("subject_name = %q, want the person's name rather than a uuid", rows[0].SubjectName)
	}
}

// entity_id is free text — it also holds "periodID/taID" pairs and storage
// keys. A uuid cast in the join would raise on the first such row and take the
// whole query with it, so the resolution has to survive them.
func TestListAudit_SurvivesASubjectThatIsNotAUUID(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "probe.pair", Entity: "submission_period_status",
			EntityID: "6f1a0e2c-0000-0000-0000-000000000000/not-a-uuid"},
		audit.Entry{Action: "probe.pair", Entity: "thing", EntityID: "audit_archives/some-key.jsonl"},
	)
	rows, _, err := svc.ListAudit(f.ctx, AuditQuery{Action: "probe.pair"})
	if err != nil {
		t.Fatalf("a non-uuid entity_id broke the whole query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.SubjectName != "" {
			t.Errorf("subject_name = %q for an id that names nobody, want empty", r.SubjectName)
		}
	}
}

// Each card in the screen's overview stands for a GROUP of actions —
// "เข้าระบบไม่สำเร็จ" is a wrong password, an unknown account and a failed
// second factor — so clicking one has to reach all of them.
//
// Matched as a single prefix, the comma-joined list returns nothing, and the
// card reads as "no such events" rather than as a broken filter. That is the
// worst possible failure for this screen: it answers "is anything wrong?" with
// a confident no.
func TestListAudit_ActionAcceptsAGroupOfActions(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := auditSvc(f)
	seedAudit(t, f,
		audit.Entry{Action: "auth.login_failed", Entity: "user", EntityID: "a"},
		audit.Entry{Action: "auth.login_unknown_account", Entity: "user", EntityID: "b"},
		audit.Entry{Action: "auth.2fa_failed", Entity: "user", EntityID: "c"},
		audit.Entry{Action: "auth.login", Entity: "user", EntityID: "d"},
	)
	_, total, err := svc.ListAudit(f.ctx, AuditQuery{
		Action: "auth.login_failed,auth.login_unknown_account,auth.2fa_failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("the group matched %d rows, want 3 — a card that reaches none of "+
			"its own events reads as 'nothing is wrong'", total)
	}

	// A single value still means the family, so "auth." keeps working.
	if _, total, err = svc.ListAudit(f.ctx, AuditQuery{Action: "auth."}); err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("prefix match returned %d, want all 4 auth rows", total)
	}
}
