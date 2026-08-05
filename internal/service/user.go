package service

import (
	"context"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"time"

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
	// AvatarURL is derived, never stored: the API path that streams the
	// picture, with the last-changed timestamp as a cache-buster. Nil when the
	// user has not set one, which is what tells the UI to draw initials.
	AvatarURL *string `json:"avatar_url,omitempty"`
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
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "user.create", Entity: "user", EntityID: id.String(), After: in}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &CreateUserResult{User: u, TempPassword: tempPw}, nil
}

func (s *UserService) Get(ctx context.Context, id uuid.UUID) (*User, error) {
	u := &User{ID: id}
	var avatarKey *string
	var avatarAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT email, title, first_name, last_name, phone, study_level::text, study_year, student_id, department, is_active, profile_completed, must_change_password, avatar_key, avatar_updated_at
		 FROM users WHERE id = $1 AND deleted_at IS NULL`, id).Scan(
		&u.Email, &u.Title, &u.FirstName, &u.LastName, &u.Phone, &u.StudyLevel, &u.StudyYear, &u.StudentID, &u.Department, &u.IsActive, &u.ProfileComplete, &u.MustChangePassword, &avatarKey, &avatarAt,
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
	// Banking details are deliberately not returned: they are not stored
	// (PDPA, migration 0047). Staff read them from the creditor-form PDF the
	// TA submitted, which is the only place they exist.
	applyDerivedStudyYear(u, currentAcademicYearBE(ctx, s.pool))
	u.AvatarURL = avatarURL(id, avatarKey, avatarAt)
	return u, nil
}

// avatarURL turns a stored key into the API path that serves it. The picture
// itself is never handed out by key — that would let anyone who saw one URL
// enumerate the store — so the path is keyed by user id and resolved server-side.
func avatarURL(id uuid.UUID, key *string, at *time.Time) *string {
	if key == nil || *key == "" {
		return nil
	}
	v := int64(0)
	if at != nil {
		v = at.Unix()
	}
	u := "/api/v1/users/" + id.String() + "/avatar?v=" + strconv.FormatInt(v, 10)
	return &u
}

// AvatarKey returns the stored key for a user's picture, or "" when unset.
func (s *UserService) AvatarKey(ctx context.Context, id uuid.UUID) (string, error) {
	var key *string
	err := s.pool.QueryRow(ctx,
		`SELECT avatar_key FROM users WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if key == nil {
		return "", nil
	}
	return *key, nil
}

// SetAvatar points the user at a freshly stored image and returns both the URL
// to show and the key that was replaced, if any. Deleting the old blob is the
// caller's job: it must happen only after the row commits, or a failed update
// would leave the user pointing at a file that no longer exists.
func (s *UserService) SetAvatar(ctx context.Context, id uuid.UUID, key string) (url string, replaced string, err error) {
	var old *string
	var at time.Time
	err = s.pool.QueryRow(ctx,
		`WITH prev AS (
		     SELECT id, avatar_key FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE
		 )
		 UPDATE users u SET avatar_key=$2, avatar_updated_at=NOW(), updated_at=NOW()
		 FROM prev WHERE u.id = prev.id
		 RETURNING prev.avatar_key, u.avatar_updated_at`,
		id, key).Scan(&old, &at)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &id, Action: "user.avatar.set", Entity: "user", EntityID: id.String()}); err != nil {
		return "", "", err
	}
	if old != nil && *old != "" && *old != key {
		replaced = *old
	}
	return *avatarURL(id, &key, &at), replaced, nil
}

