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
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/handler"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/scheduler"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
)

func main() {
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

	mailer := mail.New(cfg)
	auditor := audit.New(pool)
	tokens := auth.NewTokenService(cfg.JWTSecret, cfg.JWTIssuer)

	services := service.NewContainer(pool, store, mailer, auditor, cfg)

	app := fiber.New(fiber.Config{
		AppName:      "TA Payment API",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		BodyLimit:    32 * 1024 * 1024,
		ErrorHandler: handler.ErrorHandler,
	})
	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.CORSOrigins,
		AllowCredentials: true,
		AllowHeaders:     "Origin,Content-Type,Accept,Authorization",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
	}))

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

	// One-shot: auto-decide any legacy ta_requests still in 'submitted' state
	// after 0017 migration (officer manual approval flow is gone).
	if err := services.TARequest.RunPendingSweep(ctx); err != nil {
		log.Printf("ta_request sweep warning: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	_ = app.ShutdownWithTimeout(10 * time.Second)
}
