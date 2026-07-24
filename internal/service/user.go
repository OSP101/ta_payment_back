package service

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
)

type User struct {
	ID                 uuid.UUID `json:"id"`
	Email              string    `json:"email"`
	Title              *string   `json:"title,omitempty"`
	FirstName          string    `json:"first_name"`
	LastName           string    `json:"last_name"`
	Phone              *string   `json:"phone,omitempty"`
	StudyLevel         *string   `json:"study_level,omitempty"`
	StudyYear          *int      `json:"study_year,omitempty"`
	StudentID          *string   `json:"student_id,omitempty"`
	Department         *string   `json:"department,omitempty"`
	IsActive           bool      `json:"is_active"`
	ProfileComplete    bool      `json:"profile_completed"`
	MustChangePassword bool      `json:"must_change_password"`
	Roles              []string  `json:"roles"`
	BankName           *string   `json:"bank_name,omitempty"`
	BankBranch         *string   `json:"bank_branch,omitempty"`
	BranchCode         *string   `json:"branch_code,omitempty"`
	AccountNo          *string   `json:"account_no,omitempty"`
}

type UserService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

type CreateUserInput struct {
	Email      string   `json:"email"`
	Title      *string  `json:"title,omitempty"`
	FirstName  string   `json:"first_name"`
	LastName   string   `json:"last_name"`
	Phone      *string  `json:"phone,omitempty"`
	Roles      []string `json:"roles"`
	StudyLevel *string  `json:"study_level,omitempty"`
	StudyYear  *int     `json:"study_year,omitempty"`
	Password   *string  `json:"password,omitempty"`
}

type CreateUserResult struct {
	User         *User  `json:"user"`
	TempPassword string `json:"temp_password"`
}

func (s *UserService) Create(ctx context.Context, actor uuid.UUID, in CreateUserInput) (*CreateUserResult, error) {
	in.Email = strings.TrimSpace(in.Email)
	in.FirstName = strings.TrimSpace(in.FirstName)
	in.LastName = strings.TrimSpace(in.LastName)
	if in.Email == "" || in.FirstName == "" || in.LastName == "" {
		return nil, Invalid("กรุณากรอกอีเมล ชื่อ และนามสกุลให้ครบ")
	}
	if !validEmail(in.Email) {
		return nil, Invalid("รูปแบบอีเมลไม่ถูกต้อง")
	}
	if len(in.Roles) == 0 {
		return nil, Invalid("ต้องระบุบทบาทอย่างน้อย 1 บทบาท")
	}
	if err := validateRoles(in.Roles); err != nil {
		return nil, err
	}
	// Only an admin may create another admin.
	if containsRole(in.Roles, "admin") {
		actorAdmin, err := hasRole(ctx, s.pool, actor, "admin")
		if err != nil {
			return nil, err
		}
		if !actorAdmin {
			return nil, Forbidden("เฉพาะผู้ดูแลระบบ (admin) เท่านั้นที่สร้างบัญชี admin ได้")
		}
	}
	// Enforce the same minimum password policy staff-supplied passwords must meet.
	if in.Password != nil && *in.Password != "" && len(*in.Password) < 8 {
		return nil, Invalid("รหัสผ่านต้องมีอย่างน้อย 8 ตัวอักษร")
	}
	if err := validateStudyYear(in.StudyYear); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	tempPw := ""
	var pwHash *string
	if in.Password != nil && *in.Password != "" {
		h, err := auth.HashPassword(*in.Password)
		if err != nil {
			return nil, err
		}
		pwHash = &h
	} else {
		tempPw = generateTempPassword(12)
		h, err := auth.HashPassword(tempPw)
		if err != nil {
			return nil, err
		}
		pwHash = &h
	}
	id := uuid.New()
	_, err = tx.Exec(ctx,
		`INSERT INTO users (id, email, title, first_name, last_name, phone, study_level, study_year, password_hash, must_change_password)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,TRUE)`,
		id, strings.ToLower(in.Email), in.Title, in.FirstName, in.LastName, in.Phone, in.StudyLevel, in.StudyYear, pwHash)
	if err != nil {
		return nil, err
	}
	for _, r := range in.Roles {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, r); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "user.create", Entity: "user", EntityID: id.String(), After: in})
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &CreateUserResult{User: u, TempPassword: tempPw}, nil
}