// ClearAvatar removes the picture and returns the key to delete from the store.
func (s *UserService) ClearAvatar(ctx context.Context, id uuid.UUID) (string, error) {
	var old *string
	err := s.pool.QueryRow(ctx,
		`WITH prev AS (
		     SELECT id, avatar_key FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE
		 )
		 UPDATE users u SET avatar_key=NULL, avatar_updated_at=NOW(), updated_at=NOW()
		 FROM prev WHERE u.id = prev.id
		 RETURNING prev.avatar_key`, id).Scan(&old)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if err := s.aud.Log(ctx, audit.Entry{ActorID: &id, Action: "user.avatar.clear", Entity: "user", EntityID: id.String()}); err != nil {
		return "", err
	}
	if old == nil {
		return "", nil
	}
	return *old, nil
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

// maxUserPage caps one page of List. The screen pages at 15; this ceiling only
// bounds what a hand-written request can ask for.
const maxUserPage = 500

// UserListFilter is every knob the users screen can turn. It is a struct rather
// than more positional arguments because role/search/status/sort are all
// optional strings — as parameters they were trivially swappable at the call
// site with nothing to catch it.
//
// Status and Sort belong HERE, on the server, not in the browser. The screen
// pages at 15 rows; filtering or sorting a page after the fact would drop rows
// out of a page that still claimed to hold 15, and would sort only within the
// page — the first page of a name sort would not be the first 15 names.
type UserListFilter struct {
	Role   string // "" = any role
	Search string // matches email, first/last name, or student id
	Status string // "", "active", "inactive"
	Sort   string // "name" (default) or "email"
	Desc   bool
	Limit  int
	Offset int
}

// orderBy maps the sort key to SQL. It is a whitelist, never interpolation of
// caller input: the ORDER BY clause cannot be parameterized, so an unknown key
// must fall back to the default rather than reach the query.
func (f UserListFilter) orderBy() string {
	dir := "ASC"
	if f.Desc {
		dir = "DESC"
	}
	switch f.Sort {
	case "email":
		return "u.email " + dir
	default:
		return "u.first_name " + dir + ", u.last_name " + dir
	}
}

func (s *UserService) List(ctx context.Context, f UserListFilter) ([]User, int, error) {
	limit, offset := f.Limit, f.Offset
	// An over-large limit clamps to the ceiling; it must NOT fall back to the
	// default. It used to do both in one condition, so asking for 500 returned
	// the SMALLEST page — the users screen requested 500, got 50, and printed an
	// honest total of 51 above a list missing its last row. The one account that
	// sorts last was invisible, and the count above it said otherwise.
	if limit <= 0 {
		limit = 50
	}
	if limit > maxUserPage {
		limit = maxUserPage
	}
	if offset < 0 {
		offset = 0
	}
	where := "u.deleted_at IS NULL"
	args := []any{}
	i := 1
	if f.Role != "" {
		where += " AND EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id=u.id AND ur.role=$" + itoa(i) + "::role_code)"
		args = append(args, f.Role)
		i++
	}
	if f.Search != "" {
		where += " AND (u.email ILIKE $" + itoa(i) + " OR u.first_name ILIKE $" + itoa(i) + " OR u.last_name ILIKE $" + itoa(i) + " OR COALESCE(u.student_id,'') ILIKE $" + itoa(i) + ")"
		args = append(args, "%"+f.Search+"%")
		i++
	}
	switch f.Status {
	case "active":
		where += " AND u.is_active"
	case "inactive":
		where += " AND NOT u.is_active"
	}
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM users u WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// u.id closes the ordering. Two people can share a name, and LIMIT/OFFSET
	// over a non-unique sort key lets Postgres return tied rows in a different
	// order per query — the same person could appear on two pages while someone
	// else appeared on none.
	q := `SELECT u.id, u.email, u.title, u.first_name, u.last_name, u.phone, u.study_level::text, u.study_year, u.student_id, u.department, u.is_active, u.profile_completed, u.must_change_password, u.avatar_key, u.avatar_updated_at
	      FROM users u WHERE ` + where + ` ORDER BY ` + f.orderBy() + `, u.id
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
		var avatarKey *string
		var avatarAt *time.Time
		if err := rows.Scan(&u.ID, &u.Email, &u.Title, &u.FirstName, &u.LastName, &u.Phone, &u.StudyLevel, &u.StudyYear, &u.StudentID, &u.Department, &u.IsActive, &u.ProfileComplete, &u.MustChangePassword, &avatarKey, &avatarAt); err != nil {
			return nil, 0, err
		}
		u.AvatarURL = avatarURL(u.ID, avatarKey, avatarAt)
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

	// Bank fields on the request are accepted and ignored — see migration
	// 0047. Rejecting them would break older clients for no benefit; silently
	// dropping them is what "never stored" means here.

	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "user.update", Entity: "user", EntityID: id.String(), After: in}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *UserService) Deactivate(ctx context.Context, actor, id uuid.UUID) error {
	if actor == id {
		return Invalid("ไม่สามารถปิดใช้งานบัญชีของตนเองได้")
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "user.deactivate", Entity: "user", EntityID: id.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE users SET is_active = FALSE, updated_at=NOW() WHERE id=$1`, id)
			return err
		})
}

// Activate re-enables a previously deactivated account.
func (s *UserService) Activate(ctx context.Context, actor, id uuid.UUID) error {
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "user.activate", Entity: "user", EntityID: id.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`UPDATE users SET is_active = TRUE, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		})
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
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "user.reset_password", Entity: "user", EntityID: id.String()},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE users SET password_hash=$1, must_change_password=TRUE, updated_at=NOW() WHERE id=$2`, h, id)
			return err
		}); err != nil {
		return "", err
	}
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
