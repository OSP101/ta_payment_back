package handler

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/limiter"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

// Mount wires all routes on the app.
func Mount(app *fiber.App, svc *service.Container, tokens *auth.TokenService, r *rbac.RBAC, aud *audit.Auditor) {
	app.Get("/api/health", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })
	MountAPI(app.Group("/api/v1"), svc, tokens, Authenticated(tokens), r, aud)
}

// MountAPI wires every /api/v1/* route onto api, which may be the app's own
// "/api/v1" group (the only caller in production, via Mount above — same
// behaviour as before this function existed) or any other fiber.Router
// rooted somewhere else.
//
// That "somewhere else" is what the BETA demo sandbox (internal/demo) needs:
// each sandbox slot gets a full, independently-bound copy of every route
// under its own path (e.g. /api/v1/demo/w/3/...), pointed at that slot's own
// service.Container — not this one. authMiddleware is the other half of that:
// Mount always passes Authenticated(tokens) (unchanged), but a demo slot
// passes its own middleware reading a differently-named, differently-scoped
// cookie, so a demo login can never be read as a real session or vice versa
// (see internal/demo.DemoAuthenticated's doc comment for why that matters —
// Login below sets a root-scoped "access_token" cookie, which a same-secret
// demo token would collide with).
func MountAPI(api fiber.Router, svc *service.Container, tokens *auth.TokenService, authMiddleware fiber.Handler, r *rbac.RBAC, aud *audit.Auditor) {
	// Gate that fires only for pure-TA users whose ta_profile is not yet
	// approved. Applied selectively below to feature endpoints; profile,
	// documents, and account endpoints are left open so the TA can complete
	// onboarding.
	taApproved := RequireApprovedTAProfile(svc.Pool)

	// Publish the request's identity for the audit package, on EVERY /api/v1
	// route including the anonymous ones. A failed login is the single most
	// important row in the table for "ตามหาคนร้าย" and it is written by an
	// unauthenticated request — before this, those rows had no address at all.
	// Mounted again inside the authed group below, once there is a user and a
	// session to add.
	api.Use(AuditContext())

	// Baseline per-IP ceiling across EVERY /api/v1 endpoint, public and
	// authenticated alike — a broad backstop against scripted flooding,
	// layered UNDER the tighter route-specific limiters below (loginLimiter,
	// heavyLimiter). Generous on purpose: normal multi-tab use, plus
	// SessionActivityGuard's heartbeat (at most 1/min) and the review
	// workspace's 20s poll, sit nowhere near this — it should only ever fire
	// on genuine abuse.
	//
	// Keyed by IP, which inherits the same caveat loginLimiter always had:
	// see config.TrustedProxyIPs. Until that is set in production, every
	// request arrives looking like it came from the Next.js hop, and this
	// budget is shared across the whole user base rather than enforced per
	// visitor.
	api.Use(limiter.New(limiter.Config{
		Max:        600,
		Expiration: time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "ทำรายการบ่อยเกินไป กรุณารอสักครู่แล้วลองใหม่",
			})
		},
	}))

	// Public
	authH := &AuthHandler{Svc: svc, Tokens: tokens, RBAC: r, Aud: aud}
	// Rate-limit login attempts per IP to blunt brute-force / credential
	// stuffing. Layered with the per-ACCOUNT lockout in
	// UserService.Authenticate (login_gate.go): this stops the system being
	// hammered, that stops any one account being brute-forced from many IPs
	// at once — the two close different gaps in each other.
	loginLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "พยายามเข้าสู่ระบบบ่อยเกินไป กรุณารอสักครู่แล้วลองใหม่",
			})
		},
	})
	api.Post("/auth/login", loginLimiter, authH.Login)
	// Same limiter as /auth/login, not the 600/min baseline: this is still
	// step 2 of an anonymous login flow — the caller has a password-verified
	// challenge but no session yet — so it needs the same brute-force ceiling
	// as step 1. The per-account gate in mfa_gate.go is the OTHER half of
	// this defense, and is what actually stops a guessing loop that rotates
	// challenges to dodge a single challenge's own 5-attempt cap; see its doc
	// comment.
	api.Post("/auth/login/2fa", loginLimiter, authH.LoginTwoFactor)
	api.Get("/auth/sso/url", authH.SSOURL)
	// Where the KKU button points. Plants the CSRF nonce, then 302s to KKU
	// — see auth_sso_nonce.go.
	api.Get("/auth/sso/login", loginLimiter, authH.SSOLoginStart)
	// KKU SSONext login, two steps like the password path — see
	// service.SSOService. Both are anonymous login endpoints, so they share
	// /auth/login's brute-force ceiling: exchange is where a guessed code
	// would land, confirm is where a guessed ticket would.
	api.Post("/auth/sso/exchange", loginLimiter, authH.SSOExchange)
	api.Post("/auth/sso/confirm", loginLimiter, authH.SSOConfirm)
	// Shared announcements. Anonymous by design — a link posted to Facebook or
	// LINE is useless if it lands on a login page. The service only answers for
	// rows staff explicitly opened, and only while they are live.
	api.Get("/public/announcements/:id", (&AnnounceHandler{Svc: svc}).PublicGet)
	// Pictures and files belonging to a shared announcement. The handler asks
	// the service whether this exact key is on a public, live announcement, so
	// nothing else in the store is reachable.
	api.Get("/public/announcements/media/*", (&AnnounceHandler{Svc: svc}).ServePublicMedia)
	// Document-progress share links — a staff-issued, per-term link (see
	// migration 0084). Anonymous by the same design as public announcements:
	// only a live link's own id answers; anything else, and a revoked one, is
	// reported as plain "not found".
	publicDPH := &DocProgressHandler{Svc: svc}
	api.Get("/public/document-progress/:linkId", publicDPH.PublicGet)
	api.Get("/public/document-progress/:linkId/checklist", publicDPH.PublicListChecklist)
	// Login-page notice cards: the live TA-request deadline and how far the
	// term's document bundle has travelled. Anonymous, college-wide and
	// person-free; the service caches for a few seconds, so the 600/min
	// baseline is the only ceiling it needs.
	api.Get("/public/login-notices", func(c *fiber.Ctx) error {
		out, err := svc.PublicNotices.Get(c.Context())
		if err != nil {
			return err
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(out)
	})
	api.Post("/auth/logout", authH.Logout)

	// TDBM webhook — called by another SYSTEM, not a signed-in user, so it
	// sits outside the JWT-authed group like the public routes above. No
	// shared-secret gate (deliberate — see TDBMHandler.Webhook's doc comment):
	// the body is never trusted for anything beyond "sync now", so accepting
	// an unauthenticated ping costs at most one extra pull against TDBM.
	tdbmH := &TDBMHandler{Svc: svc}
	// Unauthenticated by design, so it gets its own per-IP ceiling: each ping
	// starts a full pull the moment the previous one finishes, and without a
	// limit anyone could keep TDBM (and our sync log) busy back-to-back.
	webhookLimiter := limiter.New(limiter.Config{
		Max:        6,
		Expiration: time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusTooManyRequests)
		},
	})
	api.Post("/tdbm-webhook", webhookLimiter, tdbmH.Webhook)

	// Authenticated. AccountGuard re-checks live account state (active +
	// must-change-password) on every protected request.
	// AuditDenied sits innermost so it sees the 403 a role guard or a service
	// returns, with the user and session AuditContext has just published.
	authed := api.Group("", authMiddleware, AccountGuard(svc), AuditContext(), AuditDenied(aud))
	editGuard := ConcurrentEdits(svc.Pool, svc.Cfg.JWTSecret)

	// Applied below to routes that generate a PDF/XLSX/ZIP or accept an
	// upload — every one of those costs real CPU, memory, or (for uploads)
	// an antivirus scan, unlike a plain JSON GET, so the 600/min baseline
	// above is far too loose to stop someone scripting repeated hits at one
	// of these from tying up the process. Keyed by user id rather than IP:
	// every route it guards is already authenticated, so — unlike
	// loginLimiter — this one has a real per-visitor identity to key on and
	// does not inherit the trusted-proxy caveat at all.
	heavyLimiter := limiter.New(limiter.Config{
		Max:        15,
		Expiration: time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string {
			return UserID(c).String()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "ทำรายการนี้บ่อยเกินไป กรุณารอสักครู่แล้วลองใหม่",
			})
		},
	})

	authed.Get("/me", editGuard, authH.Me)
	authed.Get("/me/data-export", editGuard, authH.DataExport)
	authed.Put("/me/preferences", editGuard, authH.SetPreferences)
	authed.Post("/me/password", editGuard, authH.ChangePassword)
	// Touched by AccountGuard itself (any POST does) — see AuthHandler.Heartbeat
	// and SessionActivityGuard on the frontend.
	authed.Post("/auth/heartbeat", editGuard, authH.Heartbeat)

	// Two-factor authentication (self-service). Reachable even while
	// AccountGuard's mfa_setup_required is blocking every OTHER endpoint —
	// see that branch's own "/me/2fa/" allowlist entry — since these are
	// exactly the endpoints someone in that state needs to complete enrolment.
	mfaH := &MFAHandler{Svc: svc}
	authed.Post("/me/2fa/setup", editGuard, mfaH.Setup)
	authed.Post("/me/2fa/enable", editGuard, mfaH.Enable)
	authed.Post("/me/2fa/disable", editGuard, mfaH.Disable)
	authed.Post("/me/2fa/recovery-codes", editGuard, mfaH.RegenerateRecoveryCodes)

	// Fails on purpose: proves error reporting works end to end. See
	// ErrorReportTest.
	authed.Post("/admin/error-report-test", RequireRole(rbac.RoleAdmin), editGuard, ErrorReportTest)

	// Users (admin, staff)
	adminOrStaff := RequireRole(rbac.RoleAdmin, rbac.RoleStaff)
	uh := &UserHandler{Svc: svc}
	// Lecturers are additionally allowed here but are restricted to role=ta inside the handler
	// Profile picture. Writes are self-service only (the path says "me", the
	// handler reads the id from the session), so no role gate belongs here —
	// every signed-in user owns their own face. Reading is open to any signed-in
	// user so rosters and review queues can render.
	authed.Post("/me/avatar", heavyLimiter, editGuard, uh.UploadAvatar)
	authed.Delete("/me/avatar", editGuard, uh.DeleteAvatar)
	authed.Get("/users/:id/avatar", editGuard, uh.ServeAvatar)
	// Reading the roster is a bulk PII disclosure — every account's email and
	// student id — so it is recorded, throttled to one entry per viewer per
	// hour. See audit_read.go for why the throttle rather than one row per
	// request.
	authed.Get("/users", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer),
		AuditRead(aud, "users.list.view", "user", ""), editGuard, uh.List)
	authed.Post("/users", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, uh.Create)
	// Same role gate as Create above — whoever can create a user can also set
	// the picture, since the create form collects it before the account (and
	// its self-serve /me/avatar) exists.
	authed.Post("/users/:id/avatar", heavyLimiter, RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, uh.UploadAvatarFor)
	// Subject is the :id, so looking up two different people files two entries:
	// the question here is "who has been reading THIS person's record". A
	// self-read is recorded like any other and needs no special case — actor
	// and subject are simply equal, which reads correctly on the screen.
	authed.Get("/users/:id", authed_forSelfOrStaff(),
		AuditRead(aud, "user.record.view", "user", "id"), editGuard, uh.Get)
	authed.Patch("/users/:id", adminOrStaff, editGuard, uh.Update)
	authed.Post("/users/:id/reset-password", adminOrStaff, editGuard, uh.ResetPassword)
	authed.Post("/users/:id/send-credentials", heavyLimiter, RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, uh.SendCredentials)
	authed.Post("/users/:id/deactivate", adminOrStaff, editGuard, uh.Deactivate)
	authed.Post("/users/:id/activate", adminOrStaff, editGuard, uh.Activate)
	// Admin-only, not adminOrStaff: clearing the re-auth lockout REVERSES a
	// security control, and this codebase keeps reversals with admin while staff
	// move work forward. Staff are also the people the gate protects against a
	// stolen session, so they must not be able to clear each other's lockouts.
	authed.Post("/users/:id/unlock-password-gate", RequireRole(rbac.RoleAdmin), editGuard, uh.UnlockPasswordGate)
	// Staff may reset 2FA for TA and lecturer accounts — the help-desk case of
	// a lost phone, which staff handle at the counter (requested 02/10/2026).
	// The handler applies the SAME target rule as /users/:id/reset-password:
	// an admin/staff/executive target needs an admin, so staff still cannot
	// chain password reset + 2FA reset into taking over a privileged account.
	authed.Post("/users/:id/2fa/reset", adminOrStaff, editGuard, mfaH.AdminReset)

	// Education-level history (migration 0094, ta_enrollments) — a TA may see
	// their own history (self-service transparency), same self-or-staff gate
	// as GET /users/:id above; only staff/admin may record a new transition —
	// see EnrollmentService.RecordTransition's doc comment for why this stays
	// staff-authored rather than self-service.
	enrollH := &EnrollmentHandler{Svc: svc}
	authed.Get("/users/:id/enrollments", authed_forSelfOrStaff(), editGuard, enrollH.List)
	authed.Post("/users/:id/enrollments", adminOrStaff, editGuard, enrollH.RecordTransition)
	// Typo fix of the TA's CURRENT student id / citizen id / prefix (no new
	// period) — the TA shows their card at the office and staff correct it.
	authed.Get("/users/:id/ta-identity", adminOrStaff, editGuard, enrollH.GetTAIdentity)
	authed.Post("/users/:id/ta-identity", adminOrStaff, editGuard, enrollH.CorrectTAIdentity)
	authed.Post("/users/:id/creditor-form/regenerate", adminOrStaff, heavyLimiter, editGuard, enrollH.RegenerateCreditorForm)
	// Which enrollment period THIS session is viewing — a TA-only display
	// filter (see migration 0096), set by the login-time picker
	// (EnrollmentScopeModal on the frontend). Self-only by construction: the
	// service validates the enrollment belongs to UserID(c), not a :id param.
	authed.Post("/me/enrollment-scope", editGuard, enrollH.SetSessionScope)

	// Pay-rate settings (admin, staff). The faculty course catalog was removed
	// — course identity now lives per-term on teaching_courses. The manual
	// per-course budget cap (/settings/budget-cap) was removed 15/09/2026
	// (TOR §3.4 ข.4) — BudgetService.Compute derives the ceiling from the
	// workload formula and never read it; see CourseService's doc comment.
	ch := &CourseHandler{Svc: svc}
	authed.Get("/settings/pay-rate", editGuard, ch.PayRate)
	authed.Post("/settings/pay-rate", adminOrStaff, editGuard, ch.CreatePayRate)
	// Versions saved ahead of time: listed for staff, withdrawn only by admin
	// (staff move work forward, only admin reverses) and only before their date.
	authed.Get("/settings/pay-rate/scheduled", adminOrStaff, editGuard, ch.ScheduledPayRates)
	authed.Delete("/settings/pay-rate/:id", RequireRole(rbac.RoleAdmin), editGuard, ch.DeleteScheduledPayRate)

	// Terms
	th := &TeachingHandler{Svc: svc}
	authed.Get("/terms", editGuard, th.ListTerms)
	authed.Get("/terms/years/count", adminOrStaff, editGuard, th.TermYearsCount)
	authed.Post("/terms", adminOrStaff, editGuard, th.UpsertTerm)
	authed.Get("/terms/:id/usage", adminOrStaff, editGuard, th.TermUsage)
	authed.Delete("/terms/:id", adminOrStaff, editGuard, th.DeleteTerm)
	authed.Post("/terms/:id/num-students/bulk", adminOrStaff, heavyLimiter, editGuard, th.BulkNumStudents)
	authed.Get("/reg-enrolment/sources", adminOrStaff, editGuard, th.RegSources)
	authed.Get("/terms/:id/reg-enrolment", adminOrStaff, editGuard, th.RegEnrolment)
	authed.Post("/terms/:id/reg-enrolment", adminOrStaff, heavyLimiter, editGuard, th.StartRegEnrolment)
	authed.Delete("/terms/:id/reg-enrolment", adminOrStaff, editGuard, th.StopRegEnrolment)
	authed.Patch("/terms/:id/budget-cap", adminOrStaff, editGuard, th.SetTermBudgetCap)

	// Curricula — the sheet identity the two payout documents (สรุปรายวิชาที่
	// ขอใช้ TA, ปะหน้าจ่ายตรง) print under, plus the course-group review flow
	// for courses taught under more than one registrar code.
	authed.Get("/curricula", editGuard, th.ListCurricula)
	authed.Patch("/curricula/:code", adminOrStaff, editGuard, th.UpdateCurriculum)
	// Contact block at the foot of every notification e-mail.
	dph := &DocumentPolicyHandler{Svc: svc}
	authed.Get("/settings/documents", adminOrStaff, editGuard, dph.Get)
	authed.Put("/settings/documents", adminOrStaff, editGuard, dph.Update)
	msh := &MailSettingsHandler{Svc: svc}
	authed.Get("/mail-settings", adminOrStaff, editGuard, msh.Get)
	authed.Put("/mail-settings", adminOrStaff, editGuard, msh.Update)
	authed.Post("/mail-settings/preview", adminOrStaff, editGuard, msh.Preview)
	authed.Get("/mail-settings/server", adminOrStaff, editGuard, msh.Server)
	authed.Post("/mail-settings/check", adminOrStaff, editGuard, msh.Check)
	authed.Post("/mail-settings/test-send", adminOrStaff, editGuard, msh.TestSend)
	authed.Patch("/sections/:id/curriculum", adminOrStaff, editGuard, th.UpdateSectionCurriculum)
	authed.Get("/terms/:id/course-groups/candidates", adminOrStaff, editGuard, th.CourseGroupCandidates)
	authed.Post("/terms/:id/course-groups", adminOrStaff, editGuard, th.ConfirmCourseGroup)

	// สรุปรายวิชาที่ขอใช้ TA — the budget-request workbook staff assembled by
	// hand at the start of every term.
	authed.Get("/exports/terms/:id/course-summary/warnings", adminOrStaff, editGuard, th.CourseSummaryWarnings)
	authed.Get("/exports/terms/:id/course-summary/months", adminOrStaff, editGuard, th.CourseSummaryMonths)
	authed.Get("/exports/terms/:id/course-summary.xlsx", adminOrStaff, heavyLimiter, editGuard, th.CourseSummaryXLSX)
	authed.Get("/exports/terms/:id/course-summary/preview", adminOrStaff,
		AuditRead(aud, "export.course_summary.preview", "term", "id"), editGuard, th.CourseSummaryPreview)
	authed.Get("/exports/terms/:id/course-summary/history", adminOrStaff, editGuard, th.CourseSummaryHistory)
	authed.Get("/exports/course-summary/:id/reprint", adminOrStaff, heavyLimiter, editGuard, th.CourseSummaryReprint)

	// Teaching courses
	authed.Get("/teaching-courses", editGuard, th.List)
	// Timetable kinds (บรรยาย/ปฏิบัติการ) for a term — any signed-in user.
	authed.Get("/class-kinds", editGuard, th.ClassKinds)
	// Opening a course is staff's job: courses come from the registrar import,
	// and a lecturer-opened one would carry a section roster nobody at the
	// registrar knows about. TeachingService.Create enforces this again.
	authed.Post("/teaching-courses", adminOrStaff, editGuard, th.Create)
	authed.Get("/teaching-courses/:id", editGuard, th.Get)
	authed.Delete("/teaching-courses/:id", adminOrStaff, editGuard, th.Delete)
	// Course identity, headcount, date range and the section roster all trace
	// back to the registrar file, so they are staff's. The one exception is
	// the schedule PUT: a lecturer may fill in a timetable the file left blank,
	// exactly once — the rule lives in TeachingService.ReplaceSectionSchedules,
	// which is why the route still admits them.
	authed.Patch("/teaching-courses/:id/num-students", adminOrStaff, editGuard, th.SetNumStudents)
	authed.Patch("/teaching-courses/:id/settings", adminOrStaff, editGuard, th.UpdateSettings)
	// Lecturer attribution is set at open/import time only (Create,
	// CommitImport); this is the only way to correct or change it afterwards —
	// see ReplaceLecturers. Staff/admin only, same as every other correction to
	// registrar-sourced course identity.
	authed.Put("/teaching-courses/:id/lecturers", adminOrStaff, editGuard, th.ReplaceLecturers)
	// Course identity — the registrar file arrives with typos, and rebuilding a
	// course to fix one meant re-entering every section and schedule by hand.
	authed.Patch("/teaching-courses/:id/info", adminOrStaff, editGuard, th.UpdateCourseInfo)
	authed.Post("/teaching-courses/:id/merge-code", adminOrStaff, editGuard, th.MergeCourseCode)
	authed.Post("/teaching-courses/:id/sections", adminOrStaff, editGuard, th.AddSection)
	authed.Patch("/teaching-courses/:id/sections/:sectionId", adminOrStaff, editGuard, th.UpdateSection)
	authed.Delete("/teaching-courses/:id/sections/:sectionId", adminOrStaff, editGuard, th.DeleteSection)
	authed.Post("/teaching-courses/:id/sections/:sectionId/fold", adminOrStaff, editGuard, th.FoldSection)
	authed.Put("/teaching-courses/:id/sections/:sectionId/schedules", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.ReplaceSectionSchedules)
	// Makeups (วันชดเชย) are lecturer/staff/admin only (faculty decision
	// 27/09/2026: dates come from TDBM and the course, not from TAs). A TA
	// asks the lecturer through holiday-impacts/:date/remind below.
	authed.Post("/teaching-courses/:id/makeup/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.AddMakeup)
	authed.Post("/teaching-courses/:id/makeup/:sectionId/waive", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.WaiveMakeup)
	authed.Delete("/teaching-courses/:id/makeup/:sectionId/:makeupId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.DeleteMakeup)
	authed.Put("/teaching-courses/:id/makeup/:sectionId/:makeupId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.ReplaceMakeup)
	authed.Get("/teaching-courses/:id/holiday-impacts", editGuard, th.HolidayImpacts)
	authed.Post("/teaching-courses/:id/holiday-impacts/:originalDate/remind", RequireRole(rbac.RoleTA), taApproved, editGuard, th.RemindLecturerAboutMakeup)
	authed.Post("/teaching-courses/:id/review-date/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.AddReviewDate)
	// Staff/admin only — matches TeachingService.CommitImport/PreviewImport's own
	// isPrivileged() check. RoleLecturer used to be allowed here too, which the
	// service silently refused (ErrForbidden) — the route now says what the
	// service has always enforced: the registrar file is the office's to
	// import, never the lecturer's (TOR §3.3 ข.1, corrected 15/09/2026).
	authed.Post("/teaching-courses/import", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), heavyLimiter, editGuard, th.ImportExcel)
	// Import history — TOR §3.3 ข.5. Neither path can collide with
	// "/teaching-courses/:id" (line above): that route matches exactly one
	// segment after the prefix, and both of these have two.
	authed.Get("/teaching-courses/import/history", adminOrStaff, editGuard, th.ImportHistory)
	authed.Get("/teaching-courses/import/history/:id", adminOrStaff, editGuard, th.ImportHistoryDetail)
	// Budget is management data (course pay-rate/cap usage): restrict to the
	// same roles that manage teaching courses. A per-course lecturer-ownership
	// check (only the course's own lecturer) belongs in the service/handler.
	authed.Get("/teaching-courses/:id/budget", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, th.Budget)

	// TA request windows
	rh := &TARequestHandler{Svc: svc}
	authed.Get("/ta-request/windows", editGuard, rh.ListWindows)
	authed.Post("/ta-request/windows", adminOrStaff, editGuard, rh.UpsertWindow)
	authed.Get("/ta-request/windows/readiness", adminOrStaff, editGuard, rh.WindowReadiness)
	authed.Delete("/ta-request/windows/:id", adminOrStaff, editGuard, rh.DeleteWindow)

	// TA requests
	authed.Get("/ta-requests", editGuard, rh.List)
	authed.Get("/ta-requests/preview-conflicts", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, rh.PreviewConflicts)
	authed.Get("/ta-requests/candidates", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, rh.Candidates)
	authed.Get("/ta-requests/:id", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), editGuard, rh.Detail)
	// Staff may file for a lecturer (lecturer_id in the body) — 03/10/2026.
	authed.Post("/ta-requests", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, rh.Create)
	authed.Post("/ta-requests/:id/cancel", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, rh.Cancel)
	authed.Post("/ta-requests/:id/sections", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, rh.AddSections)
	// Workload correction — staff/admin only. Cancel above refuses once
	// work_logs exist and its own error message says "contact staff"; this is
	// the tool that message used to point at without one existing.
	authed.Get("/teaching-courses/:tcId/ta/:taId/workload", adminOrStaff, editGuard, rh.ListAssignmentWorkloadsForTA)
	authed.Patch("/assignments/:id/workload", adminOrStaff, editGuard, rh.UpdateAssignmentWorkload)

	// Docs (TA)
	dh := &DocsHandler{Svc: svc}
	authed.Get("/me/profile", RequireRole(rbac.RoleTA), editGuard, dh.GetProfile)
	authed.Put("/me/profile", RequireRole(rbac.RoleTA), editGuard, dh.UpsertProfile)
	authed.Post("/me/pdpa-consent", RequireRole(rbac.RoleTA), editGuard, dh.RecordPdpaConsent)
	authed.Post("/me/citizen-id/reveal", RequireRole(rbac.RoleTA), editGuard, dh.RevealCitizenID)

	// PDPA data-deletion requests. Review is admin-only (not adminOrStaff) —
	// erasure is comparable in sensitivity to unlock-password-gate/2fa-reset/
	// audit-logs, the other admin-only actions in this router, unlike routine
	// TA-submission review queues (ta-review/*, worklog approve/reject) which
	// staff can action too.
	ddh := &DataDeletionHandler{Svc: svc}
	authed.Post("/me/data-deletion-request", RequireRole(rbac.RoleTA), editGuard, ddh.RequestDeletion)
	authed.Get("/me/data-deletion-request", RequireRole(rbac.RoleTA), editGuard, ddh.MyRequest)
	authed.Get("/staff/data-deletion-requests", RequireRole(rbac.RoleAdmin), editGuard, ddh.ListRequests)
	authed.Post("/staff/data-deletion-requests/:id/review", RequireRole(rbac.RoleAdmin), editGuard, ddh.Review)
	// 333 KB of JSON, ~74 KB gzipped — the one response worth compressing here.
	authed.Get("/thai-address", compress.New(compress.Config{Level: compress.LevelBestSpeed}), editGuard, dh.ThaiAddress)
	authed.Get("/me/documents", RequireRole(rbac.RoleTA), editGuard, dh.ListDocs)
	authed.Post("/me/documents", RequireRole(rbac.RoleTA), heavyLimiter, editGuard, dh.UploadDoc)
	authed.Get("/me/history", RequireRole(rbac.RoleTA), editGuard, dh.SelfHistory)
	authed.Post("/me/creditor-form/preview.pdf", RequireRole(rbac.RoleTA, rbac.RoleAdmin, rbac.RoleStaff), heavyLimiter, editGuard, dh.CreditorFormPDF)
	authed.Post("/me/creditor-form/confirm", RequireRole(rbac.RoleTA), editGuard, dh.ConfirmCreditorForm)
	authed.Get("/creditor-form/blank.pdf", heavyLimiter, editGuard, dh.BlankCreditorForm)
	authed.Get("/documents/:id/download", editGuard, dh.Download)
	authed.Get("/ta-review", adminOrStaff, editGuard, dh.ListPending)
	authed.Get("/ta-review/:userId/docs", adminOrStaff, editGuard, dh.ListDocsForUser)
	authed.Get("/ta-review/:userId/history", adminOrStaff, editGuard, dh.History)
	authed.Post("/ta-review/:userId/profile", adminOrStaff, editGuard, dh.ReviewProfile)
	authed.Post("/ta-review/docs/:id", adminOrStaff, editGuard, dh.ReviewDoc)
	// Redesigned officer review: batch approve/reject in the list row +
	// watermarked preview + one-shot ZIP download after approve.
	authed.Post("/ta-review/:userId/approve-all", adminOrStaff, editGuard, dh.ApproveAll)
	authed.Post("/ta-review/:userId/reject-batch", adminOrStaff, editGuard, dh.RejectBatch)
	authed.Post("/ta-review/:userId/zip-token", adminOrStaff, editGuard, dh.MintZipToken)
	// Finance office Template-Suppliers sheet: the term's new TAs (suppliers.go).
	authed.Get("/ta-review/suppliers", adminOrStaff, editGuard, dh.SuppliersPreview)
	authed.Post("/ta-review/suppliers.xlsx", adminOrStaff, heavyLimiter, editGuard, dh.SuppliersXLSX)
	// Bulk: every approved TA's documents merged into one file. Two path
	// segments, so these never collide with the three-segment ":userId" routes
	// above — no ordering dependency to get wrong later.
	authed.Post("/ta-review/download-all-token", adminOrStaff, editGuard, dh.MintAllApprovedZipToken)
	authed.Get("/ta-review/download-all.zip", adminOrStaff, heavyLimiter, editGuard, dh.DownloadAllZip)
	authed.Get("/ta-review/:userId/download.zip", adminOrStaff, heavyLimiter, editGuard, dh.DownloadZip)
	authed.Get("/ta-review/:userId/docs/:docId/preview", adminOrStaff, editGuard, dh.PreviewWatermarked)

	// TA: my assigned courses (year+term filter on FE).
	// Read-only endpoints are ungated so an un-approved TA can still browse
	// what their eventual dashboard will look like; the visible warning
	// banner tells them why nothing can be edited yet.
	authed.Get("/me/ta-courses", RequireRole(rbac.RoleTA), editGuard, th.ListMyTACourses)
	authed.Get("/timetable-form", editGuard, th.TimetableForm)
	authed.Get("/timetable-form.pdf", heavyLimiter, editGuard, th.TimetableFormPDF)
	authed.Get("/me/assignments", RequireRole(rbac.RoleTA), editGuard, th.ListMyAssignments)
	// Courses that asked for this TA and are still waiting on a verdict —
	// shown on the TA home so they know before approval, and why it waits.
	authed.Get("/me/ta-requests/pending", RequireRole(rbac.RoleTA), editGuard, rh.MyPending)

	// Workload / TA class schedule
	wh := &WorkloadHandler{Svc: svc}
	authed.Get("/me/schedule", RequireRole(rbac.RoleTA), editGuard, wh.ListClasses)
	// PUT /me/schedule intentionally does NOT require an approved profile —
	// TAs can build their weekly class timetable while their documents are
	// still under review. Worklog submission stays gated below.
	authed.Put("/me/schedule", RequireRole(rbac.RoleTA), editGuard, wh.ReplaceClasses)
	// Staff correction of a TA's timetable after approval (the TA's own save
	// can only add classes once a request is approved — see
	// assertNoClassRemovedAfterApproval).
	authed.Get("/users/:id/schedule", adminOrStaff, editGuard, wh.ListClassesForTA)
	authed.Put("/users/:id/schedule", adminOrStaff, editGuard, wh.ReplaceClassesForTA)

	// Work logs — writes are gated, GET stays open so the TA can preview.
	wl := &WorkLogHandler{Svc: svc}
	authed.Post("/assignments/:id/worklog/generate", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.Generate)
	authed.Get("/assignments/:id/worklog", editGuard, wl.List)
	// TA-only, read-only — the rate this assignment pays, so the "ส่งอนุมัติ"
	// dialog can show "≈ ฿X" per month before the TA commits. No taApproved
	// gate: same reasoning as the List route above, this only previews.
	authed.Get("/assignments/:id/worklog/pay-rate", RequireRole(rbac.RoleTA), editGuard, wl.PayRate)
	authed.Put("/assignments/:id/worklog", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.Upsert)
	authed.Delete("/assignments/:id/worklog/:logId", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.Delete)
	authed.Post("/assignments/:id/worklog/submit", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.Submit)
	authed.Post("/assignments/:id/worklog/approve", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.Approve)
	// Several assignments, one transaction — a TA on two sections is approved
	// wholly or not at all. Not under /assignments/:id because the batch is the
	// unit here, not any one assignment.
	authed.Post("/worklog/approve-batch", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.ApproveBatch)
	authed.Post("/assignments/:id/worklog/reject", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.Reject)
	// Lecturer corrections during review: fix or cut a submitted sitting with a
	// reason the TA sees (work_log_lecturer_changes). Course ownership is
	// checked in the service.
	authed.Post("/worklogs/:logId/review-edit", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.ReviewEdit)
	authed.Post("/worklogs/:logId/review-cut", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.ReviewCut)
	authed.Get("/assignments/:id/worklog/changes", editGuard, wl.Changes)
	// TA-owned weekly review pattern — TA self-service, seeds review entries
	// on auto-generate. Approval gate mirrors the worklog write endpoints so
	// unapproved TAs can't seed a schedule that later blows past caps.
	authed.Get("/assignments/:id/review-schedules", RequireRole(rbac.RoleTA), editGuard, wl.ListTAReviewSchedules)
	authed.Get("/assignments/:id/schedule-busy", RequireRole(rbac.RoleTA), editGuard, wl.ScheduleBusy)
	authed.Post("/assignments/:id/review-schedules", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.AddTAReviewSchedule)
	authed.Patch("/assignments/:id/review-schedules/:rsId", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.UpdateTAReviewSchedule)
	authed.Delete("/assignments/:id/review-schedules/:rsId", RequireRole(rbac.RoleTA), taApproved, editGuard, wl.DeleteTAReviewSchedule)
	// Lecturer's list of submitted work-logs awaiting review across their courses.
	authed.Get("/reports/pending", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.PendingReports)
	// Per-course history of the lecturer's own approve/reject actions.
	authed.Get("/teaching-courses/:id/approval-history", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, wl.ApprovalHistory)

	// Staff worklog editor (Phase 5): read all TAs' entries per course, edit
	// or remove a row before export. Enforces same business rules as TA path.
	// Lecturers were added here by the 24/07/2026 meeting ("อาจารย์...แก้ไขได้").
	// The route only widens who may call; the per-course ownership check lives
	// in the service, so a lecturer still cannot reach another lecturer's rows.
	staffOrLecturer := RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer)
	authed.Get("/staff/courses/:tcId/worklogs", staffOrLecturer, editGuard, wl.StaffListByCourse)
	authed.Get("/staff/courses/:tcId/assignments", staffOrLecturer, editGuard, wl.StaffListAssignments)
	authed.Put("/staff/worklogs", staffOrLecturer, editGuard, wl.StaffUpsert)
	authed.Delete("/staff/worklogs/:id", staffOrLecturer, editGuard, wl.StaffDelete)

	// Notifications
	nh := &NotifyHandler{Svc: svc}
	authed.Get("/me/notifications", editGuard, nh.List)
	authed.Get("/me/notifications/unread-count", editGuard, nh.UnreadCount)
	authed.Post("/me/notifications/:id/read", editGuard, nh.MarkRead)
	authed.Post("/me/notifications/read-all", editGuard, nh.MarkAllRead)

	// Announcements. Image endpoints sit under the same group so the auth
	// wall applies uniformly — a would-be scraper still needs a session.
	// Register the more specific image routes first: Fiber matches in
	// declaration order, so /:id would swallow "images/..." if placed above.
	ah := &AnnounceHandler{Svc: svc}
	authed.Post("/announcements/upload-image", adminOrStaff, heavyLimiter, editGuard, ah.UploadImage)
	authed.Post("/announcements/upload-media", adminOrStaff, heavyLimiter, editGuard, ah.UploadMedia)
	authed.Get("/announcements/media/*", editGuard, ah.ServeMedia)
	authed.Get("/announcements/images/*", editGuard, ah.ServeImage)
	authed.Get("/announcements", editGuard, ah.List)
	authed.Post("/announcements", adminOrStaff, editGuard, ah.Upsert)
	// These two literal paths MUST stay above "/announcements/:id": Fiber
	// matches in registration order, so ":id" registered first would swallow
	// them and try to parse "audience-filters" as a UUID.
	authed.Post("/announcements/preview-audience", adminOrStaff, editGuard, ah.AudiencePreview)
	authed.Get("/announcements/audience-filters", adminOrStaff, editGuard, ah.AudienceFilters)
	authed.Post("/announcements/send-test", adminOrStaff, heavyLimiter, editGuard, ah.SendTest)
	authed.Get("/announcements/backgrounds", adminOrStaff, editGuard, ah.ListBackgrounds)
	authed.Post("/announcements/backgrounds", adminOrStaff, heavyLimiter, editGuard, ah.UploadBackground)
	authed.Delete("/announcements/backgrounds/:id", adminOrStaff, editGuard, ah.DeleteBackground)
	authed.Get("/announcements/:id", editGuard, ah.Get)
	authed.Delete("/announcements/:id", adminOrStaff, editGuard, ah.Delete)
	authed.Post("/announcements/:id/publish", adminOrStaff, editGuard, ah.Publish)
	authed.Post("/announcements/:id/unpublish", adminOrStaff, editGuard, ah.Unpublish)
	authed.Post("/announcements/:id/send-email", adminOrStaff, editGuard, ah.Resend)
	authed.Post("/announcements/:id/remind", adminOrStaff, editGuard, ah.Remind)
	authed.Post("/announcements/:id/read", editGuard, ah.MarkRead)

	// Dashboard
	dashH := &DashboardHandler{Svc: svc}
	// The single widest disclosure in the product: every TA's hours and money
	// across every course, on one page.
	authed.Get("/dashboard/executive", RequireRole(rbac.RoleAdmin, rbac.RoleStaff),
		AuditRead(aud, "dashboard.executive.view", "academic_term", ""), editGuard, dashH.Executive)
	// The analytics view is the one thing the executive role can see.
	authed.Get("/dashboard/analytics", RequireExecutiveView(svc.Pool), editGuard, dashH.Analytics)
	authed.Get("/dashboard/analytics.xlsx", RequireExecutiveView(svc.Pool), heavyLimiter, editGuard, dashH.AnalyticsXLSX)
	authed.Get("/dashboard/ta/me", RequireRole(rbac.RoleTA), editGuard, dashH.TaOverview)
	authed.Get("/dashboard/lecturer/me", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), editGuard, dashH.LecturerOverview)

	// Export
	eh := &ExportHandler{Svc: svc}
	authed.Post("/exports/terms/:id/monthly-payout.zip", adminOrStaff, heavyLimiter, editGuard, eh.MonthlyPayoutZip)
	authed.Post("/exports/course/:id.zip", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), heavyLimiter, editGuard, eh.CourseZip)
	// Admin-only escape hatch to undo an accidental export lock.
	authed.Post("/exports/course/:id/unlock", RequireRole(rbac.RoleAdmin), editGuard, eh.UnlockCourse)
	// Read-only payout preview — review the numbers before the locking download.
	// The preview shows the same per-person figures the exported file carries,
	// and unlike the download (export.course) it left no trace at all.
	authed.Get("/exports/course/:id/preview", RequireRole(rbac.RoleAdmin, rbac.RoleStaff),
		AuditRead(aud, "export.course.preview", "teaching_course", "id"), editGuard, eh.CoursePreview)
	authed.Get("/exports/course/:id/coverage", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), editGuard, eh.CourseExportCoverage)
	// A TA's own ใบเบิกเวลา for one course, to check against their records.
	// A plain read — none of the course export's locking or gates.
	authed.Get("/me/ta-courses/:tcId/claim-sheet/months", RequireRole(rbac.RoleTA), editGuard, eh.TAClaimMonths)
	authed.Get("/me/ta-courses/:tcId/claim-sheet.xlsx", RequireRole(rbac.RoleTA), heavyLimiter, editGuard, eh.TAClaimSheet)
	// No role guard: the service checks that the caller teaches or assists the
	// course. A budget that decides a TA's own pay is not a staff secret.
	authed.Get("/teaching-courses/:tcId/budget-settlement", editGuard, eh.BudgetSettlement)
	// TA planner facts — the lecturer teaching the course (service-checked) or staff.
	authed.Get("/teaching-courses/:tcId/ta-plan", editGuard, eh.PlanFacts)
	// The lecturer's unsent request form, kept server-side so it survives a
	// refresh, a timed-out session or a change of machine.
	authed.Get("/teaching-courses/:tcId/request-draft", editGuard, eh.GetRequestDraft)
	authed.Put("/teaching-courses/:tcId/request-draft", editGuard, eh.PutRequestDraft)
	authed.Delete("/teaching-courses/:tcId/request-draft", editGuard, eh.DeleteRequestDraft)
	// Same reasoning, and the service checks the caller teaches the course: which
	// months of the term get paid is the lecturer's call, not staff's alone.
	authed.Patch("/teaching-courses/:tcId/settlement-mode", editGuard, eh.SetSettlementMode)
	// Phase 4 exports dashboard.
	authed.Get("/exports/summary", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), editGuard, eh.CoursesSummary)
	authed.Get("/exports/course/:id/history", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), editGuard, eh.CourseHistory)
	// Re-download a past export's exact file, from its history-list entry.
	authed.Get("/exports/batch/:id/download", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), heavyLimiter, editGuard, eh.BatchDownload)
	// Appointment order (คำสั่งแต่งตั้ง) — PDF + DOCX in one ZIP.
	// Issued in rounds: names already printed are never reprinted, so a late
	// round carries only the courses that were not ready earlier.
	// Who signs the ผู้รับรอง block on a term's claim forms. Under /exports
	// rather than /terms because it is a property of the documents this package
	// produces, not of the academic calendar.
	authed.Get("/exports/terms/:id/certifier", adminOrStaff, editGuard, eh.Certifier)
	authed.Put("/exports/terms/:id/certifier", adminOrStaff, editGuard, eh.SetCertifier)
	// ปะหน้าจ่ายตรง (แจ้งโอนจ่ายตรงเข้าบัญชีบุคลากร) — gated on every course in
	// the term reaching finance_sent, unlike the course-summary above. The
	// three routes that hand over the FILE are POST with the caller's password
	// in the body (see transferCoverStepUp): it carries full citizen IDs.
	authed.Get("/exports/terms/:id/transfer-cover/blockers", adminOrStaff, editGuard, eh.TransferCoverBlockers)
	authed.Post("/exports/terms/:id/transfer-cover.xlsx", adminOrStaff, heavyLimiter, editGuard, eh.TransferCoverXLSX)
	authed.Post("/exports/terms/:id/transfer-cover-bundle.zip", adminOrStaff, heavyLimiter, editGuard, eh.TransferCoverBundleZIP)
	authed.Get("/exports/terms/:id/transfer-cover/preview", adminOrStaff, editGuard, eh.TransferCoverPreview)
	authed.Get("/exports/terms/:id/transfer-cover/coverage", adminOrStaff, editGuard, eh.TransferCoverCoverage)
	authed.Get("/exports/terms/:id/transfer-cover/history", adminOrStaff, editGuard, eh.TransferCoverHistory)
	authed.Post("/exports/transfer-cover/:id/reprint", adminOrStaff, heavyLimiter, editGuard, eh.TransferCoverReprint)
	authed.Get("/exports/appointment-order/preview", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), editGuard, eh.AppointmentPreview)
	authed.Get("/exports/appointment-order/rounds", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), editGuard, eh.AppointmentRounds)
	authed.Post("/exports/appointment-order/remind-timetable", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), editGuard, eh.AppointmentRemindTimetable)
	authed.Post("/exports/appointment-order", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), heavyLimiter, editGuard, eh.AppointmentOrder)
	// Re-issue a copy of an order already printed. Separate from the POST above
	// because that one CREATES a round; this one only re-renders a stored
	// snapshot, so it must never be reachable by the same verb and path.
	authed.Get("/exports/appointment-order/rounds/:id/download", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), heavyLimiter, editGuard, eh.AppointmentReprint)

	// Physical-document progress board — the off-system signature/routing journey.
	// GET is readable by any authenticated user (shared status); staff/admin update.
	dpH := &DocProgressHandler{Svc: svc}
	authed.Get("/document-progress", editGuard, dpH.Get)
	authed.Post("/document-progress/:termId", adminOrStaff, editGuard, dpH.SetStage)
	// Per-course signature checklist (who hasn't signed). GET readable by anyone;
	// tick + remind are staff/admin only.
	authed.Get("/document-progress/checklist", editGuard, dpH.ListChecklist)
	authed.Post("/document-progress/checklist/:tcId", adminOrStaff, editGuard, dpH.ToggleSignature)
	authed.Post("/document-progress/:termId/remind", adminOrStaff, editGuard, dpH.RemindUnsigned)
	// Public share link — staff/admin issue, view, and revoke it; the link
	// itself is served below, outside the authed group.
	authed.Get("/document-progress/:termId/share-link", adminOrStaff, editGuard, dpH.GetShareLink)
	authed.Post("/document-progress/:termId/share-link", adminOrStaff, editGuard, dpH.CreateShareLink)
	authed.Delete("/document-progress/:termId/share-link", adminOrStaff, editGuard, dpH.RevokeShareLink)

	// Admin officers (executive roster used on generated official docs).
	// GET is open to any authenticated user so document templates can render
	// with the current roster; writes are staff/admin only.
	aoH := &AdminOfficerHandler{Svc: svc}
	authed.Get("/settings/admin-officers", editGuard, aoH.List)
	authed.Post("/settings/admin-officers", adminOrStaff, editGuard, aoH.Upsert)

	// Monthly submission periods (Phase 3): staff CRUD + TA-facing reminders.
	spH := &SubmissionPeriodHandler{Svc: svc}
	authed.Get("/submission-periods", editGuard, spH.List)
	authed.Post("/submission-periods", adminOrStaff, editGuard, spH.Upsert)
	authed.Post("/submission-periods/bulk-for-term/:termId", adminOrStaff, editGuard, spH.BulkForTerm)
	authed.Delete("/submission-periods/:id", adminOrStaff, editGuard, spH.Delete)
	authed.Get("/me/submission-periods", editGuard, spH.MePending)
	// Staff step 3 — ตรวจสอบเบิกจ่ายค่าตอบแทน. Sits between the lecturer's
	// daily approval and the export, which now refuses months that skipped it.
	authed.Get("/submission-periods/review-queue", adminOrStaff,
		AuditRead(aud, "payout.queue.view", "academic_term", ""), editGuard, spH.ReviewQueue)
	// Nudge a course's lecturers about work still sitting in THEIR queue. The
	// merged payout screen groups those months under "waiting on someone else";
	// this is the one action available there.
	authed.Post("/submission-periods/courses/:tcId/remind-lecturer",
		adminOrStaff, editGuard, spH.RemindLecturer)
	// Staff corrections to a TA's month, committed as one accountable batch:
	// reason + password + up to three evidence images, notifying the TA AND the
	// lecturer whose approval is being overridden.
	authed.Post("/submission-periods/courses/:tcId/tas/:taId/worklog-batch",
		adminOrStaff, editGuard, spH.StaffEditBatch)
	authed.Get("/submission-periods/courses/:tcId/tas/:taId/worklog-batches",
		staffOrLecturer, editGuard, spH.StaffEditHistory)
	authed.Get("/worklog-edit-files/*", staffOrLecturer, editGuard, spH.ServeEditFile)
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/staff-review",
		adminOrStaff, editGuard, spH.StaffReview)
	// No digital signatures: the lecturer's daily worklog approval is the
	// review; staff export (which locks the month) then mark it sent to finance.
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/finance-send",
		adminOrStaff, editGuard, spH.FinanceSend)
	// "ตีกลับ" — staff/admin bounce an exported month back to pending (unlock).
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/send-back",
		adminOrStaff, editGuard, spH.SendBack)
	// Admin-only unlock for a month already handed to finance.
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/finance-revert",
		RequireRole(rbac.RoleAdmin), editGuard, spH.FinanceRevert)
	authed.Get("/submission-periods/:id/courses/:tcId/tas/:taId/worklog", adminOrStaff, editGuard, spH.MonthDetail)
	authed.Get("/submission-periods/:id/courses/:tcId/tas/:taId/timeline", editGuard, spH.Timeline)
	authed.Get("/teaching-courses/:tcId/submission-timeline", editGuard, spH.ListByCourse)

	// Public holidays — GET is open to any authenticated user so the TA and
	// lecturer holidays pages can render; writes are staff/admin only. The BOT
	// (Bank of Thailand) sync that used to seed 'national' rows here was
	// removed 2026-09-15: TDBM is now the sole source of truth for holidays
	// (see docs/TDBM-API-requirements.md) — existing BOT-sourced rows are left
	// in place as historical data, just no longer kept fresh by this route.
	hh := &HolidayHandler{Svc: svc}
	authed.Get("/holidays", editGuard, hh.List)
	authed.Post("/holidays", adminOrStaff, editGuard, hh.Create)
	authed.Post("/holidays/bulk", adminOrStaff, editGuard, hh.BulkCreate)
	authed.Patch("/holidays/:id", adminOrStaff, editGuard, hh.Patch)
	authed.Delete("/holidays/:id", adminOrStaff, editGuard, hh.Delete)

	// TDBM sync — manual trigger + history, for staff to confirm the webhook
	// (registered above, outside this group) and hourly sweep are actually
	// working. Read-only history open to staff/admin; the trigger itself too.
	authed.Post("/tdbm/sync-now", adminOrStaff, editGuard, tdbmH.SyncNow)
	authed.Get("/tdbm/sync-log", adminOrStaff, editGuard, tdbmH.SyncLog)
	authed.Get("/tdbm/extra-teachings", adminOrStaff, editGuard, tdbmH.ExtraTeachings)

	// Audit log (admin)
	audH := &AuditHandler{Svc: svc}
	authed.Get("/audit-logs", RequireRole(rbac.RoleAdmin), editGuard, audH.List)
	// Feeds the screen's action filter. Same admin-only gate: the list of action
	// names is itself a map of what the system records.
	authed.Get("/audit-logs/actions", RequireRole(rbac.RoleAdmin), editGuard, audH.Actions)
	authed.Get("/audit-logs/summary", RequireRole(rbac.RoleAdmin), editGuard, audH.Summary)
	authed.Get("/audit-logs/catalog", RequireRole(rbac.RoleAdmin), editGuard, audH.Catalog)
	authed.Get("/audit-logs/sessions", RequireRole(rbac.RoleAdmin), editGuard, audH.Sessions)
	authed.Get("/audit-logs/export", RequireRole(rbac.RoleAdmin), editGuard, audH.Export)
}

// authed_forSelfOrStaff allows the user to fetch their own profile OR staff/admin.
func authed_forSelfOrStaff() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Params("id")
		if id == UserID(c).String() {
			return c.Next()
		}
		if rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
			return c.Next()
		}
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}
}