func (s *UserService) Get(ctx context.Context, id uuid.UUID) (*User, error) {
	u := &User{ID: id}
	err := s.pool.QueryRow(ctx,
		`SELECT email, title, first_name, last_name, phone, study_level::text, study_year, student_id, department, is_active, profile_completed, must_change_password
		 FROM users WHERE id = $1 AND deleted_at IS NULL`, id).Scan(
		&u.Email, &u.Title, &u.FirstName, &u.LastName, &u.Phone, &u.StudyLevel, &u.StudyYear, &u.StudentID, &u.Department, &u.IsActive, &u.ProfileComplete, &u.MustChangePassword,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT role::text FROM user_roles WHERE user_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err == nil {
			u.Roles = append(u.Roles, r)
		}
	}
	// Best-effort banking info from ta_profiles (optional).
	var bn, bb, bc, an *string
	err = s.pool.QueryRow(ctx,
		`SELECT bank_name, bank_branch, branch_code, account_no FROM ta_profiles WHERE user_id=$1`, id,
	).Scan(&bn, &bb, &bc, &an)
	if err == nil {
		u.BankName = bn
		u.BankBranch = bb
		u.BranchCode = bc
		u.AccountNo = an
	}
	applyDerivedStudyYear(u, currentAcademicYearBE(ctx, s.pool))
	return u, nil
}

func (s *UserService) FindByEmail(ctx context.Context, email string) (*User, string, error) {
	var id uuid.UUID
	var pwHash *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash FROM users WHERE email = $1 AND is_active AND deleted_at IS NULL`,
		strings.ToLower(email)).Scan(&id, &pwHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if pwHash == nil {
		return u, "", nil
	}
	return u, *pwHash, nil
}

func (s *UserService) List(ctx context.Context, role, search string, limit, offset int) ([]User, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := "u.deleted_at IS NULL"
	args := []any{}
	i := 1
	if role != "" {
		where += " AND EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id=u.id AND ur.role=$" + itoa(i) + "::role_code)"
		args = append(args, role)
		i++
	}
	if search != "" {
		where += " AND (u.email ILIKE $" + itoa(i) + " OR u.first_name ILIKE $" + itoa(i) + " OR u.last_name ILIKE $" + itoa(i) + " OR COALESCE(u.student_id,'') ILIKE $" + itoa(i) + ")"
		args = append(args, "%"+search+"%")
		i++
	}
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM users u WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT u.id, u.email, u.title, u.first_name, u.last_name, u.phone, u.study_level::text, u.study_year, u.student_id, u.department, u.is_active, u.profile_completed, u.must_change_password
	      FROM users u WHERE ` + where + ` ORDER BY u.first_name, u.last_name
		  LIMIT $` + itoa(i) + ` OFFSET $` + itoa(i+1)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Title, &u.FirstName, &u.LastName, &u.Phone, &u.StudyLevel, &u.StudyYear, &u.StudentID, &u.Department, &u.IsActive, &u.ProfileComplete, &u.MustChangePassword); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	// bulk-load roles
	for k := range out {
		if r, err := s.pool.Query(ctx, `SELECT role::text FROM user_roles WHERE user_id=$1`, out[k].ID); err == nil {
			for r.Next() {
				var s string
				if err := r.Scan(&s); err == nil {
					out[k].Roles = append(out[k].Roles, s)
				}
			}
			r.Close()
		}
	}
	// Auto-derive study_year from the student id so it stays current without
	// manual edits every academic year.
	cur := currentAcademicYearBE(ctx, s.pool)
	for k := range out {
		applyDerivedStudyYear(&out[k], cur)
	}
	return out, total, nil
}

// currentAcademicYearBE returns the active term's academic year (Buddhist era),
// or 0 if no term is active.
func currentAcademicYearBE(ctx context.Context, pool *pgxpool.Pool) int {
	var y int
	_ = pool.QueryRow(ctx,
		`SELECT academic_year FROM academic_terms WHERE is_active
		 ORDER BY academic_year DESC, semester DESC LIMIT 1`).Scan(&y)
	return y
}

