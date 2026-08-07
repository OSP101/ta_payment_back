package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// LoadDotEnv reads a KEY=VALUE file and populates os.Environ for keys not
// already set. Silently no-ops if the file doesn't exist. Exported so tools
// outside this package (internal/testutil, in particular) can point a test
// database connection at the same .env a developer already has, instead of
// keeping a second, easily-stale copy of the same credentials in Go source.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		// Strip optional quotes
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
	JWTIssuer   string
	JWTLifetime time.Duration
	CORSOrigins string
	UploadDir   string
	MaxUploadMB int
	// ClamAVAddr is a clamd TCP address ("host:port"). Empty disables scanning
	// — see internal/service.scanUpload for what that means.
	ClamAVAddr           string
	ClamAVTimeout        time.Duration
	SMTPHost             string
	SMTPPort             int
	SMTPUser             string
	SMTPPass             string
	MailFrom             string
	AppBaseURL           string
	SSOEnabled           bool
	SSOAuthURL           string
	SSOTokenURL          string
	SSOClientID          string
	SSOSecret            string
	SSORedirect          string
	CreditorTemplatePath string
	FontDir              string
	// TADocsEncKey is a 32-byte AES-256 key (base64) used to encrypt TA
	// documents at rest. Optional — when empty, files are stored unencrypted.
	TADocsEncKey string
	// BOT (Bank of Thailand) Open API — Financial Institutions' Holidays.
	// Used by the "ซิงก์จาก BOT" button on /staff/holidays to seed national
	// holidays. Empty ClientID → sync endpoint returns 503.
	BotAPIBaseURL  string
	BotAPIClientID string
}

func Load() (Config, error) {
	// Auto-load .env from cwd and from the binary's directory (in that order).
	// Never overrides existing environment variables.
	LoadDotEnv(".env")
	if exe, err := os.Executable(); err == nil {
		LoadDotEnv(exe + "/.env")
	}

	c := Config{
		Port:                 env("PORT", "8080"),
		DatabaseURL:          resolveDatabaseURL(),
		JWTSecret:            env("JWT_SECRET", ""),
		JWTIssuer:            env("JWT_ISSUER", "ta-payment"),
		CORSOrigins:          env("CORS_ORIGINS", "http://localhost:3000"),
		UploadDir:            env("UPLOAD_DIR", "./data/uploads"),
		SMTPHost:             env("SMTP_HOST", ""),
		SMTPUser:             env("SMTP_USER", ""),
		SMTPPass:             env("SMTP_PASS", ""),
		MailFrom:             env("MAIL_FROM", "no-reply@coco.kku.ac.th"),
		AppBaseURL:           env("APP_BASE_URL", "http://localhost:3000"),
		SSOAuthURL:           env("SSO_AUTH_URL", ""),
		SSOTokenURL:          env("SSO_TOKEN_URL", ""),
		SSOClientID:          env("SSO_CLIENT_ID", ""),
		SSOSecret:            env("SSO_CLIENT_SECRET", ""),
		SSORedirect:          env("SSO_REDIRECT", ""),
		CreditorTemplatePath: env("CREDITOR_TEMPLATE_PATH", "./assets/creditor_form_template.pdf"),
		FontDir:              env("FONT_DIR", "./assets/fonts"),
		TADocsEncKey:         env("TA_DOCS_ENC_KEY", ""),
		BotAPIBaseURL:        env("BOT_API_BASE_URL", "https://gateway.api.bot.or.th/financial-institutions-holidays"),
		BotAPIClientID:       env("BOT_API_CLIENT_ID", ""),
		ClamAVAddr:           env("CLAMAV_ADDR", ""),
	}
	c.SSOEnabled = c.SSOAuthURL != "" && c.SSOClientID != ""
	c.JWTLifetime = envDuration("JWT_LIFETIME", 12*time.Hour)
	c.SMTPPort = envInt("SMTP_PORT", 587)
	c.MaxUploadMB = envInt("MAX_UPLOAD_MB", 20)
	c.ClamAVTimeout = envDuration("CLAMAV_TIMEOUT", 30*time.Second)
	if c.JWTSecret == "" {
		return c, fmt.Errorf("JWT_SECRET is required")
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// resolveDatabaseURL returns DATABASE_URL if set; otherwise it composes one
// from DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/DB_NAME (docker-compose style).
// Falls back to a dev-only default.
func resolveDatabaseURL() string {
	if v, ok := os.LookupEnv("DATABASE_URL"); ok && v != "" {
		return v
	}
	host := env("DB_HOST", "")
	user := env("DB_USER", "")
	pass := env("DB_PASSWORD", "")
	if pass == "" {
		pass = env("DB_PASS", "")
	}
	name := env("DB_NAME", "")
	port := env("DB_PORT", "5432")
	if host != "" && user != "" && name != "" {
		return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
			user, pass, host, port, name)
	}
	return "postgres://tapay:tapay@localhost:5432/tapay?sslmode=disable"
}

func envDuration(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
