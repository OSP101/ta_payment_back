package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/ssonext"
)

// SSOService is the KKU SSONext login path. It is deliberately two steps on
// our side even though SSONext itself is one:
//
//  1. Exchange — redeem the callback code with KKU, match the returned email
//     to one of OUR accounts, and hand back a short-lived ticket plus the
//     identity to show. No session yet.
//  2. Redeem — the user has looked at "เข้าสู่ระบบด้วยบัญชี x@kkumail.com"
//     and clicked; turn the ticket into the user id the handler then treats
//     exactly like a password-verified login (TOTP branch and all).
//
// The pause between the two is not decoration. SSONext has no `state`
// parameter, so a callback URL with a code in it is a bearer credential
// anyone can hand to anyone: without the confirm click, a victim opening an
// attacker-crafted callback link would be silently logged in as the
// attacker's account (login CSRF) and go on to enter their own bank details
// into it. The confirm screen shows whose account is about to be entered,
// and nothing happens until the person in front of the browser agrees.
//
// Accounts are matched on email only and never created here: this is a
// payment system, and holding a KKU account proves affiliation, not that
// the college has engaged this person as a TA or that staff have set them
// up. Someone unknown gets ErrSSONoAccount and the email we saw, so they
// can tell staff which address to register.
type SSOService struct {
	client *ssonext.Client
	users  *UserService
	aud    *audit.Auditor

	mu      sync.Mutex
	pending map[string]ssoPending
}

// ssoTicketTTL matches mfaChallengeTTL — the confirm page is meant to be
// looked at and clicked, not left open.
const ssoTicketTTL = 5 * time.Minute

// Tickets live in process memory rather than a table, same as login_gate.go:
// they are five-minute, single-use, and losing them on a restart costs the
// user one extra click on "เข้าสู่ระบบด้วย KKU". The trade-off to remember
// is that this pins the deploy to one backend replica for the SSO path.
type ssoPending struct {
	userID  uuid.UUID
	expires time.Time
}

// SSOPending is what the confirm screen renders. Email is the KKU-side
// address the login was matched on; the names are OUR record's, not KKU's,
// so the person sees the account they are entering, not what KKU calls them.
type SSOPending struct {
	Ticket    string `json:"ticket"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

var (
	// ErrSSORejected: KKU would not redeem the code (stale, reused, or a
	// credential mismatch on our side). One message for all of them.
	ErrSSORejected = errors.New("sso: code rejected")
	// ErrSSOTicketInvalid: unknown, expired or already-used confirm ticket.
	ErrSSOTicketInvalid = errors.New("sso: ticket invalid")
)

// SSONoAccountError carries the KKU email so the handler can show it — the
// address is the user's own, and it is exactly what they need to tell staff.
type SSONoAccountError struct {
	Email string
	// Inactive: an account with this address exists but is switched off.
	Inactive bool
}

func (e *SSONoAccountError) Error() string { return "sso: no account for " + e.Email }

// Enabled reports whether a client was configured at all — the handler uses
// it to keep the routes as 404-equivalents when SSO is off.
func (s *SSOService) Enabled() bool { return s != nil && s.client != nil }

func (s *SSOService) LoginURL() string  { return s.client.LoginURL() }
func (s *SSOService) LogoutURL() string { return s.client.LogoutURL() }

// Exchange is step 1: code → KKU identity → our account → ticket.
func (s *SSOService) Exchange(ctx context.Context, code, ip, userAgent string) (*SSOPending, error) {
	id, err := s.client.Exchange(ctx, code)
	if err != nil {
		if errors.Is(err, ssonext.ErrRejected) {
			_ = s.aud.Log(ctx, audit.Entry{
				Action: "auth.sso_rejected", Entity: "user",
				IP: ip, UserAgent: userAgent, Note: err.Error(),
			})
			return nil, ErrSSORejected
		}
		return nil, err
	}
	u, _, err := s.users.FindByEmail(ctx, id.Email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Same shape as Authenticate's unknown-account entry: the
			// domain in clear plus a hash of the full address, never the
			// address itself in an append-only table.
			_ = s.aud.Log(ctx, audit.Entry{
				Action: "auth.sso_unknown_account", Entity: "user",
				IP: ip, UserAgent: userAgent, Note: attemptedIdentifier(id.Email),
			})
			// KKU has just proven this person owns the address, so telling
			// them their account exists but is switched off leaks nothing —
			// and "not registered" sent them to ask staff for a NEW account.
			var inactive bool
			_ = s.users.pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM users WHERE email = $1 AND NOT is_active AND deleted_at IS NULL)`,
				id.Email).Scan(&inactive)
			return nil, &SSONoAccountError{Email: id.Email, Inactive: inactive}
		}
		return nil, err
	}
	// The per-account password lockout deliberately does NOT apply here. It
	// exists to stop guessing OUR password; a KKU SSO ticket proves identity
	// through the university instead, so there is nothing to guess. Applying
	// it let anyone who knew a staff email keep that person out entirely by
	// sending 7 wrong passwords every 15 minutes — SSO included.

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	s.sweepLocked(time.Now())
	if s.pending == nil {
		s.pending = map[string]ssoPending{}
	}
	s.pending[ticket] = ssoPending{userID: u.ID, expires: time.Now().Add(ssoTicketTTL)}
	s.mu.Unlock()

	_ = s.aud.Log(ctx, audit.Entry{
		ActorID: &u.ID, Action: "auth.sso_matched", Entity: "user",
		EntityID: u.ID.String(), IP: ip, UserAgent: userAgent,
	})
	return &SSOPending{Ticket: ticket, Email: id.Email, FirstName: u.FirstName, LastName: u.LastName}, nil
}

