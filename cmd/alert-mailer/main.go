package main

import (
	"context"
	"log/slog"
	"net/http"
	netmail "net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"ta-payment-back/internal/alertmail"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/telemetry"
)

func main() {
	slog.SetDefault(slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(os.Stdout, nil))).With("service", "ta-payment-alert-mailer"))
	slog.SetLogLoggerLevel(slog.LevelInfo)
	fail := func(message string) { slog.Error(message); os.Exit(1) }
	token, err := os.ReadFile("/run/secrets/alert-webhook-token")
	if err != nil || len(strings.TrimSpace(string(token))) < 32 {
		fail("missing notification authentication token")
	}
	port, err := strconv.Atoi(os.Getenv("SMTP_PORT"))
	if err != nil || port < 1 || port > 65535 {
		fail("invalid SMTP port")
	}
	from := os.Getenv("MAILER_FROM")
	if _, err := netmail.ParseAddress(from); err != nil {
		fail("invalid notification sender")
	}
	recipients := strings.Split(os.Getenv("ALERT_EMAIL_TO"), ",")
	for i, recipient := range recipients {
		address, err := netmail.ParseAddress(strings.TrimSpace(recipient))
		if err != nil {
			fail("invalid notification recipient")
		}
		recipients[i] = address.Address
	}
	if os.Getenv("SMTP_HOST") == "" {
		fail("SMTP host is required")
	}
	legacy, _ := strconv.ParseBool(os.Getenv("SMTP_TLS_LEGACY_CIPHERS"))
	cfg := config.Config{SMTPHost: os.Getenv("SMTP_HOST"), SMTPPort: port, SMTPUser: os.Getenv("SMTP_USER"), SMTPPass: os.Getenv("SMTP_PASS"), SMTPEncryption: os.Getenv("SMTP_ENCRYPTION"), SMTPTLSLegacyCiphers: legacy, MailFrom: from, MailFromName: "TA Payment Monitoring"}
	switch cfg.SMTPEncryption {
	case "tls", "true", "starttls":
		cfg.SMTPEncryption = "starttls"
	case "ssl":
	case "none", "false":
		cfg.SMTPEncryption = "none"
	case "", "auto":
		cfg.SMTPEncryption = "auto"
	default:
		fail("invalid SMTP encryption mode")
	}
	if cfg.SMTPEncryption == "auto" && port == 465 {
		cfg.SMTPEncryption = "ssl"
	}
	mux := http.NewServeMux()
	mux.Handle("/alerts", alertmail.New(mail.New(cfg), strings.TrimSpace(string(token)), recipients))
	mux.Handle("/metrics", promhttp.HandlerFor(telemetry.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	server := &http.Server{Addr: ":9095", Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		defer close(shutdownDone)
		shutdown, cancel := context.WithTimeout(context.Background(), 65*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("alert_mailer_started")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fail("alert mailer listener failed")
	}
	<-shutdownDone
}
