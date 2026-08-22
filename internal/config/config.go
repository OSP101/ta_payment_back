package config

import (
	"bufio"
	"fmt"
	"net/url"
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
	// TOTPEncKey is a 32-byte XChaCha20-Poly1305 key (base64) used by
	// internal/pii to encrypt TOTP secrets (users.totp_secret_enc /
	// totp_pending_secret_enc). Deliberately a THIRD key, separate from both
	// PIIEncKey and TADocsEncKey — see internal/pii's own "one leaking must
	// not leak the other" doc comment, which applies here just as much as it
	// does to the citizen ID. Required, same reasoning as PIIEncKey: 2FA is
	// mandatory for admin/staff, so there is no unencrypted fallback mode an
	// unset key could safely degrade into — the app would simply be unusable
	// for those roles.
	TOTPEncKey string
	// MFAMandatoryEnforced gates the "admin/staff/executive MUST have 2FA
	// enrolled" branch in AccountGuard — see mfaMandatoryFor. Defaults to
	// true (production posture). Set MFA_MANDATORY_ENFORCED=false as a
	// temporary operational escape hatch — e.g. rolling this feature out
	// before anyone has enrolled a real authenticator app yet, or a support
	// incident where enforcement itself is what's blocking someone. This
	// does NOT touch 2FA for accounts that already opted in voluntarily
	// (lecturer/TA): it only stops the mandatory tier from being BLOCKED for
	// not having enrolled — anyone who already has TOTP enabled still gets
	// challenged at login regardless of this flag. Flip back to true (or
	// unset it) once real enrolment is ready to be required again.
	MFAMandatoryEnforced bool
	// BOT (Bank of Thailand) Open API — Financial Institutions' Holidays.
	// Used by the "ซิงก์จาก BOT" button on /staff/holidays to seed national
	// holidays. Empty ClientID → sync endpoint returns 503.
	BotAPIBaseURL  string
	BotAPIClientID string

	// TDBM (tdbm.computing.kku.ac.th) — the college's own teaching-compensation
	// system. Source of truth for holidays and lecturer-filed makeup ("สอนชดเชย")
	// submissions; see docs/TDBM-API-requirements.md. Public read API, no key.
	TDBMAPIBaseURL string
	// TDBMWebhookSecret authenticates inbound POST /tdbm-webhook calls (TDBM →
	// us) via the X-TDBM-Webhook-Secret header — see VerifyTDBMWebhookSecret.
	// Empty means the webhook is not configured yet: same "closed by default"
	// posture as ClamAVAddr/BotAPIClientID, so an unset secret refuses every
	// call (503) instead of silently accepting unauthenticated triggers.
	TDBMWebhookSecret string

	// DemoMode gates the BETA "โหมดทดลอง" sandbox (internal/demo) entirely —
	// false means main.go never provisions a slot, mounts a demo route, or
	// touches demo_workspaces beyond what migration 0086 itself created.
	// See docs/PLAN-demo-sandbox.md.
	DemoMode bool
	// DemoMaxWorkspaces bounds how many officers can have a sandbox claimed
	// at once. Each slot gets its own migrated schema and pgxpool at boot
	// (internal/demo.Bootstrap), so this is a real resource cost, not just a
	// counter — keep it to the handful of people actually testing at a time.
	DemoMaxWorkspaces int
	// DemoJWTSecret signs demo-session tokens. Deliberately a DIFFERENT key
	// from JWTSecret: a demo token must never parse as valid on the real
	// Authenticated/AccountGuard path (or vice versa), even though the two
	// live in the same process and share a signing algorithm. When unset,
	// derived from JWTSecret so flipping DEMO_MODE=true needs no new secret
	// provisioned — see the WARN this logs, same pattern as TADocsEncKey.
	DemoJWTSecret string
	// DemoIdleReclaimDays bounds how long a claimed slot sits untouched
	// before Manager.Claim is willing to hand it to someone new when every
	// slot is full — see that function's doc comment. Keeps a fixed-size
	// pool (DemoMaxWorkspaces) workable for a feature meant to onboard
	// people indefinitely: without this, the 9th person ever to try the
	// sandbox would find it permanently full, since nothing else ever frees
	// a slot a departed tester never explicitly gave up.
	DemoIdleReclaimDays int
	// DemoDatabaseURL is a SEPARATE Postgres database from DatabaseURL — the
	// demo sandbox stopped being removable BETA scaffolding (see
	// docs/PLAN-demo-sandbox.md) and became a permanent onboarding feature,
	// so it earns the same "must never touch production's own database" bar
	// as everything else that isn't production. Schema+search_path isolation
	// (demo_slot_N) already kept demo DATA apart; this keeps it apart at the
	// database level too — a runaway demo query or connection-pool exhaustion
	// can no longer touch production's actual disk/IO/backups, and dropping
	// this whole database is a clean uninstall with zero risk to real data.
	// When unset, derived from DatabaseURL (append "_demo" to the database
	// name) — same "DEMO_MODE=true needs no new required config" reasoning
	// as DemoJWTSecret above. internal/db.EnsureDatabase creates it on boot
	// if it doesn't exist yet, so a fresh environment self-provisions.
	DemoDatabaseURL string
	// IsDemoSlot marks a Config as belonging to one specific demo workspace
	// pool (internal/demo.Bootstrap builds one such Config PER SLOT, copied
	// from the production Config and then mutated). Deliberately NOT an env
	// var and NOT the same thing as DemoMode: DemoMode is true on the
	// PRODUCTION Config too whenever the sandbox feature is merely enabled
	// (main.go builds one real service.Container with DemoMode=true right
	// alongside the demo slots), so branching mandatory-2FA enforcement on
	// DemoMode would silently disable it for every real admin/staff account
	// the moment DEMO_MODE=true. IsDemoSlot is the only field that is true
	// ONLY on a demo slot's own Config — see internal/demo/manager.go where
	// it is set on the per-slot copy, never on the value main.go loads.
	IsDemoSlot bool
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
		TOTPEncKey:           env("TOTP_ENC_KEY", ""),
		BotAPIBaseURL:        env("BOT_API_BASE_URL", "https://gateway.api.bot.or.th/financial-institutions-holidays"),
		BotAPIClientID:       env("BOT_API_CLIENT_ID", ""),
		TDBMAPIBaseURL:       env("TDBM_API_BASE_URL", "https://tdbm.computing.kku.ac.th/api"),
		TDBMWebhookSecret:    env("TDBM_WEBHOOK_SECRET", ""),
		ClamAVAddr:           env("CLAMAV_ADDR", ""),
		DemoJWTSecret:        env("DEMO_JWT_SECRET", ""),
	}
	c.MFAMandatoryEnforced = envBool("MFA_MANDATORY_ENFORCED", true)
	c.DemoMode = envBool("DEMO_MODE", false)
	c.DemoMaxWorkspaces = envInt("DEMO_MAX_WORKSPACES", 8)
	c.DemoIdleReclaimDays = envInt("DEMO_IDLE_RECLAIM_DAYS", 14)
	if c.DemoJWTSecret == "" {
		// See the field's doc comment: derived so DEMO_MODE=true works with
		// zero extra provisioning, while still never colliding with a token
		// signed by JWTSecret itself.
		c.DemoJWTSecret = c.JWTSecret + "|demo-sandbox"
	}
	c.DemoDatabaseURL = env("DEMO_DATABASE_URL", "")
	if c.DemoDatabaseURL == "" {
		// See the field's doc comment: derived so DEMO_MODE=true needs no new
		// required config, while still landing on a database distinct from
		// DatabaseURL.
		c.DemoDatabaseURL = deriveDemoDatabaseURL(c.DatabaseURL)
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
	if c.TOTPEncKey == "" {
		return c, fmt.Errorf("TOTP_ENC_KEY is required")
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

func envBool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
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

// deriveDemoDatabaseURL appends "_demo" to dbURL's database name — the
// DemoDatabaseURL fallback when DEMO_DATABASE_URL isn't set. Falls back to
// dbURL unchanged if it doesn't parse as a URL at all (a malformed
// DatabaseURL is main.go's Connect call's problem to fail loudly on, not
// this function's to guess around).
func deriveDemoDatabaseURL(dbURL string) string {
	u, err := url.Parse(dbURL)
	if err != nil {
		return dbURL
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return dbURL
	}
	u.Path = "/" + name + "_demo"
	return u.String()
}

func envDuration(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
