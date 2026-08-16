package demo

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/service"
)

// Password every seeded demo account shares. Fine to publish in the UI —
// these accounts hold fake data behind a schema no real user or API token
// can reach (see docs/PLAN-demo-sandbox.md §7). Meets UserService's own
// ValidatePassword rule (8+ chars, a letter and a digit) so nothing downstream
// treats it as weak.
const DemoPassword = "Demo1234!"

// DemoAccount is one seeded login, exposed so the frontend's role picker
// (POST /api/v1/demo/enter's response) never hardcodes a copy of this list
// that could drift from what seedSlot actually creates.
type DemoAccount struct {
	Email string   `json:"email"`
	Label string   `json:"label"`
	Roles []string `json:"roles"`
}

// demoAccounts seeds one login per role the scenario engine (scenario.go)
// drives: 3 lecturers (each teaches one of the 3 seeded courses) and 4 TAs
// (3 get appointed by the scenario engine; the 4th is left unassigned — a
// TA who exists but hasn't been called on yet is itself a realistic state
// worth being able to look at). Numbered rather than named: these are seats
// in the scenario, not characters, and a number is unambiguous about which
// course/TA a given login lines up with in scenario.go.
var demoAccounts = []DemoAccount{
	{Email: "admin@demo.local", Label: "ผู้ดูแลระบบ (admin)", Roles: []string{"admin", "staff"}},
	{Email: "staff@demo.local", Label: "เจ้าหน้าที่ (staff)", Roles: []string{"staff"}},
	{Email: "lecturer1@demo.local", Label: "อาจารย์ผู้สอน 1", Roles: []string{"lecturer"}},
	{Email: "lecturer2@demo.local", Label: "อาจารย์ผู้สอน 2", Roles: []string{"lecturer"}},
	{Email: "lecturer3@demo.local", Label: "อาจารย์ผู้สอน 3", Roles: []string{"lecturer"}},
	{Email: "ta1@demo.local", Label: "ผู้ช่วยสอน (TA) 1", Roles: []string{"ta"}},
	{Email: "ta2@demo.local", Label: "ผู้ช่วยสอน (TA) 2", Roles: []string{"ta"}},
	{Email: "ta3@demo.local", Label: "ผู้ช่วยสอน (TA) 3", Roles: []string{"ta"}},
	{Email: "ta4@demo.local", Label: "ผู้ช่วยสอน (TA) 4 — ยังไม่ถูกเรียกใช้งาน", Roles: []string{"ta"}},
}

