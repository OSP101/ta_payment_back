package handler

import (
	"github.com/gofiber/fiber/v2"

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
	api.Post("/auth/login", authH.Login)
	api.Post("/auth/sso/callback", authH.SSOCallback) // stub
	api.Get("/auth/sso/url", authH.SSOURL)
	api.Post("/auth/logout", authH.Logout)

	// Authenticated
	authed := api.Group("", Authenticated(tokens))
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

	// Faculty courses & settings (admin, staff)
	ch := &CourseHandler{Svc: svc}
	authed.Get("/faculty-courses", ch.List)
	authed.Post("/faculty-courses", adminOrStaff, ch.Upsert)
	authed.Delete("/faculty-courses/:id", adminOrStaff, ch.Delete)
	authed.Get("/settings/pay-rate", ch.PayRate)
	authed.Post("/settings/pay-rate", adminOrStaff, ch.CreatePayRate)
	authed.Get("/settings/hour-caps", ch.HourCaps)
	authed.Post("/settings/hour-caps", adminOrStaff, ch.UpsertHourCap)
	authed.Delete("/settings/hour-caps/:credits", adminOrStaff, ch.DeleteHourCap)
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
	authed.Post("/teaching-courses", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.Create)
	authed.Get("/teaching-courses/:id", th.Get)
	authed.Patch("/teaching-courses/:id/num-students", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.SetNumStudents)
	authed.Patch("/teaching-courses/:id/settings", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.UpdateSettings)
	authed.Post("/teaching-courses/:id/sections", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.AddSection)
	authed.Patch("/teaching-courses/:id/sections/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.UpdateSection)
	authed.Delete("/teaching-courses/:id/sections/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.DeleteSection)
	authed.Put("/teaching-courses/:id/sections/:sectionId/schedules", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.ReplaceSectionSchedules)
	authed.Post("/teaching-courses/:id/makeup/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.AddMakeup)
	authed.Post("/teaching-courses/:id/review-date/:sectionId", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.AddReviewDate)
	authed.Post("/teaching-courses/import", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), th.ImportExcel)
	authed.Get("/teaching-courses/:id/budget", th.Budget)

	// TA request windows
	rh := &TARequestHandler{Svc: svc}
	authed.Get("/ta-request/windows", rh.ListWindows)
	authed.Post("/ta-request/windows", adminOrStaff, rh.UpsertWindow)
	authed.Delete("/ta-request/windows/:id", adminOrStaff, rh.DeleteWindow)

	// TA requests
	authed.Get("/ta-requests", rh.List)
	authed.Get("/ta-requests/:id", RequireRole(rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer), rh.Detail)
	authed.Post("/ta-requests", RequireRole(rbac.RoleLecturer), rh.Create)
	authed.Post("/ta-requests/:id/approve", adminOrStaff, rh.Approve)
	authed.Post("/ta-requests/:id/reject", adminOrStaff, rh.Reject)

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

	// TA: my assigned courses (year+term filter on FE).
	// Read-only endpoints are ungated so an un-approved TA can still browse
	// what their eventual dashboard will look like; the visible warning
	// banner tells them why nothing can be edited yet.
	authed.Get("/me/ta-courses", RequireRole(rbac.RoleTA), th.ListMyTACourses)

	// Workload / TA class schedule
	wh := &WorkloadHandler{Svc: svc}
	authed.Get("/me/schedule", RequireRole(rbac.RoleTA), wh.ListClasses)
	authed.Put("/me/schedule", RequireRole(rbac.RoleTA), taApproved, wh.ReplaceClasses)

	// Work logs — writes are gated, GET stays open so the TA can preview.
	wl := &WorkLogHandler{Svc: svc}
	authed.Post("/assignments/:id/worklog/generate", RequireRole(rbac.RoleTA), taApproved, wl.Generate)
	authed.Get("/assignments/:id/worklog", wl.List)
	authed.Put("/assignments/:id/worklog", RequireRole(rbac.RoleTA), taApproved, wl.Upsert)
	authed.Post("/assignments/:id/worklog/submit", RequireRole(rbac.RoleTA), taApproved, wl.Submit)
	authed.Post("/assignments/:id/worklog/approve", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), wl.Approve)
	authed.Post("/assignments/:id/worklog/reject", RequireRole(rbac.RoleLecturer, rbac.RoleAdmin, rbac.RoleStaff), wl.Reject)

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

	// Export
	eh := &ExportHandler{Svc: svc}
	authed.Get("/exports/course/:id.zip", RequireRole(rbac.RoleAdmin, rbac.RoleStaff), eh.CourseZip)

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
