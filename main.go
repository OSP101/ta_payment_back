package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/helmet"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/api"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/handler"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/scheduler"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
)

func main() {
	// pdfcpu writes a config.yml under os.UserConfigDir() the first time any
	// call builds a default configuration, and it reports failure to create
	// that directory by PANICKING (model.NewDefaultConfiguration → fault.Fail),
	// not by returning an error. In the container the app runs as `nobody`,
	// whose home is /, so the write landed on /.config and every creditor-form
	// render died with "pdfcpu: config problem: mkdir /.config: permission
	// denied" — a stack trace rendered into the page where the PDF should be.
	//
	// Nothing here customises pdfcpu, so the config file has no purpose: this
	// tells pdfcpu to use its compiled-in defaults and never touch the disk.
	// The one documented side effect is that user fonts become unavailable,
	// which costs us nothing — the only pdfcpu text is the Helvetica watermark
	// in internal/watermark, a core font. Thai text is drawn by gopdf with its
	// own TTF registration and never goes through pdfcpu at all.
	//
	// Must run before the first NewDefaultConfiguration call. All three call
	// sites (pdfgen.normalizedTemplate, service.mergePDFs, watermark.applyPDF)
	// are inside request handlers, so main() is early enough.
	pdfcpu.DisableConfigDir()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	migrationDir := os.Getenv("MIGRATIONS_DIR")
	if migrationDir == "" {
		migrationDir = "./migrations"
	}
	if err := db.Migrate(ctx, pool, migrationDir); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	var encKey []byte
	if cfg.TADocsEncKey != "" {
		encKey, err = storage.ParseKeyFromBase64(cfg.TADocsEncKey)
		if err != nil {
			log.Fatalf("TA_DOCS_ENC_KEY: %v", err)
		}
	} else {
		log.Printf("WARN: TA_DOCS_ENC_KEY not set — uploaded TA documents will be stored unencrypted")
	}
	store, err := storage.NewLocalWithKey(cfg.UploadDir, encKey)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	// PII_ENC_KEY is validated as present by config.Load — parse it the same
	// way as TA_DOCS_ENC_KEY (32 bytes, hex or base64), but into its own
	// cipher. See internal/pii for why this is a separate key from encKey.
	piiKey, err := storage.ParseKeyFromBase64(cfg.PIIEncKey)
	if err != nil {
		log.Fatalf("PII_ENC_KEY: %v", err)
	}
	piiCipher, err := pii.New(piiKey)
	if err != nil {
		log.Fatalf("pii: %v", err)
	}

	mailer := mail.New(cfg)
	auditor := audit.New(pool)
	tokens := auth.NewTokenService(cfg.JWTSecret, cfg.JWTIssuer)

	services := service.NewContainer(pool, store, mailer, auditor, cfg, piiCipher)

	if cfg.AppEnv == "production" && len(cfg.TrustedProxyIPs) == 0 {
		// Not fatal — the app still runs correctly for cookie security (that's
		// derived from AppBaseURL, not this). But every caller's c.IP() will
		// collapse to the Next.js hop's address, silently defeating the per-IP
		// login rate limiter (router.go) and mislabeling every audit log IP.
		log.Printf("WARN: APP_ENV=production but TRUSTED_PROXY_IPS is empty — " +
			"c.IP() will return the Next.js proxy's address for every request, " +
			"not the real client. Set TRUSTED_PROXY_IPS to the Next.js server's " +
			"IP/CIDR to fix per-IP rate limiting and audit-log IPs.")
	}

	app := fiber.New(fiber.Config{
		AppName:      "TA Payment API",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		BodyLimit:    32 * 1024 * 1024,
		ErrorHandler: handler.ErrorHandler,
		// See config.TrustedProxyIPs: only a listed peer's X-Forwarded-For is
		// honoured, so c.IP() resolves to the real browser IP instead of the
		// Next.js hop's — everyone else's copy of the header is ignored.
		EnableTrustedProxyCheck: len(cfg.TrustedProxyIPs) > 0,
		TrustedProxies:          cfg.TrustedProxyIPs,
		ProxyHeader:             fiber.HeaderXForwardedFor,
	})
	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(helmet.New(helmet.Config{
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "SAMEORIGIN",
		ReferrerPolicy:     "strict-origin-when-cross-origin",
		// HSTS is deliberately NOT set here. This process is two hops behind
		// the browser (browser → TLS-terminating reverse proxy → Next.js →
		// here), so it never reliably knows whether the original connection
		// was HTTPS — see config.TrustedProxyIPs for the same problem applied
		// to the cookie Secure flag. Set HSTS at the reverse proxy (closest to
		// the browser) or in ta_payment_front/next.config.ts (see headers()).
		//
		// CrossOriginResourcePolicy is overridden from fiber's default
		// ("same-origin") to "cross-origin": same-origin is enforced by the
		// browser independently of CORS and would silently break the
		// documented cross-origin upload path (UPLOAD_ORIGIN in
		// ta_payment_front/app/lib/api.ts) whenever NEXT_PUBLIC_API_ORIGIN is
		// configured.
		//
		// Cross-Origin-Embedder-Policy/Opener-Policy are left at fiber's
		// defaults (require-corp / same-origin) rather than fought — the
		// helmet middleware has no "off" value for them, only "unset field
		// falls back to the default". Both only matter for a response the
		// browser treats as a navigable top-level document; nothing here
		// currently opens this API as one (no window.open()+postMessage flow
		// against it, no page embeds its JSON/PDF/ZIP responses as a
		// document), so they're inert today. Revisit if that changes.
		CrossOriginResourcePolicy: "cross-origin",
	}))
	app.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.CORSOrigins,
		AllowCredentials: true,
		AllowHeaders:     "Origin,Content-Type,Accept,Authorization",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
	}))
	// Second layer of CSRF defence, under the cookie's own SameSite=Lax — see
	// handler.OriginCheck's doc comment for why this reuses CORS_ORIGINS and
	// why it cannot reject requests with no Origin/Referer at all.
	app.Use(handler.OriginCheck(cfg.CORSOrigins))

	handler.Mount(app, services, tokens, rbac.New(pool), auditor)

	go func() {
		if err := app.Listen(":" + cfg.Port); err != nil {
			log.Fatalf("listen: %v", err)
		}
	}()
	log.Printf("TA Payment API listening on :%s", cfg.Port)

	// Background sweep for scheduled-but-not-yet-fanned-out announcements.
	// Lazy fanout on GET /announcements handles the common case; this loop
	// covers windows where no one is reading, so a scheduled 08:00 post
	// still lands in inboxes if the office opens at 08:15.
	go services.Announce.RunScheduler(ctx)

	// Retention sweep: delete on-disk blobs 7 days after approval and mark
	// file_deleted_at on the row for audit. See docs_review.go.
	go services.Docs.RunRetention(ctx)

	// Monthly submission-period scheduler: hourly reminders + daily auto-close.
	// Runs in-process; extract to cmd/scheduler if the reminder volume grows.
	scheduler.New(services).Start(ctx)

	// Finalise any TA request whose TAs have all filed their timetables while
	// the server was down. The fast path is the trigger on the TA's own save
	// (WorkloadService.ReplaceClasses); this catches what that missed. Requests
	// still waiting on someone are left untouched — 'submitted' is a valid
	// resting state under the deferred-decision model.
	if n, err := services.TARequest.SweepPendingRequests(ctx); err != nil {
		log.Printf("ta_request sweep warning: %v", err)
	} else if n > 0 {
		log.Printf("ta_request sweep: finalised %d pending request(s)", n)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	_ = app.ShutdownWithTimeout(10 * time.Second)
}