// deriveStudyYear computes an undergrad's current year from the 2-digit Buddhist
// admission-year prefix of the KKU student id (e.g. "67…" → admitted 2567,
// so in academic year 2569 they are in year 3). Returns 0 when it cannot derive
// a sane 1..8 value, so callers keep the stored value as a fallback for
// irregular ids / transfer students.
func deriveStudyYear(studentID string, curYearBE int) int {
	digits := make([]byte, 0, 2)
	for i := 0; i < len(studentID) && len(digits) < 2; i++ {
		if c := studentID[i]; c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	if len(digits) < 2 || curYearBE <= 0 {
		return 0
	}
	admitBE := 2500 + int(digits[0]-'0')*10 + int(digits[1]-'0')
	yr := curYearBE - admitBE + 1
	if yr < 1 || yr > 8 {
		return 0
	}
	return yr
}

// applyDerivedStudyYear overrides study_year in place for undergrad users who
// have a student id, keeping the value current automatically.
func applyDerivedStudyYear(u *User, curYearBE int) {
	if u.StudyLevel == nil || *u.StudyLevel != "undergrad" || u.StudentID == nil {
		return
	}
	if yr := deriveStudyYear(*u.StudentID, curYearBE); yr > 0 {
		u.StudyYear = &yr
	}
}

// UpdateInput is a partial patch — only non-nil fields are applied.
type UpdateUserInput struct {
	Email      *string   `json:"email,omitempty"`
	Title      *string   `json:"title,omitempty"`
	FirstName  *string   `json:"first_name,omitempty"`
	LastName   *string   `json:"last_name,omitempty"`
	Phone      *string   `json:"phone,omitempty"`
	StudyLevel *string   `json:"study_level,omitempty"`
	StudyYear  *int      `json:"study_year,omitempty"`
	Roles      *[]string `json:"roles,omitempty"`
	BankName   *string   `json:"bank_name,omitempty"`
	BankBranch *string   `json:"bank_branch,omitempty"`
	BranchCode *string   `json:"branch_code,omitempty"`
	AccountNo  *string   `json:"account_no,omitempty"`
}

func (s *UserService) Update(ctx context.Context, actor, id uuid.UUID, in UpdateUserInput) (*User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	sets := []string{}
	args := []any{}
	i := 1
	add := func(col string, val any) {
		sets = append(sets, col+"=$"+itoa(i))
		args = append(args, val)
		i++
	}
	if in.Email != nil {
		add("email", strings.ToLower(*in.Email))
	}
	if in.Title != nil {
		add("title", *in.Title)
	}
	if in.FirstName != nil {
		add("first_name", *in.FirstName)
	}
	if in.LastName != nil {
		add("last_name", *in.LastName)
	}
	if in.Phone != nil {
		add("phone", *in.Phone)
	}
	if in.StudyLevel != nil {
		if *in.StudyLevel == "" {
			sets = append(sets, "study_level=NULL")
		} else {
			sets = append(sets, "study_level=$"+itoa(i)+"::study_level")
			args = append(args, *in.StudyLevel)
			i++
		}
	}
	if in.StudyYear != nil {
		// 0 is the "clear" sentinel; any other value is validated to 1..8.
		if *in.StudyYear == 0 {
			sets = append(sets, "study_year=NULL")
		} else {
			if err := validateStudyYear(in.StudyYear); err != nil {
				return nil, err
			}
			sets = append(sets, "study_year=$"+itoa(i))
			args = append(args, *in.StudyYear)
			i++
		}
	}
	if len(sets) > 0 {
		sets = append(sets, "updated_at=NOW()")
		q := "UPDATE users SET " + strings.Join(sets, ", ") + " WHERE id=$" + itoa(i)
		args = append(args, id)
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return nil, err
		}
	}

	if in.Roles != nil {
		if err := validateRoles(*in.Roles); err != nil {
			return nil, err
		}
		if len(*in.Roles) == 0 {
			return nil, Invalid("ต้องมีบทบาทอย่างน้อย 1 บทบาท")
		}
		actorAdmin, err := hasRole(ctx, s.pool, actor, "admin")
		if err != nil {
			return nil, err
		}
		// Non-admins may not grant admin, nor alter the roles of an existing
		// admin account (privilege-escalation guard, C4).
		if !actorAdmin {
			if containsRole(*in.Roles, "admin") {
				return nil, Forbidden("เฉพาะผู้ดูแลระบบ (admin) เท่านั้นที่กำหนดบทบาท admin ได้")
			}
			targetAdmin, err := hasRole(ctx, s.pool, id, "admin")
			if err != nil {
				return nil, err
			}
			if targetAdmin {
				return nil, Forbidden("เฉพาะผู้ดูแลระบบ (admin) เท่านั้นที่แก้ไขบทบาทของบัญชี admin ได้")
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id=$1`, id); err != nil {
			return nil, err
		}
		for _, r := range *in.Roles {
			if _, err := tx.Exec(ctx,
				`INSERT INTO user_roles (user_id, role) VALUES ($1, $2::role_code)`, id, r); err != nil {
				return nil, err
			}
		}
	}

	if in.BankName != nil || in.BankBranch != nil || in.BranchCode != nil || in.AccountNo != nil {
		bn := coalesceStr(in.BankName)
		bb := coalesceStr(in.BankBranch)
		bc := coalesceStr(in.BranchCode)
		an := coalesceStr(in.AccountNo)
		if _, err := tx.Exec(ctx, `
			INSERT INTO ta_profiles (user_id, bank_name, bank_branch, branch_code, account_no)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (user_id) DO UPDATE SET
			  bank_name=COALESCE(EXCLUDED.bank_name, ta_profiles.bank_name),
			  bank_branch=COALESCE(EXCLUDED.bank_branch, ta_profiles.bank_branch),
			  branch_code=COALESCE(EXCLUDED.branch_code, ta_profiles.branch_code),
			  account_no=COALESCE(EXCLUDED.account_no, ta_profiles.account_no)
			`, id, bn, bb, bc, an); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "user.update", Entity: "user", EntityID: id.String(), After: in})
	return s.Get(ctx, id)
}

func (s *UserService) Deactivate(ctx context.Context, actor, id uuid.UUID) error {
	if actor == id {
		return Invalid("ไม่สามารถปิดใช้งานบัญชีของตนเองได้")
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET is_active = FALSE, updated_at=NOW() WHERE id=$1`, id)
	if err == nil {
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "user.deactivate", Entity: "user", EntityID: id.String()})
	}
	return err
}

