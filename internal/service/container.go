package service

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"log"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/ssonext"
	"ta-payment-back/internal/storage"
)

// Container groups all domain services and shared deps.
type Container struct {
	Pool    *pgxpool.Pool
	Storage storage.Store
	Mailer  *mail.Mailer
	Auditor *audit.Auditor
	Cfg     config.Config
	// AV is the ONE antivirus scanner instance for the whole process — see
	// ScanUpload (upload_scan.go). Exported so it is shared, not one scanner
	// per service each independently deciding whether to bother calling it.
	AV antivirus.Scanner

	Sessions          *SessionService
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
	MFA               *MFAService
	SSO               *SSOService
	DataDeletion      *DataDeletionService
	Enrollments       *EnrollmentService
	TDBM              *TDBMService
	Audit             *AuditService
}

func NewContainer(pool *pgxpool.Pool, store storage.Store, mailer *mail.Mailer, auditor *audit.Auditor, cfg config.Config, piiCipher *pii.Cipher, totpCipher *pii.Cipher) *Container {
	c := &Container{Pool: pool, Storage: store, Mailer: mailer, Auditor: auditor, Cfg: cfg}
	c.Sessions = &SessionService{pool: pool}
	c.Audit = &AuditService{pool: pool, store: store}
	c.Users = &UserService{pool: pool, aud: auditor}
	c.Courses = &CourseService{pool: pool, aud: auditor}
	c.Notify = &NotifyService{pool: pool, mailer: mailer}
	c.Teaching = &TeachingService{pool: pool, aud: auditor, notify: c.Notify, fontDir: cfg.FontDir}
	c.Budget = &BudgetService{pool: pool}
	c.TARequest = &TARequestService{pool: pool, aud: auditor, budget: c.Budget, notify: c.Notify}
	// A nil scanner is not allowed: antivirus.New returns the Disabled no-op when
	// CLAMAV_ADDR is unset, so the upload path always has something to ask.
	av := antivirus.New(cfg.ClamAVAddr, cfg.ClamAVTimeout)
	if !av.Enabled() {
		log.Printf("WARNING: CLAMAV_ADDR is not set — uploaded documents are NOT virus-scanned")
	}
	c.AV = av
	c.Docs = &DocsService{pool: pool, aud: auditor, store: store, av: av, pii: piiCipher, notify: c.Notify}
	// Workload holds a back-reference to TARequest so saving a TA timetable can
	// finalise the requests that were waiting for it (deferred-decision model).
	c.Workload = &WorkloadService{pool: pool, aud: auditor, requests: c.TARequest}
	c.WorkLog = &WorkLogService{pool: pool, aud: auditor, budget: c.Budget, notify: c.Notify}
	c.Export = &ExportService{pool: pool, aud: auditor, notify: c.Notify, store: store, budget: c.Budget, teaching: c.Teaching, users: c.Users, docs: c.Docs}
	// Back-reference, set after both exist: approving is what moves the budget,
	// and the settlement that decides the shortfall lives on Export.
	c.WorkLog.export = c.Export
	c.Announce = &AnnounceService{pool: pool, aud: auditor, notify: c.Notify}
	c.Dashboard = &DashboardService{pool: pool}
	c.AdminOfficers = &AdminOfficerService{pool: pool, aud: auditor}
	c.SubmissionPeriods = &SubmissionPeriodService{pool: pool, aud: auditor, notify: c.Notify}
	c.ExportBatches = &ExportBatchService{pool: pool, aud: auditor}
	c.DocProgress = &DocumentProgressService{pool: pool, aud: auditor, notify: c.Notify, export: c.Export}
	c.Appointment = &AppointmentOrderService{pool: pool, aud: auditor, fontDir: cfg.FontDir}
	c.Holiday = &HolidayService{pool: pool, aud: auditor, notify: c.Notify}
	c.MFA = &MFAService{pool: pool, aud: auditor, totp: totpCipher}
	c.SSO = &SSOService{users: c.Users, aud: auditor}
	if cfg.SSOEnabled {
		c.SSO.client = &ssonext.Client{
			LoginBase: cfg.SSOLoginBase, APIBase: cfg.SSOAPIBase,
			AppID: cfg.SSOAppID, ClientID: cfg.SSOClientID, ClientSecret: cfg.SSOSecret,
			RedirectURL: cfg.SSORedirect,
		}
	}
	c.DataDeletion = &DataDeletionService{
		pool: pool, aud: auditor, docs: c.Docs, users: c.Users,
		sessions: c.Sessions, notify: c.Notify, store: store,
	}
	c.Enrollments = &EnrollmentService{pool: pool, aud: auditor}
	c.TDBM = &TDBMService{pool: pool, aud: auditor, apiBase: cfg.TDBMAPIBaseURL}
	// Back-reference, set after both exist (same pattern as WorkLog.export
	// above): lets a course/section write trigger an immediate re-match
	// instead of waiting for the next TDBM sync — see TeachingService.tdbm's
	// doc comment.
	c.Teaching.tdbm = c.TDBM
	return c
}
