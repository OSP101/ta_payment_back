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
	// AppEnv gates production-only hardening (see main.go's ClamAV startup
	// check). Anything other than "production" is treated as a dev/staging
	// box where those checks would just get in the way.
	AppEnv string
	// TrustedProxyIPs are the IPs/CIDRs of infrastructure we control that sits
	// directly in front of this process (here: the Next.js server that proxies
	// /api/v1/* — see ta_payment_front/next.config.ts). Fiber only reads
	// X-Forwarded-For from a peer in this list; anyone else's copy of that
	// header is ignored. Leave empty (the default) and c.IP() falls back to the
	// TCP peer address for every caller, which — because ALL browser traffic
	// arrives via that same Next.js hop — means every login attempt looks like
	// it came from one IP and the per-IP rate limiter in router.go effectively
	// rate-limits the whole system, not each caller. Must be set in production.
	TrustedProxyIPs []string
	// ClamAVAddr is a clamd TCP address ("host:port"). Empty disables scanning
	// — see internal/service.scanUpload for what that means.
	ClamAVAddr    string
	ClamAVTimeout time.Duration
	SMTPHost      string
	SMTPPort      int
	SMTPUser      string
	SMTPPass      string
	MailFrom      string
	AppBaseURL    string
	// CookieSecure gates the auth cookie's Secure attribute. Derived from
	// AppBaseURL's scheme rather than the inbound request's protocol: this
	// process sits behind Next.js behind a TLS-terminating reverse proxy (see
	// TrustedProxyIPs), so per-request protocol detection would need that
	// whole trust chain configured correctly just to decide a cookie flag.
	// AppBaseURL is already required to be the real public URL for the SSO
	// redirect, so reusing it here can't drift from that URL and needs no
	// extra trust configuration.
	CookieSecure         bool
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
	// PIIEncKey is a 32-byte XChaCha20-Poly1305 key (base64) used by
	// internal/pii to encrypt the TA citizen ID (ta_profiles.citizen_id_enc).
	// Deliberately a SEPARATE secret from TADocsEncKey — one leaking must not
	// leak the other. Required, unlike TADocsEncKey: that key protects files
	// that predate encryption and so has an unencrypted fallback for
	// continuity; this one protects a field being stored for the first time
	// under an explicit "must be encrypted, no exceptions" requirement, so
	// there is no unencrypted mode to fall back to.
	PIIEncKey string
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
		AppEnv:               env("APP_ENV", "development"),
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
		PIIEncKey:            env("PII_ENC_KEY", ""),
		BotAPIBaseURL:        env("BOT_API_BASE_URL", "https://gateway.api.bot.or.th/financial-institutions-holidays"),
		BotAPIClientID:       env("BOT_API_CLIENT_ID", ""),
		ClamAVAddr:           env("CLAMAV_ADDR", ""),
	}
	c.SSOEnabled = c.SSOAuthURL != "" && c.SSOClientID != ""
	c.JWTLifetime = envDuration("JWT_LIFETIME", 12*time.Hour)
	c.SMTPPort = envInt("SMTP_PORT", 587)
	c.MaxUploadMB = envInt("MAX_UPLOAD_MB", 20)
	c.ClamAVTimeout = envDuration("CLAMAV_TIMEOUT", 30*time.Second)
	c.CookieSecure = strings.HasPrefix(c.AppBaseURL, "https://")
	c.TrustedProxyIPs = envList("TRUSTED_PROXY_IPS")
	if c.JWTSecret == "" {
		return c, fmt.Errorf("JWT_SECRET is required")
	}
	if c.PIIEncKey == "" {
		return c, fmt.Errorf("PII_ENC_KEY is required")
	}
	// A scanner deployment can't be assumed for every dev machine, but
	// production must never silently accept unscanned uploads — see
	// internal/service.scanUpload and docker-compose.yml's clamav service.
	if c.AppEnv == "production" && c.ClamAVAddr == "" {
		return c, fmt.Errorf("CLAMAV_ADDR is required when APP_ENV=production")
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

// envList reads a comma-separated env var into a trimmed, non-empty slice of
// strings. Returns nil when unset — callers treat a nil/empty list as "no
// trust configured" rather than defaulting to something.
func envList(k string) []string {
	v, ok := os.LookupEnv(k)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
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