// Activate re-enables a previously deactivated account.
func (s *UserService) Activate(ctx context.Context, actor, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET is_active = TRUE, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "user.activate", Entity: "user", EntityID: id.String()})
	return nil
}

// GetEmail returns the current email for a user (used for deactivation confirmation).
func (s *UserService) GetEmail(ctx context.Context, id uuid.UUID) (string, error) {
	var e string
	err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, id).Scan(&e)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return e, err
}

func (s *UserService) UpdatePassword(ctx context.Context, id uuid.UUID, newPassword string) error {
	h, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE users SET password_hash=$1, must_change_password=FALSE, updated_at=NOW() WHERE id=$2`, h, id)
	return err
}

// ResetPassword issues a fresh temp password and forces a change on next login.
func (s *UserService) ResetPassword(ctx context.Context, actor, id uuid.UUID) (string, error) {
	tempPw := generateTempPassword(12)
	h, err := auth.HashPassword(tempPw)
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE users SET password_hash=$1, must_change_password=TRUE, updated_at=NOW() WHERE id=$2`, h, id)
	if err != nil {
		return "", err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "user.reset_password", Entity: "user", EntityID: id.String()})
	return tempPw, nil
}

func (s *UserService) MarkProfileComplete(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET profile_completed=TRUE, updated_at=NOW() WHERE id=$1`, id)
	return err
}

// validateStudyYear allows nil (not set) or 1..8; 0 is handled by callers as a
// clear sentinel and should not reach here as a stored value.
func validateStudyYear(y *int) error {
	if y == nil || *y == 0 {
		return nil
	}
	if *y < 1 || *y > 8 {
		return Invalid("ชั้นปีต้องอยู่ระหว่าง 1 ถึง 8")
	}
	return nil
}

var validRoles = map[string]bool{"admin": true, "staff": true, "lecturer": true, "ta": true}

func validateRoles(roles []string) error {
	for _, r := range roles {
		if !validRoles[r] {
			return Invalid("บทบาทไม่ถูกต้อง: " + r)
		}
	}
	return nil
}

func containsRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// validEmail does a lightweight structural check (local@domain.tld). It is not a
// full RFC 5322 validator — just enough to reject obvious garbage.
func validEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.Count(s, "@") != 1 {
		return false
	}
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return true
}

func coalesceStr(p *string) any {
	if p == nil {
		return nil
	}
	if *p == "" {
		return nil
	}
	return *p
}

// generateTempPassword returns a URL-safe, human-friendly random string.
// Uses an unambiguous alphabet (no 0/O, 1/l/I) so users can transcribe it.
func generateTempPassword(n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a fixed but obviously-temporary string; caller can reset again.
		return "Temp-Password-Change-Me"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func itoa(i int) string {
	// tiny helper to avoid strconv import churn
	return uintToString(uint64(i))
}
func uintToString(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