// Redeem is step 2: the confirm click. Single use — the ticket is deleted
// whether or not what follows (TOTP, session mint) succeeds, so a ticket can
// never be replayed to mint a second session.
func (s *SSOService) Redeem(ctx context.Context, ticket, ip, userAgent string) (*User, error) {
	ticket = strings.TrimSpace(ticket)
	s.mu.Lock()
	p, ok := s.pending[ticket]
	delete(s.pending, ticket)
	s.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		return nil, ErrSSOTicketInvalid
	}
	// Re-read rather than trust the five-minute-old match: the account
	// could have been deactivated in between.
	u, err := s.users.Get(ctx, p.userID)
	if err != nil {
		return nil, err
	}
	if !u.IsActive {
		return nil, ErrSSOTicketInvalid
	}
	loginGateSucceed(u.ID)
	_ = s.aud.Log(ctx, audit.Entry{
		ActorID: &u.ID, Action: "auth.login", Entity: "user",
		EntityID: u.ID.String(), IP: ip, UserAgent: userAgent, Note: "sso",
	})
	// A pending "set a new password" belongs to a temporary password someone
	// else (the lecturer who created the account, or staff after a reset) has
	// seen. The KKU login just proved who this is, so that temporary password
	// is retired instead of forcing a KKU user to invent a local one: it can
	// no longer be used by whoever saw it, and the account continues on SSO.
	if u.MustChangePassword {
		if _, err := s.users.pool.Exec(ctx,
			`UPDATE users SET password_hash = NULL, must_change_password = FALSE, updated_at = NOW() WHERE id = $1`,
			u.ID); err != nil {
			return nil, err
		}
		_ = s.aud.Log(ctx, audit.Entry{
			ActorID: &u.ID, Action: "user.temp_password_retired", Entity: "user",
			EntityID: u.ID.String(), IP: ip, UserAgent: userAgent, Note: "sso login",
		})
		u.MustChangePassword = false
	}
	return u, nil
}

func (s *SSOService) sweepLocked(now time.Time) {
	for k, p := range s.pending {
		if now.After(p.expires) {
			delete(s.pending, k)
		}
	}
}