// seedSlot bootstraps a freshly-truncated slot schema with just enough data
// to log in as each seeded role and land on a working (if empty) home page.
// Written as raw SQL against svc.Pool rather than through UserService.Create,
// mirroring cmd/seed/main.go's own bootstrap — that command hits the exact
// same chicken-and-egg problem (the first admin can't be created by an actor
// who needs to already be an admin) and solves it the same way.
func seedSlot(ctx context.Context, svc *service.Container) error {
	hash, err := auth.HashPassword(DemoPassword)
	if err != nil {
		return err
	}
	for _, acc := range demoAccounts {
		id := uuid.New()
		firstName, lastName := splitLabel(acc.Label)
		// must_change_password=FALSE and profile_completed=TRUE: a demo
		// account should drop the visitor straight into the app, not into
		// the forced-password-change or profile-completion flows real new
		// accounts go through.
		//
		// study_level is set ONLY for TA accounts, and is NOT cosmetic:
		// BudgetService.Compute's undergrad-regular branch filters
		// work_logs by `u.study_level = 'undergrad'` (budget.go) — a NULL
		// study_level (what every account got before this) silently zeroes
		// out that TA's entire contribution to UsedBaht, which would make
		// every course in the sandbox look like it never spent a baht no
		// matter how many hours got approved.
		var studyLevel *string
		for _, r := range acc.Roles {
			if r == "ta" {
				v := "undergrad"
				studyLevel = &v
				break
			}
		}
		if _, err := svc.Pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name, password_hash, is_active, profile_completed, must_change_password, study_level)
			 VALUES ($1,$2,$3,$4,$5,TRUE,TRUE,FALSE,$6::study_level)`,
			id, acc.Email, firstName, lastName, hash, studyLevel); err != nil {
			return fmt.Errorf("seed user %s: %w", acc.Email, err)
		}
		for _, r := range acc.Roles {
			if _, err := svc.Pool.Exec(ctx,
				`INSERT INTO user_roles (user_id, role) VALUES ($1,$2::role_code)`, id, r); err != nil {
				return fmt.Errorf("seed role %s/%s: %w", acc.Email, r, err)
			}
		}
		// Deliberately NO ta_profiles row here — a fresh TA account has none,
		// and handler.RequireApprovedTAProfile already treats "no row" the
		// same as "pending" (see its own doc comment), leaving profile/
		// documents/account endpoints open so onboarding itself isn't
		// blocked. Scenario step 7 (ta_docs) is what fills this in via the
		// real DocsService.UpsertProfile/Upload, and step 8 (docs_approve)
		// is what a staff member would actually click — pre-seeding
		// 'approved' here would make both of those steps permanent no-ops
		// and rob the walkthrough of the one queue staff most need to see.
	}

	// Reference data every money calculation reads before it will run at
	// all — same defaults and the same "only if the table is empty" guard
	// cmd/seed/main.go uses, so a demo slot behaves like a freshly-seeded
	// real deployment instead of erroring out the moment staff touch
	// anything budget-related.
	if _, err := svc.Pool.Exec(ctx, `INSERT INTO pay_rates (id, effective_from,
	                    undergrad_regular, undergrad_special,
	                    graduate_regular, graduate_special_lumpsum,
	                    ug_lecture_hours_per_credit, ug_lab_hours_per_credit,
	                    baseline_students_lecture, baseline_students_lab,
	                    ug_workload_rate_regular, term_months,
	                    graduate_regular_hourly, grad_special_term_cap, daily_pay_cap_baht,
	                    ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap,
	                    note)
	                SELECT gen_random_uuid(), CURRENT_DATE,
	                       40, 50,
	                       3000, 4000,
	                       3, 4.5, 60, 30, 300, 4,
	                       50, 12000, 300,
	                       7, 6, 6,
	                       'seed defaults (demo sandbox)'
	                WHERE NOT EXISTS (SELECT 1 FROM pay_rates)`); err != nil {
		return fmt.Errorf("seed pay_rates: %w", err)
	}
	if _, err := svc.Pool.Exec(ctx,
		`INSERT INTO budget_caps (id, effective_from, per_course_max, note)
		 SELECT gen_random_uuid(), CURRENT_DATE, 20000, 'seed default (demo sandbox)'
		 WHERE NOT EXISTS (SELECT 1 FROM budget_caps)`); err != nil {
		return fmt.Errorf("seed budget_caps: %w", err)
	}

	// The scenario engine's appointment-order step (BuildAppointmentOrder)
	// needs a signer to resolve — see service.loadSignerAuthority, which
	// reads admin_officers by id. Title matches signer_authority.go's
	// deanTitlePrefix exactly so the printed order shows this person as the
	// actual dean rather than "ปฏิบัติหน้าที่แทน" (acting for) a vacant seat.
	if _, err := svc.Pool.Exec(ctx,
		`INSERT INTO admin_officers (id, academic_prefix, full_name, title, is_active)
		 SELECT gen_random_uuid(), 'ผู้ช่วยศาสตราจารย์ ดร.', 'สมมติ ตัวอย่างดี', 'คณบดีวิทยาลัยการคอมพิวเตอร์', TRUE
		 WHERE NOT EXISTS (SELECT 1 FROM admin_officers)`); err != nil {
		return fmt.Errorf("seed admin_officers: %w", err)
	}
	return nil
}

// splitLabel turns "ผู้ดูแลระบบ (admin)" into ("ผู้ดูแลระบบ", "(admin)") — good
// enough for a first/last name pair that only ever appears inside the demo
// sandbox UI, never on a real document.
func splitLabel(label string) (first, last string) {
	if i := strings.IndexByte(label, '('); i >= 0 {
		return strings.TrimSpace(label[:i]), strings.TrimSpace(label[i:])
	}
	return label, "Demo"
}
