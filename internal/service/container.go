package service

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"log"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/storage"
)

// Container groups all domain services and shared deps.
type Container struct {
	Pool    *pgxpool.Pool
	Storage storage.Store
	Mailer  *mail.Mailer
	Auditor *audit.Auditor
	Cfg     config.Config

	Appointment       *AppointmentOrderService
	Users             *UserService
	Courses           *CourseService
	Teaching          *TeachingService
	TARequest         *TARequestService
	Docs              *DocsService
	Budget            *BudgetService
	Workload          *WorkloadService
	WorkLog           *WorkLogService
	Export            *ExportService
	Announce          *AnnounceService
	Notify            *NotifyService
	Dashboard         *DashboardService
	AdminOfficers     *AdminOfficerService
	SubmissionPeriods *SubmissionPeriodService
	ExportBatches     *ExportBatchService
	Holiday           *HolidayService
	DocProgress       *DocumentProgressService
}

func NewContainer(pool *pgxpool.Pool, store storage.Store, mailer *mail.Mailer, auditor *audit.Auditor, cfg config.Config) *Container {
	c := &Container{Pool: pool, Storage: store, Mailer: mailer, Auditor: auditor, Cfg: cfg}
	c.Users = &UserService{pool: pool, aud: auditor}
	c.Courses = &CourseService{pool: pool, aud: auditor}
	c.Teaching = &TeachingService{pool: pool, aud: auditor, notify: c.Notify, fontDir: cfg.FontDir}
	c.Budget = &BudgetService{pool: pool}
	c.Notify = &NotifyService{pool: pool, mailer: mailer}
	c.TARequest = &TARequestService{pool: pool, aud: auditor, budget: c.Budget, notify: c.Notify}
	// A nil scanner is not allowed: antivirus.New returns the Disabled no-op when
	// CLAMAV_ADDR is unset, so the upload path always has something to ask.
	av := antivirus.New(cfg.ClamAVAddr, cfg.ClamAVTimeout)
	if !av.Enabled() {
		log.Printf("WARNING: CLAMAV_ADDR is not set — uploaded documents are NOT virus-scanned")
	}
	c.Docs = &DocsService{pool: pool, aud: auditor, store: store, av: av}
	// Workload holds a back-reference to TARequest so saving a TA timetable can
	// finalise the requests that were waiting for it (deferred-decision model).
	c.Workload = &WorkloadService{pool: pool, requests: c.TARequest}
	c.WorkLog = &WorkLogService{pool: pool, aud: auditor, budget: c.Budget, notify: c.Notify}
	c.Export = &ExportService{pool: pool, store: store, budget: c.Budget, fontDir: cfg.FontDir, teaching: c.Teaching}
	c.Announce = &AnnounceService{pool: pool, aud: auditor, notify: c.Notify}
	c.Dashboard = &DashboardService{pool: pool}
	c.AdminOfficers = &AdminOfficerService{pool: pool, aud: auditor}
	c.SubmissionPeriods = &SubmissionPeriodService{pool: pool, aud: auditor, notify: c.Notify}
	c.ExportBatches = &ExportBatchService{pool: pool, aud: auditor}
	c.DocProgress = &DocumentProgressService{pool: pool, aud: auditor, notify: c.Notify}
	c.Appointment = &AppointmentOrderService{pool: pool, aud: auditor, fontDir: cfg.FontDir}
	c.Holiday = &HolidayService{
		pool: pool, aud: auditor, notify: c.Notify,
		botAPIBaseURL:  cfg.BotAPIBaseURL,
		botAPIClientID: cfg.BotAPIClientID,
	}
	return c
}
