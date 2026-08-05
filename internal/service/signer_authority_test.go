package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/testutil"
)

// A คำสั่ง carries the dean's authority. When a deputy signs it, the block has
// to say so — otherwise the paper claims a รองคณบดี issued an order they have
// no power to issue. These tests pin who counts as the dean and what the
// signature block says when they are not the one signing.

func TestIsDeanTitle(t *testing.T) {
	cases := []struct {
		title string
		want  bool
		why   string
	}{
		{"คณบดีวิทยาลัยการคอมพิวเตอร์", true, "the seat itself"},
		{"คณบดี", true, "bare title, as the roster sometimes holds it"},
		{"  คณบดีวิทยาลัยการคอมพิวเตอร์  ", true, "padding is not a different person"},
		// The three below all CONTAIN "คณบดี". A substring test would hand every
		// deputy the dean's authority, which is the whole bug this guards.
		{"รองคณบดีฝ่ายวิชาการ", false, "a deputy dean is not the dean"},
		{"รองคณบดีฝ่ายบริหาร", false, "a deputy dean is not the dean"},
		{"ผู้ช่วยคณบดีฝ่ายดิจิทัล", false, "an assistant dean is not the dean"},
		{"หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", false, "unrelated post"},
		{"", false, "no title is not the dean"},
	}
	for _, c := range cases {
		if got := IsDeanTitle(c.title); got != c.want {
			t.Errorf("IsDeanTitle(%q) = %v, want %v — %s", c.title, got, c.want, c.why)
		}
	}
}

// signerFixture puts a roster in a throwaway database and returns the pool.
func signerFixture(t *testing.T, officers ...[2]string) (*pgxpool.Pool, []uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	ids := make([]uuid.UUID, 0, len(officers))
	for _, o := range officers {
		id := uuid.New()
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO admin_officers (id, academic_prefix, full_name, title, is_active)
			 VALUES ($1, $2, $3, $4, TRUE)`, id, "ผศ. ดร.", o[0], o[1]); err != nil {
			t.Fatalf("insert officer: %v", err)
		}
		ids = append(ids, id)
	}
	return pool, ids
}

func TestSignerAuthority_DeanSignsAsThemselves(t *testing.T) {
	pool, ids := signerFixture(t, [2]string{"สิรภัทร เชี่ยวชาญวัฒนา", "คณบดีวิทยาลัยการคอมพิวเตอร์"})

	a, err := loadSignerAuthority(context.Background(), pool, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "คณบดีวิทยาลัยการคอมพิวเตอร์" {
		t.Errorf("title = %q — the dean's own line must be untouched", a.Title)
	}
	if a.ActingFor != "" {
		t.Errorf("ActingFor = %q, want empty — the dean is not acting for anyone", a.ActingFor)
	}
}

func TestSignerAuthority_DeputyGetsTheActingLines(t *testing.T) {
	pool, ids := signerFixture(t,
		[2]string{"สิรภัทร เชี่ยวชาญวัฒนา", "คณบดีวิทยาลัยการคอมพิวเตอร์"},
		[2]string{"ณกร วัฒนกิจ", "รองคณบดีฝ่ายวิชาการ"},
	)

	a, err := loadSignerAuthority(context.Background(), pool, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	// Own position first, acting phrase attached — NOT the dean's title in place
	// of theirs, which would misrepresent who signed.
	if a.Title != "รองคณบดีฝ่ายวิชาการ รักษาการแทน" {
		t.Errorf("position line = %q, want %q", a.Title, "รองคณบดีฝ่ายวิชาการ รักษาการแทน")
	}
	// Then the seat whose authority is being exercised, worded as the roster
	// words it rather than as a hardcoded guess.
	if a.ActingFor != "คณบดีวิทยาลัยการคอมพิวเตอร์" {
		t.Errorf("acting-for line = %q, want the dean's seat as the roster spells it", a.ActingFor)
	}
	if a.Name != "ผศ. ดร.ณกร วัฒนกิจ" {
		t.Errorf("name = %q — the deputy signs under their OWN name", a.Name)
	}
}

// The vacancy is exactly when someone acts, so the roster may hold no dean at
// all. The order still has to name the seat.
func TestSignerAuthority_NamesTheSeatWhenNoDeanIsOnTheRoster(t *testing.T) {
	pool, ids := signerFixture(t, [2]string{"ศรัณย์ อภิชนตระกูล", "รองคณบดีฝ่ายบริหาร"})

	a, err := loadSignerAuthority(context.Background(), pool, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.ActingFor != fallbackDeanTitle {
		t.Errorf("acting-for line = %q, want the fallback seat %q — a vacant deanship "+
			"must not print a blank authority line", a.ActingFor, fallbackDeanTitle)
	}
}

// An inactive dean row still words the seat better than the constant does.
func TestSignerAuthority_PrefersTheRostersWordingOverTheFallback(t *testing.T) {
	pool, ids := signerFixture(t, [2]string{"ศรัณย์ อภิชนตระกูล", "รองคณบดีฝ่ายบริหาร"})
	deanID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO admin_officers (id, academic_prefix, full_name, title, is_active)
		 VALUES ($1, '', 'คณบดีคนก่อน ทดสอบ', 'คณบดีวิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น', FALSE)`,
		deanID); err != nil {
		t.Fatal(err)
	}

	a, err := loadSignerAuthority(context.Background(), pool, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.ActingFor != "คณบดีวิทยาลัยการคอมพิวเตอร์ มหาวิทยาลัยขอนแก่น" {
		t.Errorf("acting-for line = %q — the seat should read as the roster spells it", a.ActingFor)
	}
}

// An active dean wins over an inactive one, so a superseded row cannot word the
// authority line of a current order.
func TestSignerAuthority_ActiveDeanWinsOverAnInactiveOne(t *testing.T) {
	pool, ids := signerFixture(t, [2]string{"ศรัณย์ อภิชนตระกูล", "รองคณบดีฝ่ายบริหาร"})
	for _, row := range [][3]any{
		{"คณบดีคนก่อน", "คณบดีวิทยาลัยการคอมพิวเตอร์ (เก่า)", false},
		{"คณบดีคนปัจจุบัน", "คณบดีวิทยาลัยการคอมพิวเตอร์", true},
	} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO admin_officers (id, academic_prefix, full_name, title, is_active)
			 VALUES ($1, '', $2, $3, $4)`, uuid.New(), row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}

	a, err := loadSignerAuthority(context.Background(), pool, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.ActingFor != "คณบดีวิทยาลัยการคอมพิวเตอร์" {
		t.Errorf("acting-for line = %q, want the ACTIVE dean's wording", a.ActingFor)
	}
}

func TestSignerAuthority_RefusesAnUnknownOfficer(t *testing.T) {
	pool, _ := signerFixture(t)
	if _, err := loadSignerAuthority(context.Background(), pool, uuid.New()); err == nil {
		t.Fatal("an officer id that is not on the roster must be refused")
	}
}
