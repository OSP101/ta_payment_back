package service

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/storage"
)

// Container groups all domain services and shared deps.
type Container struct {
	Pool     *pgxpool.Pool
	Storage  storage.Store
	Mailer   *mail.Mailer
	Auditor  *audit.Auditor
	Cfg      config.Config

	Users      *UserService
	Courses    *CourseService
	Teaching   *TeachingService
	TARequest  *TARequestService
	Docs       *DocsService
	Budget     *BudgetService
	Workload   *WorkloadService
	WorkLog    *WorkLogService
	Export     *ExportService
	Announce   *AnnounceService
	Notify     *NotifyService
	Dashboard  *DashboardService
}

func NewContainer(pool *pgxpool.Pool, store storage.Store, mailer *mail.Mailer, auditor *audit.Auditor, cfg config.Config) *Container {
	c := &Container{Pool: pool, Storage: store, Mailer: mailer, Auditor: auditor, Cfg: cfg}
	c.Users = &UserService{pool: pool, aud: auditor}
	c.Courses = &CourseService{pool: pool, aud: auditor}
	c.Teaching = &TeachingService{pool: pool, aud: auditor}
	c.Budget = &BudgetService{pool: pool}
	c.Notify = &NotifyService{pool: pool, mailer: mailer}
	c.TARequest = &TARequestService{pool: pool, aud: auditor, budget: c.Budget, notify: c.Notify}
	c.Docs = &DocsService{pool: pool, aud: auditor, store: store}
	c.Workload = &WorkloadService{pool: pool}
	c.WorkLog = &WorkLogService{pool: pool, aud: auditor, budget: c.Budget}
	c.Export = &ExportService{pool: pool, store: store}
	c.Announce = &AnnounceService{pool: pool, aud: auditor, notify: c.Notify}
	c.Dashboard = &DashboardService{pool: pool}
	return c
}
