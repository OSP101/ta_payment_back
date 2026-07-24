package handler

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

// Mount wires all routes on the app.
func Mount(app *fiber.App, svc *service.Container, tokens *auth.TokenService, r *rbac.RBAC, aud *audit.Auditor) {
	// Gate that fires only for pure-TA users whose ta_profile is not yet
	// approved. Applied selectively below to feature endpoints; profile,
	// documents, and account endpoints are left open so the TA can complete
	// onboarding.
	taApproved := RequireApprovedTAProfile(svc.Pool)
	app.Get("/api/health", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	api := app.Group("/api/v1")

	// Public
	authH := &AuthHandler{Svc: svc, Tokens: tokens, RBAC: r, Aud: aud}
	// Rate-limit login attempts per IP to blunt brute-force / credential stuffing.
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
	api.Post("/auth/sso/callback", authH.SSOCallback) // stub
	api.Get("/auth/sso/url", authH.SSOURL)
	api.Post("/auth/logout", authH.Logout)

	// Authenticated. AccountGuard re-checks live account state (active +
	// must-change-password) on every protected request.
	authed := api.Group("", Authenticated(tokens), AccountGuard(svc.Pool))
	authed.Get("/me", authH.Me)
	authed.Post("/me/password", authH.ChangePassword)

	// Users (admin, staff)
	adminOrStaff := RequireRole(rbac.RoleAdmin, rbac.RoleStaff)
	uh := &UserHandler{Svc: svc}
	// Lecturers are additionally allowed here but are restricted to role=ta inside the handler
	authed.Get("/users", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), uh.List)
	authed.Post("/users", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), uh.Create)
	authed.Get("/users/:id", authed_forSelfOrStaff(), uh.Get)
	authed.Patch("/users/:id", adminOrStaff, uh.Update)
	authed.Post("/users/:id/reset-password", adminOrStaff, uh.ResetPassword)
	authed.Post("/users/:id/deactivate", adminOrStaff, uh.Deactivate)
	authed.Post("/users/:id/activate", adminOrStaff, uh.Activate)

	// Pay-rate & budget-cap settings (admin, staff). The faculty course catalog
	// was removed — course identity now lives per-term on teaching_courses.
	ch := &CourseHandler{Svc: svc}
	authed.Get("/settings/pay-rate", ch.PayRate)
	authed.Post("/settings/pay-rate", adminOrStaff, ch.CreatePayRate)
	authed.Get("/settings/budget-cap", ch.BudgetCap)
	authed.Post("/settings/budget-cap", adminOrStaff, ch.UpsertBudgetCap)

	// Terms
	th := &TeachingHandler{Svc: svc}
	authed.Get("/terms", th.ListTerms)
	authed.Get("/terms/years/count", adminOrStaff, th.TermYearsCount)
	authed.Post("/terms", adminOrStaff, th.UpsertTerm)
	authed.Get("/terms/:id/usage", adminOrStaff, th.TermUsage)
	authed.Delete("/terms/:id", adminOrStaff, th.DeleteTerm)

	// Teaching courses
	authed.Get("/teaching-courses", th.List)
	// Timetable kinds (บรรยาย/ปฏิบัติการ) for a term — any signed-in user.
	authed.Get("/class-kinds", th.ClassKinds)
	authed.Post("/teaching-courses", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.Create)
	authed.Get("/teaching-courses/:id", th.Get)
	authed.Delete("/teaching-courses/:id", adminOrStaff, th.Delete)
	authed.Patch("/teaching-courses/:id/num-students", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.SetNumStudents)
	authed.Patch("/teaching-courses/:id/settings", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.UpdateSettings)
	authed.Post("/teaching-courses/:id/sections", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.AddSection)
	authed.Patch("/teaching-courses/:id/sections/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.UpdateSection)
	authed.Delete("/teaching-courses/:id/sections/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.DeleteSection)
	authed.Put("/teaching-courses/:id/sections/:sectionId/schedules", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.ReplaceSectionSchedules)
	authed.Post("/teaching-courses/:id/makeup/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.AddMakeup)
	authed.Delete("/teaching-courses/:id/makeup/:sectionId/:makeupId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.DeleteMakeup)
	authed.Get("/teaching-courses/:id/holiday-impacts", th.HolidayImpacts)
	authed.Post("/teaching-courses/:id/holiday-impacts/:originalDate/remind", RequireRole(rbac.RoleTA), taApproved, th.RemindLecturerAboutMakeup)
	authed.Post("/teaching-courses/:id/review-date/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.AddReviewDate)
	authed.Post("/teaching-courses/import", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.ImportExcel)
	// Budget is management data (course pay-rate/cap usage): restrict to the
	// same roles that manage teaching courses. A per-course lecturer-ownership
	// check (only the course's own lecturer) belongs in the service/handler.
	authed.Get("/teaching-courses/:id/budget", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.Budget)

	// TA request windows
	rh := &TARequestHandler{Svc: svc}
	authed.Get("/ta-request/windows", rh.ListWindows)
	authed.Post("/ta-request/windows", adminOrStaff, rh.UpsertWindow)
	authed.Delete("/ta-request/windows/:id", adminOrStaff, rh.DeleteWindow)

	// TA requests
	authed.Get("/ta-requests", rh.List)
	authed.Get("/ta-requests/preview-conflicts", RequireRole(rbac.RoleLecturer), rh.PreviewConflicts)
	authed.Get("/ta-requests/candidates", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), rh.Candidates)
	authed.Get("/ta-requests/:id", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), rh.Detail)
	authed.Post("/ta-requests", RequireRole(rbac.RoleLecturer), rh.Create)

	// Docs (TA)
	dh := &DocsHandler{Svc: svc}
	authed.Get("/me/profile", RequireRole(rbac.RoleTA), dh.GetProfile)
	authed.Put("/me/profile", RequireRole(rbac.RoleTA), dh.UpsertProfile)
	authed.Get("/me/documents", RequireRole(rbac.RoleTA), dh.ListDocs)
	authed.Post("/me/documents", RequireRole(rbac.RoleTA), dh.UploadDoc)
	authed.Get("/me/history", RequireRole(rbac.RoleTA), dh.SelfHistory)
	authed.Get("/me/creditor-form.pdf", RequireRole(rbac.RoleTA, rbac.RoleAdmin, rbac.RoleStaff), dh.CreditorFormPDF)
	authed.Post("/me/creditor-form/confirm", RequireRole(rbac.RoleTA), dh.ConfirmCreditorForm)
	authed.Get("/creditor-form/blank.pdf", dh.BlankCreditorForm)
	authed.Get("/documents/:id/download", dh.Download)
	authed.Get("/ta-review", adminOrStaff, dh.ListPending)
	authed.Get("/ta-review/:userId/docs", adminOrStaff, dh.ListDocsForUser)
	authed.Get("/ta-review/:userId/history", adminOrStaff, dh.History)
	authed.Post("/ta-review/:userId/profile", adminOrStaff, dh.ReviewProfile)
	authed.Post("/ta-review/docs/:id", adminOrStaff, dh.ReviewDoc)
	// Redesigned officer review: batch approve/reject in the list row +
	// watermarked preview + one-shot ZIP download after approve.
	authed.Post("/ta-review/:userId/approve-all", adminOrStaff, dh.ApproveAll)
	authed.Post("/ta-review/:userId/reject-batch", adminOrStaff, dh.RejectBatch)
	authed.Post("/ta-review/:userId/zip-token", adminOrStaff, dh.MintZipToken)
	authed.Get("/ta-review/:userId/download.zip", adminOrStaff, dh.DownloadZip)
	authed.Get("/ta-review/:userId/docs/:docId/preview", adminOrStaff, dh.PreviewWatermarked)

	// TA: my assigned courses (year+term filter on FE).
	// Read-only endpoints are ungated so an un-approved TA can still browse
	// what their eventual dashboard will look like; the visible warning
	// banner tells them why nothing can be edited yet.
	authed.Get("/me/ta-courses", RequireRole(rbac.RoleTA), th.ListMyTACourses)
	authed.Get("/me/assignments", RequireRole(rbac.RoleTA), th.ListMyAssignments)

	// Workload / TA class schedule
	wh := &WorkloadHandler{Svc: svc}
	authed.Get("/me/schedule", RequireRole(rbac.RoleTA), wh.ListClasses)
	// PUT /me/schedule intentionally does NOT require an approved profile —
	// TAs can build their weekly class timetable while their documents are
	// still under review. Worklog submission stays gated below.
	authed.Put("/me/schedule", RequireRole(rbac.RoleTA), wh.ReplaceClasses)

	// Work logs — writes are gated, GET stays open so the TA can preview.
	wl := &WorkLogHandler{Svc: svc}
	authed.Post("/assignments/:id/worklog/generate", RequireRole(rbac.RoleTA), taApproved, wl.Generate)
	authed.Get("/assignments/:id/worklog", wl.List)
	authed.Put("/assignments/:id/worklog", RequireRole(rbac.RoleTA), taApproved, wl.Upsert)
	authed.Delete("/assignments/:id/worklog/:logId", RequireRole(rbac.RoleTA), taApproved, wl.Delete)
	authed.Post("/assignments/:id/worklog/submit", RequireRole(rbac.RoleTA), taApproved, wl.Submit)
	authed.Post("/assignments/:id/worklog/approve", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), wl.Approve)
	authed.Post("/assignments/:id/worklog/reject", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), wl.Reject)
	// TA-owned weekly review pattern — TA self-service, seeds review entries
	// on auto-generate. Approval gate mirrors the worklog write endpoints so
	// unapproved TAs can't seed a schedule that later blows past caps.
	authed.Get("/assignments/:id/review-schedules", RequireRole(rbac.RoleTA), wl.ListTAReviewSchedules)
	authed.Get("/assignments/:id/schedule-busy", RequireRole(rbac.RoleTA), wl.ScheduleBusy)
	authed.Post("/assignments/:id/review-schedules", RequireRole(rbac.RoleTA), taApproved, wl.AddTAReviewSchedule)
	authed.Patch("/assignments/:id/review-schedules/:rsId", RequireRole(rbac.RoleTA), taApproved, wl.UpdateTAReviewSchedule)
	authed.Delete("/assignments/:id/review-schedules/:rsId", RequireRole(rbac.RoleTA), taApproved, wl.DeleteTAReviewSchedule)
	// Lecturer's list of submitted work-logs awaiting review across their courses.
	authed.Get("/reports/pending", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), wl.PendingReports)
	// Per-course history of the lecturer's own approve/reject actions.
	authed.Get("/teaching-courses/:id/approval-history", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), wl.ApprovalHistory)

	// Staff worklog editor (Phase 5): read all TAs' entries per course, edit
	// or remove a row before export. Enforces same business rules as TA path.
	authed.Get("/staff/courses/:tcId/worklogs", adminOrStaff, wl.StaffListByCourse)
	authed.Get("/staff/courses/:tcId/assignments", adminOrStaff, wl.StaffListAssignments)
	authed.Put("/staff/worklogs", adminOrStaff, wl.StaffUpsert)
	authed.Delete("/staff/worklogs/:id", adminOrStaff, wl.StaffDelete)

	// Notifications
	nh := &NotifyHandler{Svc: svc}
	authed.Get("/me/notifications", nh.List)
	authed.Get("/me/notifications/unread-count", nh.UnreadCount)
	authed.Post("/me/notifications/:id/read", nh.MarkRead)
	authed.Post("/me/notifications/read-all", nh.MarkAllRead)

	// Announcements. Image endpoints sit under the same group so the auth
	// wall applies uniformly — a would-be scraper still needs a session.
	// Register the more specific image routes first: Fiber matches in
	// declaration order, so /:id would swallow "images/..." if placed above.
	ah := &AnnounceHandler{Svc: svc}
	authed.Post("/announcements/upload-image", adminOrStaff, ah.UploadImage)
	authed.Get("/announcements/images/*", ah.ServeImage)
	authed.Get("/announcements", ah.List)
	authed.Post("/announcements", adminOrStaff, ah.Upsert)
	authed.Get("/announcements/:id", ah.Get)
	authed.Delete("/announcements/:id", adminOrStaff, ah.Delete)
	authed.Post("/announcements/:id/publish", adminOrStaff, ah.Publish)
	authed.Post("/announcements/:id/unpublish", adminOrStaff, ah.Unpublish)

	// Dashboard
	dashH := &DashboardHandler{Svc: svc}
	authed.Get("/dashboard/executive", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), dashH.Executive)
	authed.Get("/dashboard/ta/me", RequireRole(rbac.RoleTA), dashH.TaOverview)
	authed.Get("/dashboard/lecturer/me", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), dashH.LecturerOverview)

	// Export
	eh := &ExportHandler{Svc: svc}
	authed.Get("/exports/course/:id.zip", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), eh.CourseZip)
	// Admin-only escape hatch to undo an accidental export lock.
	authed.Post("/exports/course/:id/unlock", RequireRole(rbac.RoleAdmin), eh.UnlockCourse)
	// Read-only payout preview — review the numbers before the locking download.
	authed.Get("/exports/course/:id/preview", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), eh.CoursePreview)
	// Phase 4 exports dashboard.
	authed.Get("/exports/summary", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), eh.CoursesSummary)
	authed.Get("/exports/course/:id/history", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), eh.CourseHistory)
	// Appointment order (คำสั่งแต่งตั้ง) — PDF + DOCX in one ZIP.
	authed.Post("/exports/appointment-order", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), eh.AppointmentOrder)

	// Physical-document progress board — the off-system signature/routing journey.
	// GET is readable by any authenticated user (shared status); staff/admin update.
	dpH := &DocProgressHandler{Svc: svc}
	authed.Get("/document-progress", dpH.Get)
	authed.Post("/document-progress/:termId", adminOrStaff, dpH.SetStage)
	// Per-course signature checklist (who hasn't signed). GET readable by anyone;
	// tick + remind are staff/admin only.
	authed.Get("/document-progress/checklist", dpH.ListChecklist)
	authed.Post("/document-progress/checklist/:tcId", adminOrStaff, dpH.ToggleSignature)
	authed.Post("/document-progress/:termId/remind", adminOrStaff, dpH.RemindUnsigned)

	// Admin officers (executive roster used on generated official docs).
	// GET is open to any authenticated user so document templates can render
	// with the current roster; writes are staff/admin only.
	aoH := &AdminOfficerHandler{Svc: svc}
	authed.Get("/settings/admin-officers", aoH.List)
	authed.Post("/settings/admin-officers", adminOrStaff, aoH.Upsert)
	authed.Delete("/settings/admin-officers/:id", adminOrStaff, aoH.Delete)

	// Monthly submission periods (Phase 3): staff CRUD + TA-facing reminders.
	spH := &SubmissionPeriodHandler{Svc: svc}
	authed.Get("/submission-periods", spH.List)
	authed.Post("/submission-periods", adminOrStaff, spH.Upsert)
	authed.Post("/submission-periods/bulk-for-term/:termId", adminOrStaff, spH.BulkForTerm)
	authed.Delete("/submission-periods/:id", adminOrStaff, spH.Delete)
	authed.Get("/me/submission-periods", spH.MePending)
	// No digital signatures: the lecturer's daily worklog approval is the
	// review; staff export (which locks the month) then mark it sent to finance.
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/finance-send",
		adminOrStaff, spH.FinanceSend)
	// "ตีกลับ" — staff/admin bounce an exported month back to pending (unlock).
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/send-back",
		adminOrStaff, spH.SendBack)
	// Admin-only unlock for a month already handed to finance.
	authed.Post("/submission-periods/:id/courses/:tcId/tas/:taId/finance-revert",
		RequireRole(rbac.RoleAdmin), spH.FinanceRevert)
	authed.Get("/submission-periods/:id/courses/:tcId/tas/:taId/timeline", spH.Timeline)
	authed.Get("/teaching-courses/:tcId/submission-timeline", spH.ListByCourse)

	// Public holidays — GET is open to any authenticated user so the TA and
	// lecturer holidays pages can render; writes are staff/admin only.
	hh := &HolidayHandler{Svc: svc}
	authed.Get("/holidays", hh.List)
	authed.Post("/holidays", adminOrStaff, hh.Create)
	authed.Post("/holidays/bulk", adminOrStaff, hh.BulkCreate)
	authed.Post("/holidays/sync-from-bot", adminOrStaff, hh.SyncFromBOT)
	authed.Patch("/holidays/:id", adminOrStaff, hh.Patch)
	authed.Delete("/holidays/:id", adminOrStaff, hh.Delete)

	// Audit log (admin)
	audH := &AuditHandler{Svc: svc}
	authed.Get("/audit-logs", RequireRole(rbac.RoleAdmin), audH.List)
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
